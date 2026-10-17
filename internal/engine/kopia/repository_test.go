package kopia_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"

	"github.com/redthing1/lode/internal/model"
)

// Only the test backend controls time. All bytes use the real filesystem backend,
// and collection retains Kopia's unmodified SafetyFull parameters.
type clockKey struct{}

type proofClock struct {
	nanos   atomic.Int64
	written atomic.Int64
	deleted atomic.Int64
}

func (c *proofClock) now() time.Time          { return time.Now().UTC().Add(time.Duration(c.nanos.Load())) }
func (c *proofClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

type storageOptions struct {
	Path string `json:"path"`
}

type clockStorage struct {
	blob.Storage
	clock   *proofClock
	options storageOptions
}

func init() {
	blob.AddSupportedStorage("lode-proof-clock", storageOptions{}, func(ctx context.Context, opt *storageOptions, create bool) (blob.Storage, error) {
		clock, ok := ctx.Value(clockKey{}).(*proofClock)
		if !ok {
			return nil, fmt.Errorf("test clock missing")
		}
		st, err := filesystem.New(ctx, &filesystem.Options{Path: opt.Path}, create)
		if err != nil {
			return nil, err
		}
		return &clockStorage{Storage: st, clock: clock, options: *opt}, nil
	})
}

func (s *clockStorage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: "lode-proof-clock", Config: &s.options}
}

func (s *clockStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	if opts.SetModTime.IsZero() {
		opts.SetModTime = s.clock.now()
	}
	if err := s.Storage.PutBlob(ctx, id, data, opts); err != nil {
		return err
	}
	s.clock.written.Add(int64(data.Length()))
	return nil
}

func (s *clockStorage) DeleteBlob(ctx context.Context, id blob.ID) error {
	if err := s.Storage.DeleteBlob(ctx, id); err != nil {
		return err
	}
	s.clock.deleted.Add(1)
	return nil
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	dir    string
	clock  *proofClock
	rep    repo.DirectRepository
	config string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c := &proofClock{}
	f := &fixture{t: t, dir: t.TempDir(), clock: c}
	f.ctx = context.WithValue(t.Context(), clockKey{}, c)
	st, err := blob.NewStorage(f.ctx, blob.ConnectionInfo{Type: "lode-proof-clock", Config: &storageOptions{Path: filepath.Join(f.dir, "repository")}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Initialize(f.ctx, st, nil, "temporary-proof-password"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.config = f.connect("server")
	r, err := repo.Open(f.ctx, f.config, "temporary-proof-password", &repo.Options{TimeNowFunc: c.now})
	if err != nil {
		t.Fatal(err)
	}
	f.rep = r.(repo.DirectRepository)
	t.Cleanup(func() {
		if err := f.rep.Close(context.WithoutCancel(f.ctx)); err != nil {
			t.Error(err)
		}
	})
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		params := maintenance.DefaultParams()
		params.Owner = w.ClientOptions().UsernameAtHost()
		params.QuickCycle.Enabled, params.FullCycle.Enabled = false, false
		return maintenance.SetParams(ctx, w, &params)
	})
	return f
}

func (f *fixture) connect(name string) string {
	f.t.Helper()
	st, err := blob.NewStorage(f.ctx, blob.ConnectionInfo{Type: "lode-proof-clock", Config: &storageOptions{Path: filepath.Join(f.dir, "repository")}}, false)
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close(f.ctx)
	config := filepath.Join(f.dir, name+".config")
	if err := repo.Connect(f.ctx, config, st, "temporary-proof-password", &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{Username: "lode", Hostname: name},
		CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(f.dir, name+"-cache"),
			ContentCacheSizeBytes: 1 << 20, MetadataCacheSizeBytes: 1 << 20},
	}); err != nil {
		f.t.Fatal(err)
	}
	return config
}

func (f *fixture) reader(name string) repo.Repository {
	f.t.Helper()
	r, err := repo.Open(f.ctx, f.connect(name), "temporary-proof-password", &repo.Options{TimeNowFunc: f.clock.now})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) write(fn func(context.Context, repo.RepositoryWriter) error) {
	f.t.Helper()
	if err := repo.WriteSession(f.ctx, f.rep, repo.WriteSessionOptions{Purpose: "engine proof"}, fn); err != nil {
		f.t.Fatal(err)
	}
	if err := f.rep.Refresh(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func path(t *testing.T, name string) model.Path {
	t.Helper()
	p, err := model.ParsePath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
