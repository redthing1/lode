package transfer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type boundedInput struct {
	reader *safeReader
	limit  int64
	count  int64
}

func (r *boundedInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.count >= r.limit {
		var extra [1]byte
		n, err := readProgress(r.reader, extra[:])
		if n > 0 {
			return 0, ErrLimitExceeded
		}
		return 0, err
	}
	remaining := r.limit - r.count
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

type patchReader struct {
	ctx        context.Context
	basis      *safeReadSeeker
	basisSize  int64
	targetSize int64
	encoded    *boundedInput
	produced   int64
	commands   int
	literal    int64
	copyLeft   int64
	ended      bool
	terminal   error
}

func (r *patchReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.terminal != nil {
		return 0, r.terminal
	}
	if r.ended {
		return 0, io.EOF
	}
	written := 0
	for written < len(p) {
		if err := r.ctx.Err(); err != nil {
			return r.fail(written, err)
		}
		if r.literal > 0 {
			n, err := r.readLiteral(p[written:])
			written += n
			if err != nil {
				return r.fail(written, err)
			}
			continue
		}
		if r.copyLeft > 0 {
			n, err := r.readCopy(p[written:])
			written += n
			if err != nil {
				return r.fail(written, err)
			}
			continue
		}
		end, err := r.nextCommand()
		if err != nil {
			return r.fail(written, err)
		}
		if end {
			r.ended = true
			if written > 0 {
				return written, nil
			}
			return 0, io.EOF
		}
	}
	return written, nil
}

func (r *patchReader) fail(written int, err error) (int, error) {
	if !errors.Is(err, ErrMalformedDelta) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		err = malformed("decode delta", err)
	}
	r.terminal = err
	return written, err
}

func (r *patchReader) readLiteral(p []byte) (int, error) {
	want := int64(len(p))
	if want > r.literal {
		want = r.literal
	}
	if want > r.targetSize-r.produced {
		return 0, malformed("literal exceeds target size", nil)
	}
	n, err := readProgress(r.encoded, p[:int(want)])
	r.literal -= int64(n)
	r.produced += int64(n)
	if err != nil {
		if n == int(want) && errors.Is(err, io.EOF) {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			return n, io.ErrUnexpectedEOF
		}
		return n, err
	}
	if n == 0 {
		return 0, io.ErrNoProgress
	}
	return n, nil
}

func (r *patchReader) readCopy(p []byte) (int, error) {
	want := int64(len(p))
	if want > r.copyLeft {
		want = r.copyLeft
	}
	if want > r.targetSize-r.produced {
		return 0, malformed("copy exceeds target size", nil)
	}
	n, err := readProgress(r.basis, p[:int(want)])
	r.copyLeft -= int64(n)
	r.produced += int64(n)
	if err != nil {
		if n == int(want) && errors.Is(err, io.EOF) {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			return n, io.ErrUnexpectedEOF
		}
		return n, err
	}
	if n == 0 {
		return 0, io.ErrNoProgress
	}
	return n, nil
}

func (r *patchReader) nextCommand() (bool, error) {
	var one [1]byte
	if _, err := io.ReadFull(r.encoded, one[:]); err != nil {
		return false, malformed("read command", err)
	}
	op := one[0]
	if op == 0 {
		if r.produced != r.targetSize {
			return false, malformed(fmt.Sprintf("output length %d, expected %d", r.produced, r.targetSize), nil)
		}
		var trailing [1]byte
		n, err := readProgress(r.encoded, trailing[:])
		if n != 0 {
			return false, malformed("trailing bytes after end command", nil)
		}
		if !errors.Is(err, io.EOF) {
			if err == nil {
				err = io.ErrNoProgress
			}
			return false, malformed("verify delta end", err)
		}
		return true, nil
	}
	r.commands++
	if r.commands > maxCommands {
		return false, fmt.Errorf("%w: command count exceeds %d", ErrMalformedDelta, maxCommands)
	}
	if op <= 64 {
		return false, r.setLiteral(int64(op))
	}
	if op <= 68 {
		width := []int{0, 1, 2, 4, 8}[op-64]
		length, err := r.readUint(width)
		if err != nil {
			return false, err
		}
		if length > uint64(mathMaxInt64) {
			return false, malformed("literal length overflow", nil)
		}
		return false, r.setLiteral(int64(length))
	}
	if op < 69 || op > 84 {
		return false, malformed(fmt.Sprintf("unsupported command %d", op), nil)
	}
	command := int(op - 69)
	offsetWidth := []int{1, 2, 4, 8}[command/4]
	lengthWidth := []int{1, 2, 4, 8}[command%4]
	offset, err := r.readUint(offsetWidth)
	if err != nil {
		return false, err
	}
	length, err := r.readUint(lengthWidth)
	if err != nil {
		return false, err
	}
	if length == 0 || offset > uint64(r.basisSize) || length > uint64(r.basisSize)-offset {
		return false, malformed("copy range outside basis", nil)
	}
	if length > uint64(r.targetSize-r.produced) {
		return false, malformed("copy exceeds target size", nil)
	}
	position, err := r.basis.Seek(int64(offset), io.SeekStart)
	if err != nil {
		return false, malformed("seek basis", err)
	}
	if position != int64(offset) {
		return false, malformed("basis seek returned unexpected offset", nil)
	}
	r.copyLeft = int64(length)
	return false, nil
}

const mathMaxInt64 = int64(^uint64(0) >> 1)

func (r *patchReader) setLiteral(length int64) error {
	if length <= 0 || length > r.targetSize-r.produced {
		return malformed("literal length outside target", nil)
	}
	r.literal = length
	return nil
}

func (r *patchReader) readUint(width int) (uint64, error) {
	var b [8]byte
	if width != 1 && width != 2 && width != 4 && width != 8 {
		return 0, malformed("invalid integer width", nil)
	}
	if _, err := io.ReadFull(r.encoded, b[:width]); err != nil {
		return 0, malformed("read command integer", err)
	}
	switch width {
	case 1:
		return uint64(b[0]), nil
	case 2:
		return uint64(binary.BigEndian.Uint16(b[:2])), nil
	case 4:
		return uint64(binary.BigEndian.Uint32(b[:4])), nil
	default:
		return binary.BigEndian.Uint64(b[:]), nil
	}
}
