// Package transfer wraps librsync's streaming jobs with Go readers and writers.
package transfer

/*
#cgo LDFLAGS: -lrsync
#include "bridge.h"
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime/cgo"
	"unsafe"
)

// MaxSignatureSize bounds encoded signatures accepted by Delta.
const MaxSignatureSize = 1 << 20

// MaxBlockSize bounds librsync's per-block scratch allocation.
const MaxBlockSize = 1 << 20

const signatureHeaderSize = 12
const signatureRecordSize = 36
const maxSignatureBlocks = (MaxSignatureSize - signatureHeaderSize) / signatureRecordSize

// MaxBasisSize is the largest basis representable within MaxSignatureSize and
// MaxBlockSize. Larger files can use whole-file transfer.
const MaxBasisSize = int64(MaxBlockSize) * maxSignatureBlocks

type stream struct {
	ctx       context.Context
	in        io.Reader
	out       io.Writer
	basis     io.ReadSeeker
	basisSize int64
	err       error
	readErr   error
	eof       bool
	written   int64
	limit     int64
	read      int64
}

//export goRead
func goRead(h C.uintptr_t, dst *C.uchar, cap C.size_t, nr *C.size_t, eof *C.int) C.int {
	s := cgo.Handle(h).Value().(*stream)
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return 1
	}
	if s.err != nil {
		return 1
	}
	if s.readErr != nil {
		s.err = s.readErr
		return 1
	}
	if s.eof {
		*eof = 1
		return 0
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(dst)), int(cap))
	n, err := s.in.Read(b)
	s.read += int64(n)
	*nr = C.size_t(n)
	if errors.Is(err, io.EOF) {
		s.eof = true
		*eof = 1
	} else if err != nil {
		if n > 0 {
			s.readErr = err
		} else {
			s.err = err
			return 1
		}
	}
	if n == 0 && err == nil {
		s.err = io.ErrNoProgress
		return 1
	}
	return 0
}

//export goWrite
func goWrite(h C.uintptr_t, src *C.uchar, n C.size_t) C.int {
	s := cgo.Handle(h).Value().(*stream)
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return 1
	}
	if s.err != nil {
		return 1
	}
	count := int64(n)
	if s.limit >= 0 && count > s.limit-s.written {
		s.err = fmt.Errorf("transfer output exceeds limit %d", s.limit)
		return 1
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(src)), int(n))
	wrote, err := s.out.Write(b)
	s.written += int64(wrote)
	if err != nil {
		s.err = err
		return 1
	}
	if wrote != len(b) {
		s.err = io.ErrShortWrite
		return 1
	}
	return 0
}

//export goCopy
func goCopy(h C.uintptr_t, pos C.rs_long_t, dst *C.uchar, want C.size_t, nr *C.size_t) C.int {
	s := cgo.Handle(h).Value().(*stream)
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return 1
	}
	off, n := int64(pos), uint64(want)
	if s.basis == nil || off < 0 || uint64(off) > uint64(s.basisSize) || n > uint64(s.basisSize)-uint64(off) {
		s.err = fmt.Errorf("basis copy outside %d-byte basis", s.basisSize)
		return 1
	}
	if _, err := s.basis.Seek(off, io.SeekStart); err != nil {
		s.err = err
		return 1
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(dst)), int(want))
	got, err := io.ReadFull(s.basis, b)
	*nr = C.size_t(got)
	if err != nil {
		s.err = err
		return 1
	}
	return 0
}

func run(s *stream, j *C.rs_job_t, output bool) error {
	if j == nil {
		return errors.New("librsync returned a nil job")
	}
	defer C.rs_job_free(j)
	h := cgo.NewHandle(s)
	ch := C.uintptr_t(h)
	defer h.Delete()
	var left C.size_t
	o := C.int(0)
	if output {
		o = 1
	}
	r := C.lode_drive(j, ch, o, &left)
	if s.err != nil {
		return s.err
	}
	if s.readErr != nil {
		return s.readErr
	}
	if r != C.RS_DONE {
		return fmt.Errorf("librsync: %s", C.GoString(C.rs_strerror(r)))
	}
	if left != 0 {
		return fmt.Errorf("librsync left %d trailing buffered bytes", uint64(left))
	}
	return nil
}
func eof(r io.Reader) error {
	var b [1]byte
	n, e := r.Read(b[:])
	if n > 0 {
		return errors.New("trailing bytes after librsync END")
	}
	if e == nil {
		return io.ErrNoProgress
	}
	if !errors.Is(e, io.EOF) {
		return e
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Signature streams a basis into a bounded librsync signature (maximum 1 MiB).
// The recommended block size grows when needed to keep the signature within
// MaxSignatureSize. Signatures use rolling BLAKE2 and a 32-byte strong checksum.
func Signature(ctx context.Context, basis io.Reader, basisSize int64, out io.Writer) error {
	if ctx == nil || basis == nil || out == nil || basisSize < 0 {
		return errors.New("invalid signature arguments")
	}
	if basisSize > MaxBasisSize {
		return fmt.Errorf("basis size %d exceeds signature limit of %d bytes", basisSize, MaxBasisSize)
	}
	var recommendedBlock, recommendedStrong C.size_t
	recommendedMagic := C.rs_magic_number(C.RS_RK_BLAKE2_SIG_MAGIC)
	if result := C.rs_sig_args(C.rs_long_t(basisSize), &recommendedMagic, &recommendedBlock, &recommendedStrong); result != C.RS_DONE {
		return fmt.Errorf("librsync signature parameters: %s", C.GoString(C.rs_strerror(result)))
	}
	blockLength := (basisSize + maxSignatureBlocks - 1) / maxSignatureBlocks
	if blockLength < 1 {
		blockLength = 1
	}
	if blockLength < int64(recommendedBlock) {
		blockLength = int64(recommendedBlock)
	}
	if blockLength > MaxBlockSize {
		blockLength = MaxBlockSize
	}
	s := &stream{ctx: ctx, in: basis, out: out, limit: MaxSignatureSize}
	j := C.rs_sig_begin(C.size_t(blockLength), C.size_t(32), recommendedMagic)
	if err := run(s, j, true); err != nil {
		return err
	}
	if s.read != basisSize {
		return fmt.Errorf("basis size %d != declared size %d", s.read, basisSize)
	}
	if err := eof(basis); err != nil {
		return err
	}
	return nil
}

func loadSignature(ctx context.Context, r io.Reader) (*C.rs_signature_t, error) {
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, r: r}, MaxSignatureSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSignatureSize {
		return nil, errors.New("signature exceeds 1 MiB")
	}
	// The wire format is three big-endian words followed by fixed-size
	// (weak checksum, strong checksum) records; librsync treats EOF in a
	// partial final weak checksum as a successful end marker, so validate it.
	if len(data) < 12 {
		return nil, errors.New("truncated signature header")
	}
	magic := binary.BigEndian.Uint32(data[:4])
	blockLength := binary.BigEndian.Uint32(data[4:8])
	strong := binary.BigEndian.Uint32(data[8:12])
	if magic != uint32(C.RS_RK_BLAKE2_SIG_MAGIC) || blockLength == 0 || blockLength > MaxBlockSize || strong != 32 ||
		(len(data)-signatureHeaderSize)%signatureRecordSize != 0 ||
		(len(data)-signatureHeaderSize)/signatureRecordSize > maxSignatureBlocks {
		return nil, errors.New("malformed signature records")
	}
	s := &stream{ctx: ctx, in: bytes.NewReader(data), limit: -1}
	var sums *C.rs_signature_t
	if err := run(s, C.rs_loadsig_begin(&sums), false); err != nil {
		C.rs_free_sumset(sums)
		return nil, err
	}
	if err := eof(s.in); err != nil {
		C.rs_free_sumset(sums)
		return nil, err
	}
	if sums == nil {
		return nil, errors.New("empty signature")
	}
	if x := C.rs_build_hash_table(sums); x != C.RS_DONE {
		C.rs_free_sumset(sums)
		return nil, fmt.Errorf("librsync signature index: %s", C.GoString(C.rs_strerror(x)))
	}
	return sums, nil
}

// Delta streams the target against a bounded librsync signature.
func Delta(ctx context.Context, signature, target io.Reader, out io.Writer) error {
	if ctx == nil || signature == nil || target == nil || out == nil {
		return errors.New("invalid delta arguments")
	}
	sums, err := loadSignature(ctx, signature)
	if err != nil {
		return err
	}
	defer C.rs_free_sumset(sums)
	s := &stream{ctx: ctx, in: target, out: out, limit: -1}
	if err := run(s, C.rs_delta_begin(sums), true); err != nil {
		return err
	}
	return eof(target)
}

// Patch reconstructs exactly targetSize bytes, reading basis ranges on demand.
func Patch(ctx context.Context, basis io.ReadSeeker, basisSize int64, delta io.Reader, targetSize int64, out io.Writer) error {
	if ctx == nil || basis == nil || delta == nil || out == nil || basisSize < 0 || targetSize < 0 {
		return errors.New("invalid patch arguments")
	}
	s := &stream{ctx: ctx, in: delta, out: out, basis: basis, basisSize: basisSize, limit: targetSize}
	h := cgo.NewHandle(s)
	ch := C.uintptr_t(h)
	defer h.Delete()
	var cc unsafe.Pointer
	j := C.lode_patch_begin(ch, &cc)
	if cc != nil {
		defer C.lode_free_copy_context(cc)
	}
	if err := run(s, j, true); err != nil {
		return err
	}
	if err := eof(delta); err != nil {
		return err
	}
	if s.written != targetSize {
		return fmt.Errorf("reconstructed size %d != expected %d", s.written, targetSize)
	}
	return nil
}
