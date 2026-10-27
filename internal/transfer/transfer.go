// Package transfer provides bounded whole-file rsync transfer primitives.
//
// Delta is a byte-transfer optimization only: callers must still verify the
// reconstructed target against its sealed size and content identity before
// publishing or installing it.
package transfer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/balena-os/librsync-go"
)

const (
	maxFileSize       int64  = 1 << 40
	minBlockSize      uint32 = 64 << 10
	maxBlockSize      uint32 = 16 << 20
	strongSize        uint32 = 32
	maxSignatureBlock        = 1 << 16
	// A 1 TiB target is at most 65,536 adaptive blocks. Delta can emit at
	// most one copy run per matched block; literals split on transitions and
	// at 16 MiB. This cap leaves substantial room over that upper bound.
	maxCommands            = 1 << 20
	deltaFramingPerCommand = 17
	deltaHeaderSize        = 5
)

var (
	ErrInvalidSize      = errors.New("invalid transfer size")
	ErrLimitExceeded    = errors.New("transfer limit exceeded")
	ErrInvalidSignature = errors.New("invalid rsync signature")
	ErrMalformedDelta   = errors.New("malformed rsync delta")
	ErrSourceSize       = errors.New("source size does not match sealed size")
)

// Parameters chooses the block size used for both signature generation and
// delta matching. The larger input determines the block size so either side
// stays within the maximum retained signature count.
func Parameters(basisSize, targetSize int64) (uint32, error) {
	if basisSize < 0 || targetSize < 0 || basisSize > maxFileSize || targetSize > maxFileSize {
		return 0, ErrInvalidSize
	}
	size := basisSize
	if targetSize > size {
		size = targetSize
	}
	block := minBlockSize
	for blocksFor(size, block) > maxSignatureBlock && block < maxBlockSize {
		block <<= 1
	}
	if blocksFor(size, block) > maxSignatureBlock {
		return 0, ErrLimitExceeded
	}
	return block, nil
}

// Signature makes a bounded BLAKE2 signature for exactly basisSize bytes.
// targetSize participates in choosing a block size shared with Delta.
func Signature(ctx context.Context, basis io.Reader, basisSize, targetSize int64) ([]byte, error) {
	if basis == nil {
		return nil, fmt.Errorf("%w: nil basis reader", ErrSourceSize)
	}
	ctx = nonNilContext(ctx)
	block, err := Parameters(basisSize, targetSize)
	if err != nil {
		return nil, err
	}
	blocks := blocksFor(basisSize, block)
	expected := int64(12) + int64(blocks)*(4+int64(strongSize))
	if expected > int64(math.MaxInt) {
		return nil, ErrLimitExceeded
	}

	input := newExactSource(ctx, basis, basisSize)
	writer := &boundedWriter{ctx: ctx, limit: expected, buffer: &bytes.Buffer{}}
	_, err = librsync.Signature(input, writer, block, strongSize, librsync.BLAKE2_SIG_MAGIC)
	if writer.err != nil {
		return nil, fmt.Errorf("write signature: %w", writer.err)
	}
	if err != nil {
		return nil, fmt.Errorf("generate signature: %w", err)
	}
	if err := input.verifyExact(); err != nil {
		return nil, err
	}
	if writer.count != expected {
		return nil, fmt.Errorf("%w: produced %d signature bytes, expected %d", ErrInvalidSignature, writer.count, expected)
	}
	return writer.buffer.Bytes(), nil
}

// Delta streams a bounded librsync delta from exactly targetSize source bytes
// to output. Output may be partial if generation fails; callers should stage it
// and publish only after NewReader and their sealed-content check succeed.
func Delta(ctx context.Context, signature []byte, basisSize, targetSize int64, source io.Reader, output io.Writer) error {
	if source == nil || output == nil {
		return fmt.Errorf("%w: nil source or output", ErrSourceSize)
	}
	ctx = nonNilContext(ctx)
	block, err := Parameters(basisSize, targetSize)
	if err != nil {
		return err
	}
	sig, err := parseSignature(signature, basisSize, block)
	if err != nil {
		return err
	}
	input := newExactSource(ctx, source, targetSize)
	writer := &boundedWriter{ctx: ctx, limit: maxDeltaBytes(targetSize), output: output}
	deltaErr := librsync.Delta(sig, input, writer)
	if writer.err != nil {
		return fmt.Errorf("write delta: %w", writer.err)
	}
	if deltaErr != nil {
		return fmt.Errorf("generate delta: %w", deltaErr)
	}
	if err := input.verifyExact(); err != nil {
		return err
	}
	return nil
}

// NewReader returns a strict streaming decoder for an encoded librsync delta.
// It does not close basis or encoded; their owner remains responsible for both.
func NewReader(ctx context.Context, basis io.ReadSeeker, basisSize, targetSize int64, encoded io.Reader) (io.Reader, error) {
	if basis == nil || encoded == nil {
		return nil, fmt.Errorf("%w: nil basis or encoded reader", ErrMalformedDelta)
	}
	if _, err := Parameters(basisSize, targetSize); err != nil {
		return nil, err
	}
	ctx = nonNilContext(ctx)
	input := &boundedInput{reader: &safeReader{ctx: ctx, reader: encoded}, limit: maxDeltaBytes(targetSize)}
	var magic [4]byte
	if _, err := io.ReadFull(input, magic[:]); err != nil {
		return nil, malformed("read delta header", err)
	}
	if binary.BigEndian.Uint32(magic[:]) != uint32(librsync.DELTA_MAGIC) {
		return nil, fmt.Errorf("%w: bad delta magic", ErrMalformedDelta)
	}
	return &patchReader{
		ctx:        ctx,
		basis:      &safeReadSeeker{ctx: ctx, seeker: basis},
		basisSize:  basisSize,
		targetSize: targetSize,
		encoded:    input,
	}, nil
}

func blocksFor(size int64, block uint32) int64 {
	if size == 0 {
		return 0
	}
	return (size + int64(block) - 1) / int64(block)
}

func maxDeltaBytes(targetSize int64) int64 {
	return targetSize + maxCommands*deltaFramingPerCommand + deltaHeaderSize
}

func parseSignature(signature []byte, basisSize int64, block uint32) (*librsync.SignatureType, error) {
	blocks := blocksFor(basisSize, block)
	expected := int64(12) + blocks*(4+int64(strongSize))
	if int64(len(signature)) != expected || expected < 12 {
		return nil, fmt.Errorf("%w: length %d, expected %d", ErrInvalidSignature, len(signature), expected)
	}
	if binary.BigEndian.Uint32(signature[0:4]) != uint32(librsync.BLAKE2_SIG_MAGIC) ||
		binary.BigEndian.Uint32(signature[4:8]) != block ||
		binary.BigEndian.Uint32(signature[8:12]) != strongSize {
		return nil, fmt.Errorf("%w: unexpected header parameters", ErrInvalidSignature)
	}
	sig, err := librsync.ReadSignature(bytes.NewReader(signature))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	return sig, nil
}

type boundedWriter struct {
	ctx    context.Context
	limit  int64
	count  int64
	buffer *bytes.Buffer
	output io.Writer
	err    error
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.ctx != nil {
		if err := w.ctx.Err(); err != nil {
			w.err = err
			return 0, err
		}
	}
	if int64(len(p)) > w.limit-w.count {
		w.err = ErrLimitExceeded
		return 0, w.err
	}
	var n int
	if w.buffer != nil {
		n, w.err = w.buffer.Write(p)
	} else {
		n, w.err = w.output.Write(p)
		if w.err == nil && n != len(p) {
			w.err = io.ErrShortWrite
		}
	}
	w.count += int64(n)
	if w.err != nil {
		return n, w.err
	}
	return n, nil
}

type safeReader struct {
	ctx        context.Context
	reader     io.Reader
	emptyReads int
}

func (r *safeReader) Read(p []byte) (int, error) {
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
	n, err := r.reader.Read(p)
	if n > 0 {
		r.emptyReads = 0
	} else if err == nil {
		r.emptyReads++
		if r.emptyReads >= 100 {
			return 0, io.ErrNoProgress
		}
	}
	if r.ctx != nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
	}
	return n, err
}

type exactSource struct {
	reader *safeReader
	limit  int64
	count  int64
}

func newExactSource(ctx context.Context, r io.Reader, size int64) *exactSource {
	ctx = nonNilContext(ctx)
	return &exactSource{reader: &safeReader{ctx: ctx, reader: r}, limit: size}
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (r *exactSource) Read(p []byte) (int, error) {
	if r.count >= r.limit {
		return 0, io.EOF
	}
	remaining := r.limit - r.count
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

func (r *exactSource) verifyExact() error {
	if r.count != r.limit {
		return fmt.Errorf("%w: read %d bytes, expected %d", ErrSourceSize, r.count, r.limit)
	}
	var extra [1]byte
	n, err := readProgress(r.reader, extra[:])
	if n != 0 {
		return fmt.Errorf("%w: source contains bytes after %d", ErrSourceSize, r.limit)
	}
	if err == nil {
		return fmt.Errorf("%w: source did not end after %d bytes", ErrSourceSize, r.limit)
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("verify source end: %w", err)
	}
	return nil
}

func readProgress(reader io.Reader, p []byte) (int, error) {
	for {
		n, err := reader.Read(p)
		if n != 0 || err != nil {
			return n, err
		}
	}
}

// safeReadSeeker checks cancellation around basis seeks and reads while leaving
// the underlying seek position and lifetime with the caller.
type safeReadSeeker struct {
	ctx    context.Context
	seeker io.ReadSeeker
	reader *safeReader
}

func (r *safeReadSeeker) Read(p []byte) (int, error) {
	if r.reader == nil {
		r.reader = &safeReader{ctx: r.ctx, reader: r.seeker}
	}
	return r.reader.Read(p)
}

func (r *safeReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.seeker.Seek(offset, whence)
}

func malformed(where string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s", ErrMalformedDelta, where)
	}
	return fmt.Errorf("%w: %s: %w", ErrMalformedDelta, where, err)
}
