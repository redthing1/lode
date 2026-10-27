package transfer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func encoded(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	base := make([]byte, 75000)
	for i := range base {
		base[i] = byte((i*37 + i/251) % 256)
	}
	target := append([]byte(nil), base...)
	copy(target[30000:30100], bytes.Repeat([]byte{0xff}, 100))
	var sig, delta bytes.Buffer
	if err := Signature(context.Background(), bytes.NewReader(base), int64(len(base)), &sig); err != nil {
		t.Fatal(err)
	}
	if err := Delta(context.Background(), bytes.NewReader(sig.Bytes()), bytes.NewReader(target), &delta); err != nil {
		t.Fatal(err)
	}
	return base, target, append([]byte(nil), delta.Bytes()...)
}
func TestRoundTripOpaqueStream(t *testing.T) {
	base, target, d := encoded(t)
	var sig bytes.Buffer
	if err := Signature(context.Background(), bytes.NewReader(base), int64(len(base)), &sig); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Patch(context.Background(), bytes.NewReader(base), int64(len(base)), bytes.NewReader(d), int64(len(target)), &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), target) {
		t.Fatal("patched bytes differ")
	}
}
func TestPatchRejectsMalformedAndWrongSize(t *testing.T) {
	base, target, d := encoded(t)
	for _, tc := range []struct {
		name  string
		delta []byte
		size  int64
	}{{"truncated", d[:len(d)-1], int64(len(target))}, {"trailing", append(append([]byte(nil), d...), 0), int64(len(target))}, {"small cap", d, int64(len(target) - 1)}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Patch(context.Background(), bytes.NewReader(base), int64(len(base)), bytes.NewReader(tc.delta), tc.size, io.Discard); err == nil {
				t.Fatal("accepted malformed patch")
			}
		})
	}
}
func TestCallbacksAndContextErrorsPropagate(t *testing.T) {
	base, _, _ := encoded(t)
	want := errors.New("sink failed")
	if err := Signature(context.Background(), bytes.NewReader(base), int64(len(base)), errorWriter{want}); !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if err := Signature(context.Background(), &terminalErrorReader{data: base, err: want}, int64(len(base)), io.Discard); !errors.Is(err, want) {
		t.Fatalf("reader returned data with error, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := Signature(ctx, bytes.NewReader(base), int64(len(base)), &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type terminalErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *terminalErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.data)
	return n, r.err
}

func TestDeltaRejectsUnsupportedSignatureHeader(t *testing.T) {
	base, _, _ := encoded(t)
	var sig bytes.Buffer
	if err := Signature(context.Background(), bytes.NewReader(base), int64(len(base)), &sig); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		offset int
		value  byte
	}{{"magic", 3, 0}, {"block length", 6, 0}, {"checksum length", 11, 31}} {
		t.Run(tc.name, func(t *testing.T) {
			bad := append([]byte(nil), sig.Bytes()...)
			bad[tc.offset] = tc.value
			if err := Delta(context.Background(), bytes.NewReader(bad), bytes.NewReader(base), io.Discard); err == nil {
				t.Fatal("accepted unsupported signature header")
			}
		})
	}
}

func TestDeltaRejectsBlockAboveScratchBound(t *testing.T) {
	base, _, _ := encoded(t)
	var sig bytes.Buffer
	if err := Signature(context.Background(), bytes.NewReader(base), int64(len(base)), &sig); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), sig.Bytes()...)
	binary.BigEndian.PutUint32(bad[4:8], MaxBlockSize+1)
	if err := Delta(context.Background(), bytes.NewReader(bad), bytes.NewReader(base), io.Discard); err == nil {
		t.Fatal("accepted block length above scratch bound")
	}
}

func TestBasisLimitMatchesSignatureAndBlockBounds(t *testing.T) {
	if MaxBasisSize != int64(MaxBlockSize)*int64(maxSignatureBlocks) {
		t.Fatalf("basis limit %d does not match block and record bounds", MaxBasisSize)
	}
	err := Signature(context.Background(), bytes.NewReader(nil), MaxBasisSize+1, io.Discard)
	if err == nil {
		t.Fatal("accepted basis larger than representable signature limit")
	}
}

func TestRejectsTrailingInputAndOversizedSignature(t *testing.T) {
	base, _, _ := encoded(t)
	var sig bytes.Buffer
	if err := Signature(context.Background(), bytes.NewReader(base), int64(len(base)), &sig); err != nil {
		t.Fatal(err)
	}
	if err := Delta(context.Background(), bytes.NewReader(append(sig.Bytes(), 1)), bytes.NewReader(base), io.Discard); err == nil {
		t.Fatal("accepted trailing signature")
	}
	if err := Delta(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0}, MaxSignatureSize+1)), bytes.NewReader(base), io.Discard); err == nil {
		t.Fatal("accepted oversized signature")
	}
}
