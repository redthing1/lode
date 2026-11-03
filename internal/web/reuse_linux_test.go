package web

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestSaveRenameSkipsContentUpload(t *testing.T) {
	f := apiTest(t)
	ctx := context.Background()
	data := make([]byte, 4<<20)
	rand.New(rand.NewSource(1)).Read(data)
	before := entry("FILE.iso", data)
	first, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{before}}, nil, func(model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := &countedTransport{RoundTripper: f.client.HTTP.Transport}
	f.client.HTTP.Transport = counts
	after := before
	after.Path = "FILE.ISO"
	changes := []model.Entry{{Path: before.Path, Kind: model.Absent}, after, entry("other", []byte("changed"))}
	request := model.Save{ID: operation(t), Branch: "main", Base: first.ID, Changes: changes, Reuse: model.ReuseFiles([]model.Entry{before}, changes)}
	second, err := f.client.Save(ctx, "alice", "project", request, []model.Entry{before}, func(e model.Entry) (io.ReadCloser, error) {
		if e.Path != "other" {
			t.Errorf("rename opened local contents: %q", e.Path)
		}
		return io.NopCloser(bytes.NewReader([]byte("changed"))), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts.upload.Load() > 4096 || counts.signature.Load() != 0 {
		t.Fatalf("rename transfer: upload=%d, signature=%d", counts.upload.Load(), counts.signature.Load())
	}
	t.Logf("4 MiB rename plus small edit: HTTP upload=%d bytes, signature=%d bytes", counts.upload.Load(), counts.signature.Load())
	var out bytes.Buffer
	if err := f.client.Download(ctx, "alice", "project", second.ID, after, nil, 0, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("renamed contents changed")
	}
	if retry, err := f.client.Save(ctx, "alice", "project", request, nil, nil); err != nil || retry.ID != second.ID {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
}
