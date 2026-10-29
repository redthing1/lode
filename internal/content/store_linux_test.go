package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/repo/blob"
	"github.com/redthing1/lode/internal/model"
)

func TestNativeTreeReopenAndRelocate(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "store")
	s, err := Open(ctx, root, "test-only", true)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("opaque\x00\xff")
	hash := sha256.Sum256(data)
	e := model.Entry{Path: "a", Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	var object Object
	var protection string
	err = s.Write(ctx, func(w *Writer) error {
		var err error
		object, err = w.File(e, func(out io.Writer) error { _, err := out.Write(data); return err })
		if err != nil {
			return err
		}
		dir, err := w.Directory([]Entry{{Entry: e, Object: object}})
		if err != nil {
			return err
		}
		protection, err = w.Protect("operation", dir)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, moved, "test-only", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	r, err := s.Read(ctx, object.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != string(data) {
		t.Fatalf("read %x: %v", got, err)
	}
	roots, err := s.Roots(ctx)
	if err != nil || roots[protection].Operation != "operation" {
		t.Fatalf("protection: %v %v", roots, err)
	}
}

func TestFilesystemPutPropagatesDirectorySyncFailure(t *testing.T) {
	ctx := context.Background()
	s, err := newStorage(ctx, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	want := errors.New("directory sync failed")
	s.(*durableFilesystem).syncDir = func(string) error { return want }
	if err := s.PutBlob(ctx, blob.ID("blob"), byteBlob("data"), blob.PutOptions{}); !errors.Is(err, want) {
		t.Fatalf("put: %v", err)
	}
}

type byteBlob []byte
type readSeekCloser struct{ *bytes.Reader }

func (r readSeekCloser) Close() error                 { return nil }
func (b byteBlob) Length() int                        { return len(b) }
func (b byteBlob) Reader() io.ReadSeekCloser          { return readSeekCloser{bytes.NewReader(b)} }
func (b byteBlob) WriteTo(w io.Writer) (int64, error) { n, err := w.Write(b); return int64(n), err }
