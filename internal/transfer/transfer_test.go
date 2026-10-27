package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"testing"
)

func TestParameters(t *testing.T) {
	for _, test := range []struct {
		basis, target int64
		want          uint32
	}{
		{0, 0, minBlockSize},
		{1, 1, minBlockSize},
		{4 << 30, 0, minBlockSize},
		{(4 << 30) + 1, 0, 2 * minBlockSize},
		{(64 << 30) + 1, 0, 2 * 1024 * 1024},
		{1 << 40, 1 << 40, maxBlockSize},
	} {
		got, err := Parameters(test.basis, test.target)
		if err != nil || got != test.want {
			t.Fatalf("Parameters(%d,%d)=(%d,%v), want %d", test.basis, test.target, got, err, test.want)
		}
	}
	for _, test := range [][2]int64{{-1, 0}, {0, -1}, {maxFileSize + 1, 0}, {0, maxFileSize + 1}} {
		if _, err := Parameters(test[0], test[1]); !errors.Is(err, ErrInvalidSize) {
			t.Errorf("Parameters(%d,%d) error=%v, want invalid size", test[0], test[1], err)
		}
	}
}

func TestSignatureDeltaReaderRoundTrip(t *testing.T) {
	for _, size := range []int{0, 7, 64 << 10, 200 << 10, 2 << 20} {
		basis := make([]byte, size)
		_, _ = rand.New(rand.NewSource(int64(size) + 30)).Read(basis)
		for _, kind := range []string{"same", "mutated", "prefix"} {
			target := append([]byte(nil), basis...)
			switch kind {
			case "mutated":
				if len(target) != 0 {
					target[len(target)/2] ^= 0xff
				}
			case "prefix":
				target = append([]byte{0xa5}, target...)
			}
			sig, err := Signature(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)))
			if err != nil {
				t.Fatalf("size %d/%s signature: %v", size, kind, err)
			}
			var encoded bytes.Buffer
			if err := Delta(context.Background(), sig, int64(len(basis)), int64(len(target)), bytes.NewReader(target), &encoded); err != nil {
				t.Fatalf("size %d/%s delta: %v", size, kind, err)
			}
			decoded, err := NewReader(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)), bytes.NewReader(encoded.Bytes()))
			if err != nil {
				t.Fatalf("size %d/%s decoder: %v", size, kind, err)
			}
			got, err := io.ReadAll(decoded)
			if err != nil || !bytes.Equal(got, target) {
				t.Fatalf("size %d/%s decode differs (len=%d err=%v)", size, kind, len(got), err)
			}
		}
	}
}

func TestDecoderReadsFromBasisForCopyCommands(t *testing.T) {
	basis := make([]byte, 256<<10)
	_, _ = rand.New(rand.NewSource(42)).Read(basis)
	target := append([]byte{0xaa, 0xbb}, basis...)
	signature, err := Signature(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)))
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := Delta(context.Background(), signature, int64(len(basis)), int64(len(target)), bytes.NewReader(target), &encoded); err != nil {
		t.Fatal(err)
	}
	basisReader := &countingSeeker{Reader: bytes.NewReader(basis)}
	decoded, err := NewReader(context.Background(), basisReader, int64(len(basis)), int64(len(target)), bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(decoded)
	if err != nil || !bytes.Equal(got, target) {
		t.Fatalf("decode differs: len=%d err=%v", len(got), err)
	}
	if basisReader.readBytes == 0 {
		t.Fatal("encoded delta did not exercise a basis copy")
	}
}

type countingSeeker struct {
	*bytes.Reader
	readBytes int64
}

func (r *countingSeeker) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.readBytes += int64(n)
	return n, err
}

func TestSignatureRequiresExactSourceSize(t *testing.T) {
	for _, test := range []struct {
		name    string
		data    []byte
		readErr error
	}{
		{name: "short", data: []byte("abc")},
		{name: "extra", data: []byte("abcdef")},
		{name: "read-error", data: []byte("abc"), readErr: errInjectedRead},
		{name: "bytes-with-eof", data: []byte("abcd"), readErr: io.EOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := io.Reader(bytes.NewReader(test.data))
			if test.readErr != nil {
				reader = &dataThenError{data: test.data, err: test.readErr}
			}
			_, err := Signature(context.Background(), reader, 4, 4)
			if errors.Is(test.readErr, io.EOF) {
				if err != nil {
					t.Fatalf("Signature rejected legal bytes+EOF read: %v", err)
				}
			} else if err == nil {
				t.Fatal("Signature accepted a source that was not exactly four bytes")
			}
		})
	}
}

func TestReadersTolerateTransientNoProgress(t *testing.T) {
	basis := []byte("basis bytes")
	target := []byte("target bytes")
	signature, err := Signature(context.Background(), &emptyOnceAtEOF{reader: bytes.NewReader(basis)}, int64(len(basis)), int64(len(target)))
	if err != nil {
		t.Fatalf("Signature rejected transient empty read: %v", err)
	}
	var encoded bytes.Buffer
	if err := Delta(context.Background(), signature, int64(len(basis)), int64(len(target)), &emptyOnceAtEOF{reader: bytes.NewReader(target)}, &encoded); err != nil {
		t.Fatalf("Delta rejected transient empty read: %v", err)
	}
	reader, err := NewReader(context.Background(), &emptyOnceSeeker{reader: bytes.NewReader(basis)}, int64(len(basis)), int64(len(target)), &emptyOnceAtEOF{reader: bytes.NewReader(encoded.Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, target) {
		t.Fatalf("decoder rejected transient empty read: len=%d err=%v", len(got), err)
	}
}

type emptyOnceAtEOF struct {
	reader io.Reader
	empty  bool
}

type emptyOnceSeeker struct {
	reader *bytes.Reader
	empty  bool
}

func (r *emptyOnceSeeker) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n == 0 && errors.Is(err, io.EOF) && !r.empty {
		r.empty = true
		return 0, nil
	}
	return n, err
}

func (r *emptyOnceSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.reader.Seek(offset, whence)
}

func (r *emptyOnceAtEOF) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n == 0 && errors.Is(err, io.EOF) && !r.empty {
		r.empty = true
		return 0, nil
	}
	return n, err
}

var errInjectedRead = errors.New("injected read error")

type dataThenError struct {
	data []byte
	err  error
	done bool
}

func (r *dataThenError) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), r.err
	}
	return 0, r.err
}

func TestDeltaRequiresExactSourceAndPropagatesOutputErrors(t *testing.T) {
	basis := []byte("basis bytes")
	target := []byte("target bytes")
	sig, err := Signature(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		reader io.Reader
	}{
		{name: "short", reader: bytes.NewReader(target[:len(target)-1])},
		{name: "extra", reader: bytes.NewReader(append(append([]byte(nil), target...), 0))},
		{name: "injected", reader: &dataThenError{data: target, err: errInjectedRead}},
		{name: "no-progress", reader: zeroReader{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := Delta(context.Background(), sig, int64(len(basis)), int64(len(target)), test.reader, io.Discard); err == nil {
				t.Fatal("Delta accepted invalid source")
			}
		})
	}
	if err := Delta(context.Background(), sig, int64(len(basis)), int64(len(target)), &dataThenError{data: target, err: io.EOF}, io.Discard); err != nil {
		t.Fatalf("Delta rejected legal bytes+EOF read: %v", err)
	}
	if err := Delta(context.Background(), sig, int64(len(basis)), int64(len(target)), bytes.NewReader(target), failWriter{}); err == nil {
		t.Fatal("Delta ignored output failure")
	}
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errInjectedRead }

func TestSignatureRejectsInvalidHeaderAndLength(t *testing.T) {
	basis := []byte("basis")
	target := []byte("target")
	sig, err := Signature(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)))
	if err != nil {
		t.Fatal(err)
	}
	bad := [][]byte{
		nil,
		append([]byte(nil), sig[:len(sig)-1]...),
		append(append([]byte(nil), sig...), 0),
		append([]byte(nil), sig...),
	}
	bad[3][0] ^= 0xff
	for i, candidate := range bad {
		if err := Delta(context.Background(), candidate, int64(len(basis)), int64(len(target)), bytes.NewReader(target), io.Discard); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("invalid signature case %d error=%v", i, err)
		}
	}
}

func TestNewReaderRejectsMalformedDeltas(t *testing.T) {
	basis := []byte("basis")
	for _, test := range []struct {
		name   string
		delta  []byte
		output int64
	}{
		{name: "bad-magic", delta: []byte{0, 0, 0, 0}},
		{name: "truncated-header", delta: []byte{0x72}},
		{name: "unknown-op", delta: []byte{0x72, 0x73, 0x02, 0x36, 0xff}},
		{name: "truncated-literal", delta: []byte{0x72, 0x73, 0x02, 0x36, 3, 'a'}, output: 3},
		{name: "out-of-range-copy", delta: []byte{0x72, 0x73, 0x02, 0x36, 69, 5, 2, 0}, output: 2},
		{name: "trailing-bytes", delta: []byte{0x72, 0x73, 0x02, 0x36, 0, 1}},
		{name: "output-size-mismatch", delta: []byte{0x72, 0x73, 0x02, 0x36, 0}, output: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, err := NewReader(context.Background(), bytes.NewReader(basis), int64(len(basis)), test.output, bytes.NewReader(test.delta))
			if err != nil {
				return
			}
			if _, err := io.Copy(io.Discard, reader); err == nil {
				t.Fatal("decoder accepted malformed delta")
			}
		})
	}
}

func TestNewReaderValidatesTrailerOnTargetPlusOneRead(t *testing.T) {
	basis := []byte("basis payload")
	target := []byte("target payload")
	signature, err := Signature(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)))
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := Delta(context.Background(), signature, int64(len(basis)), int64(len(target)), bytes.NewReader(target), &encoded); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)), bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.CopyN(io.Discard, reader, int64(len(target))+1)
	if n != int64(len(target)) || !errors.Is(err, io.EOF) || errors.Is(err, ErrMalformedDelta) {
		t.Fatalf("valid target+1 read=(%d,%v), want target and EOF", n, err)
	}

	withTrailing := append(append([]byte(nil), encoded.Bytes()...), 0xff)
	reader, err = NewReader(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)), bytes.NewReader(withTrailing))
	if err != nil {
		t.Fatal(err)
	}
	n, err = io.CopyN(io.Discard, reader, int64(len(target))+1)
	if n != int64(len(target)) || !errors.Is(err, ErrMalformedDelta) {
		t.Fatalf("trailing target+1 read=(%d,%v), want malformed trailer", n, err)
	}
}

type cancelOnEOF struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r *cancelOnEOF) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n == 0 && errors.Is(err, io.EOF) {
		r.cancel()
	}
	return n, err
}

func TestNewReaderSurfacesCancellationAtFinalEOF(t *testing.T) {
	basis := []byte("basis")
	target := []byte("target")
	signature, err := Signature(context.Background(), bytes.NewReader(basis), int64(len(basis)), int64(len(target)))
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := Delta(context.Background(), signature, int64(len(basis)), int64(len(target)), bytes.NewReader(target), &encoded); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := &cancelOnEOF{reader: bytes.NewReader(encoded.Bytes()), cancel: cancel}
	reader, err := NewReader(ctx, bytes.NewReader(basis), int64(len(basis)), int64(len(target)), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("reader error=%v, want final-read cancellation", err)
	}
}

func TestNewReaderBoundsCommandCount(t *testing.T) {
	var encoded bytes.Buffer
	encoded.Write([]byte{0x72, 0x73, 0x02, 0x36})
	for i := 0; i <= maxCommands; i++ {
		encoded.Write([]byte{1, 'x'})
	}
	encoded.WriteByte(0)
	reader, err := NewReader(context.Background(), bytes.NewReader(nil), 0, maxCommands+1, bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, reader); err == nil || !errors.Is(err, ErrMalformedDelta) {
		t.Fatalf("command cap error=%v, want malformed delta", err)
	}
}

func TestNewReaderPropagatesReadSeekAndContextErrors(t *testing.T) {
	valid := []byte{0x72, 0x73, 0x02, 0x36, 69, 0, 1, 1, 0}
	reader, err := NewReader(context.Background(), brokenSeeker{}, 1, 1, bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("decoder ignored basis seek failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewReader(ctx, bytes.NewReader(nil), 0, 0, bytes.NewReader([]byte{0x72, 0x73, 0x02, 0x36})); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewReader canceled context error=%v", err)
	}
}

type brokenSeeker struct{}

func (brokenSeeker) Read([]byte) (int, error)       { return 0, io.EOF }
func (brokenSeeker) Seek(int64, int) (int64, error) { return 0, errInjectedRead }

func FuzzNewReaderNoPanic(f *testing.F) {
	f.Add([]byte{0x72, 0x73, 0x02, 0x36, 0})
	f.Add([]byte{0x72, 0x73, 0x02, 0x36, 69, 0xff, 0xff, 0xff, 0xff, 0})
	f.Fuzz(func(t *testing.T, encoded []byte) {
		if len(encoded) > 1<<20 {
			t.Skip()
		}
		reader, err := NewReader(context.Background(), bytes.NewReader(make([]byte, 128)), 128, 64, bytes.NewReader(encoded))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, reader)
	})
}
