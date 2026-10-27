// Package project owns shared history and publication decisions.
package project

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrStale             = errors.New("branch changed; update the workspace before saving")
	ErrCancelled         = errors.New("operation was cancelled")
	ErrOperationConflict = errors.New("operation ID is bound to a different request")
	ErrInvalid           = errors.New("invalid request")
	ErrAlreadyAccepted   = errors.New("operation was already accepted; resume it")
)

type Commit struct {
	ID      string    `json:"id"`
	Parents []string  `json:"parents"`
	Author  string    `json:"author"`
	Message string    `json:"message"`
	Created time.Time `json:"created"`
}

type Request struct {
	ID      string        `json:"id"`
	Branch  string        `json:"branch"`
	Base    string        `json:"base"`
	Author  string        `json:"author"`
	Message string        `json:"message"`
	Changes []model.Entry `json:"changes"`
}

// Source supplies only changed files. Finish validates the end of a streamed
// request before acceptance; neither method is called for an accepted retry.
type Source struct {
	Open   func(string) (io.ReadCloser, error)
	Finish func() error
}

type Config struct {
	ID       string `json:"id"`
	Token    string `json:"token"`
	Password string `json:"password"`
}

type Project struct {
	mu       sync.RWMutex
	db       *sql.DB
	contents *content.Store
	owner    *os.File
	config   Config
	closed   bool
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func ValidID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == id
}

func Create(ctx context.Context, directory string) (*Project, error) {
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, err
	}
	cfg := Config{ID: NewID(), Token: NewID() + NewID(), Password: NewID() + NewID()}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(directory, "project.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return nil, err
	}
	for _, d := range []string{directory, filepath.Dir(directory)} {
		f, err := os.Open(d)
		if err != nil {
			return nil, err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return nil, err
		}
	}
	return open(ctx, directory, true)
}

func Open(ctx context.Context, directory string) (*Project, error) {
	return open(ctx, directory, false)
}

func open(ctx context.Context, directory string, create bool) (_ *Project, err error) {
	directory, err = filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(directory, "project.json"))
	if err != nil {
		return nil, err
	}
	p := &Project{}
	if err := json.Unmarshal(data, &p.config); err != nil {
		return nil, err
	}
	if !ValidID(p.config.ID) || len(p.config.Token) != 64 || len(p.config.Password) != 64 {
		return nil, errors.New("invalid project configuration")
	}
	p.owner, err = os.OpenFile(filepath.Join(directory, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(p.owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		p.owner.Close()
		return nil, errors.New("project is in use")
	}
	defer func() {
		if err != nil {
			_ = p.Close()
		}
	}()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(directory, "history.sqlite")}).String() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(EXTRA)"
	p.db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	p.db.SetMaxOpenConns(1)
	if create {
		_, err = p.db.ExecContext(ctx, schema)
	} else {
		var version int
		err = p.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
		if err == nil && version != 1 {
			err = errors.New("unsupported project database version")
		}
	}
	if err != nil {
		return nil, err
	}
	p.contents, err = content.Open(ctx, filepath.Join(directory, "content"), p.config.Password, create)
	if err != nil {
		return nil, err
	}
	// No unaccepted candidate is promoted on recovery. The original client
	// request can retry capture; accepted roots must retain their exact identity.
	roots, err := p.contents.Roots(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, "SELECT id,root FROM commits")
	if err != nil {
		return nil, err
	}
	accepted := map[string]string{}
	for rows.Next() {
		var operation, root string
		if err := rows.Scan(&operation, &root); err != nil {
			rows.Close()
			return nil, err
		}
		accepted[operation] = root
		found := false
		for _, id := range roots[operation] {
			if id == root {
				found = true
			}
		}
		if !found {
			rows.Close()
			return nil, fmt.Errorf("accepted version %s has no protected contents", operation)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for operation, ids := range roots {
		for _, id := range ids {
			if id != accepted[operation] {
				if err := p.contents.Release(ctx, id); err != nil {
					return nil, err
				}
			}
		}
	}
	return p, nil
}

func (p *Project) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var errs []error
	if p.contents != nil {
		errs = append(errs, p.contents.Close(context.Background()))
	}
	if p.db != nil {
		errs = append(errs, p.db.Close())
	}
	if p.owner != nil {
		errs = append(errs, p.owner.Close())
	}
	return errors.Join(errs...)
}

func (p *Project) ID() string    { return p.config.ID }
func (p *Project) Token() string { return p.config.Token }

func (p *Project) Head(ctx context.Context, branch string) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.head(ctx, branch)
}

func (p *Project) head(ctx context.Context, branch string) (string, error) {
	var head string
	err := p.db.QueryRowContext(ctx, "SELECT coalesce(tip,'') FROM branches WHERE name=?", branch).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return head, err
}

func canonical(request Request) (Request, string, error) {
	if !ValidID(request.ID) || request.Branch == "" || strings.TrimSpace(request.Message) == "" || len(request.Changes) == 0 {
		return request, "", ErrInvalid
	}
	if request.Base != "" && !ValidID(request.Base) {
		return request, "", ErrInvalid
	}
	request.Changes = append([]model.Entry(nil), request.Changes...)
	model.Sort(request.Changes)
	for i, e := range request.Changes {
		if err := e.Validate(); err != nil {
			return request, "", fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if i > 0 && e.Path == request.Changes[i-1].Path {
			return request, "", fmt.Errorf("%w: duplicate path", ErrInvalid)
		}
	}
	data, err := json.Marshal(request)
	if err != nil {
		return request, "", err
	}
	h := sha256.Sum256(data)
	return request, hex.EncodeToString(h[:]), nil
}

func (p *Project) Save(ctx context.Context, request Request, source Source) (Commit, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, fingerprint, err := canonical(request)
	if err != nil {
		return Commit{}, err
	}
	var stored, outcome string
	err = p.db.QueryRowContext(ctx, "SELECT fingerprint,outcome FROM operations WHERE id=?", r.ID).Scan(&stored, &outcome)
	existing := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Commit{}, err
	}
	if existing {
		if outcome == "cancelled" {
			return Commit{}, ErrCancelled
		}
		if stored != fingerprint {
			return Commit{}, ErrOperationConflict
		}
		switch outcome {
		case "accepted":
			return p.commit(ctx, r.ID)
		case "stale":
			return Commit{}, ErrStale
		}
	} else if _, err := p.db.ExecContext(ctx, "INSERT INTO operations VALUES(?,?,'pending')", r.ID, fingerprint); err != nil {
		return Commit{}, err
	}
	head, err := p.head(ctx, r.Branch)
	if err != nil {
		return Commit{}, err
	}
	if head != r.Base {
		_, err := p.db.ExecContext(ctx, "UPDATE operations SET outcome='stale' WHERE id=?", r.ID)
		return Commit{}, errors.Join(ErrStale, err)
	}
	if existing {
		roots, err := p.contents.Roots(ctx)
		if err != nil {
			return Commit{}, err
		}
		for _, id := range roots[r.ID] {
			if err := p.contents.Release(ctx, id); err != nil {
				return Commit{}, err
			}
		}
	}
	base, err := p.inventory(ctx, r.Base, nil)
	if err != nil {
		return Commit{}, err
	}
	logical := make([]model.Entry, 0, len(base))
	objects := make(map[string]string, len(base))
	for _, e := range base {
		logical = append(logical, e.Entry)
		objects[e.Path] = e.Object
	}
	logical, err = model.Apply(logical, r.Changes)
	if err != nil {
		return Commit{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for _, e := range r.Changes {
		delete(objects, e.Path)
	}
	entries := make([]content.Entry, 0, len(logical))
	for _, e := range logical {
		entries = append(entries, content.Entry{Entry: e, Object: objects[e.Path]})
	}
	root, captured, err := p.contents.Capture(ctx, r.ID, entries, source.Open)
	if err != nil {
		return Commit{}, err
	}
	if source.Finish != nil {
		if err := source.Finish(); err != nil {
			return Commit{}, err
		}
	}
	return p.accept(ctx, r, root, base, captured)
}

func (p *Project) Cancel(ctx context.Context, id string) error {
	if !ValidID(id) {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.ExecContext(ctx, `INSERT INTO operations VALUES(?,'','cancelled') ON CONFLICT(id) DO UPDATE SET outcome='cancelled' WHERE outcome='pending'`, id)
	if err != nil {
		return err
	}
	var outcome string
	if err := p.db.QueryRowContext(ctx, "SELECT outcome FROM operations WHERE id=?", id).Scan(&outcome); err != nil {
		return err
	}
	if outcome == "accepted" {
		return ErrAlreadyAccepted
	}
	roots, err := p.contents.Roots(ctx)
	if err != nil {
		return err
	}
	for _, root := range roots[id] {
		if err := p.contents.Release(ctx, root); err != nil {
			return err
		}
	}
	return nil
}

func (p *Project) Entries(ctx context.Context, id string, selection model.Selection) ([]model.Entry, error) {
	if err := selection.Validate(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	entries, err := p.inventory(ctx, id, selection)
	result := make([]model.Entry, 0, len(entries))
	for _, e := range entries {
		result = append(result, e.Entry)
	}
	return result, err
}

// Read holds the shared gate until the consumer finishes; maintenance and
// release will use the same exclusive gate as publication.
func (p *Project) Read(ctx context.Context, id, name string, consume func(model.Entry, io.Reader) error) error {
	if !model.ValidPath(name) {
		return ErrInvalid
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	entries, err := p.inventory(ctx, id, model.Selection{{Path: name, Include: true}})
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Path == name && e.Kind == model.File {
			r, err := p.contents.Read(ctx, e.Object)
			if err != nil {
				return err
			}
			defer r.Close()
			return consume(e.Entry, r)
		}
	}
	return ErrNotFound
}

func (p *Project) History(ctx context.Context, branch string, limit int) ([]Commit, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	id, err := p.head(ctx, branch)
	if err != nil {
		return nil, err
	}
	result := []Commit{}
	for id != "" && len(result) < limit {
		c, err := p.commit(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
		id = ""
		if len(c.Parents) > 0 {
			id = c.Parents[0]
		}
	}
	return result, nil
}
