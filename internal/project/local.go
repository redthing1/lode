package project

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/gofrs/flock"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"

	"github.com/redthing1/lode/internal/engine/kopia"
)

const localStateVersion = 1

var errLocalUnsupported = errors.New("local projects require Linux local filesystem durability")

type localState struct {
	Version  int    `json:"version"`
	Password string `json:"repositoryPassword"`
}

// Local owns an on-disk project from before storage opens until after all
// storage closes. Close stops service operations before closing its resources.
type Local struct {
	Service *Service
	Token   string

	metadata    *Metadata
	repository  repo.DirectRepository
	runtimeLock string
	owner       *flock.Flock
	once        sync.Once
	closeErr    error
}

// InitializeLocal atomically creates a dedicated project directory. It does
// not overwrite an existing destination and publishes only a fully initialized
// native content repository plus private credentials and SQLite metadata.
func InitializeLocal(ctx context.Context, directory string) error {
	if runtime.GOOS != "linux" {
		return errLocalUnsupported
	}
	if directory == "" {
		return errors.New("initialize local project: empty directory")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve project directory: %w", err)
	}
	parent := filepath.Dir(absolute)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("stat project parent: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("project parent %q is not a directory", parent)
	}
	if _, err := os.Lstat(absolute); err == nil {
		return fmt.Errorf("project destination already exists: %q", absolute)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect project destination: %w", err)
	}

	password, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate native repository password: %w", err)
	}
	token, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate API token: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".lode-project-init-")
	if err != nil {
		return fmt.Errorf("create project staging directory: %w", err)
	}
	if err := os.Chmod(stage, 0700); err != nil {
		_ = os.RemoveAll(stage)
		return fmt.Errorf("secure project staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()

	storage, err := kopia.NewLocalStorage(ctx, filepath.Join(stage, "content"), true)
	if err != nil {
		return fmt.Errorf("create native content storage: %w", err)
	}
	initErr := repo.Initialize(ctx, storage, nil, password)
	closeStorageErr := storage.Close(ctx)
	if initErr != nil {
		return fmt.Errorf("initialize native content repository: %w", initErr)
	}
	if closeStorageErr != nil {
		return fmt.Errorf("close native content storage after initialization: %w", closeStorageErr)
	}

	metadata, err := OpenMetadata(filepath.Join(stage, "metadata.sqlite"))
	if err != nil {
		return fmt.Errorf("initialize project metadata: %w", err)
	}
	metadataErr := metadata.Close()
	if chmodErr := os.Chmod(filepath.Join(stage, "metadata.sqlite"), 0600); chmodErr != nil {
		return fmt.Errorf("secure project metadata: %w", chmodErr)
	}
	if metadataErr != nil {
		return fmt.Errorf("close initialized project metadata: %w", metadataErr)
	}
	stateBytes, err := json.Marshal(localState{Version: localStateVersion, Password: password})
	if err != nil {
		return fmt.Errorf("encode local project credentials: %w", err)
	}
	if err := writePrivateFile(filepath.Join(stage, "state.json"), append(stateBytes, '\n')); err != nil {
		return fmt.Errorf("write local project credentials: %w", err)
	}
	if err := writePrivateFile(filepath.Join(stage, "token"), []byte(token+"\n")); err != nil {
		return fmt.Errorf("write local API token: %w", err)
	}
	if err := syncLocalProjectDir(stage); err != nil {
		return fmt.Errorf("sync initialized project directory: %w", err)
	}
	if err := renameNoReplace(stage, absolute); err != nil {
		return fmt.Errorf("publish initialized project directory: %w", err)
	}
	published = true
	if err := syncLocalProjectDir(parent); err != nil {
		return fmt.Errorf("sync project parent after publication: %w", err)
	}
	return nil
}

// OpenLocal reconstructs a disposable Kopia connection config for this
// project's real content path, then opens the native repository and SQLite
// history and establishes exclusive project service ownership.
func OpenLocal(ctx context.Context, directory string) (*Local, error) {
	if runtime.GOOS != "linux" {
		return nil, errLocalUnsupported
	}
	if directory == "" {
		return nil, errors.New("open local project: empty directory")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve project directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect project directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local project directory must be a private directory")
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve local project path: %w", err)
	}
	metadataPath := filepath.Join(absolute, "metadata.sqlite")
	owner, err := acquireProjectOwner(metadataPath)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	stateBytes, err := readPrivateFile(filepath.Join(absolute, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("read local project credentials: %w", err)
	}
	var state localState
	decoder := json.NewDecoder(bytes.NewReader(stateBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode local project credentials: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("decode local project credentials: trailing data")
	}
	passwordBytes, err := hex.DecodeString(state.Password)
	if err != nil || state.Version != localStateVersion || len(passwordBytes) != 32 || strings.ToLower(state.Password) != state.Password {
		return nil, errors.New("local project credentials have an unsupported or invalid format")
	}
	tokenBytes, err := readPrivateFile(filepath.Join(absolute, "token"))
	if err != nil {
		return nil, fmt.Errorf("read local API token: %w", err)
	}
	if len(tokenBytes) != 65 || tokenBytes[64] != '\n' {
		return nil, errors.New("local API token has an invalid format")
	}
	token := string(tokenBytes[:64])
	tokenRaw, err := hex.DecodeString(token)
	if err != nil || len(tokenRaw) != 32 || strings.ToLower(token) != token {
		return nil, errors.New("local API token has an invalid format")
	}

	contentPath := filepath.Join(absolute, "content")
	storage, err := kopia.NewLocalStorage(ctx, contentPath, false)
	if err != nil {
		return nil, fmt.Errorf("open local content storage: %w", err)
	}
	configNonce, err := randomHex(16)
	if err != nil {
		_ = storage.Close(ctx)
		return nil, fmt.Errorf("generate disposable native config name: %w", err)
	}
	configPath := filepath.Join(absolute, ".connect-runtime-"+configNonce+".json")
	connectErr := repo.Connect(ctx, configPath, storage, state.Password, &repo.ConnectOptions{
		// The project service is the native maintenance identity. A local store
		// remains usable after moving to another host or operating-system user.
		ClientOptions:  repo.ClientOptions{Username: "lode", Hostname: "local"},
		CachingOptions: content.CachingOptions{},
	})
	storageCloseErr := storage.Close(ctx)
	if connectErr != nil {
		_ = os.Remove(configPath)
		return nil, fmt.Errorf("regenerate native repository connection: %w", connectErr)
	}
	if storageCloseErr != nil {
		_ = os.Remove(configPath)
		return nil, fmt.Errorf("close native storage after connection: %w", storageCloseErr)
	}
	native, err := repo.Open(ctx, configPath, state.Password, nil)
	removeConfigErr := os.Remove(configPath)
	if err != nil {
		return nil, fmt.Errorf("open native content repository: %w", err)
	}
	if removeConfigErr != nil {
		_ = native.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("remove disposable native connection config: %w", removeConfigErr)
	}
	direct, ok := native.(repo.DirectRepository)
	if !ok {
		_ = native.Close(context.WithoutCancel(ctx))
		return nil, errors.New("opened native repository does not support direct local access")
	}

	metadataInfo, err := os.Lstat(metadataPath)
	if err != nil || !metadataInfo.Mode().IsRegular() {
		_ = direct.Close(context.WithoutCancel(ctx))
		if err != nil {
			return nil, fmt.Errorf("inspect project metadata: %w", err)
		}
		return nil, errors.New("project metadata must be a regular file")
	}
	metadata, err := openMetadata(metadataPath, owner)
	if err != nil {
		_ = direct.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("open project metadata: %w", err)
	}
	if err := os.Chmod(metadataPath, 0600); err != nil {
		_ = metadata.Close()
		_ = direct.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("secure project metadata: %w", err)
	}
	roots, err := kopia.NewRoots(direct)
	if err != nil {
		_ = metadata.Close()
		_ = direct.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("open native protection roots: %w", err)
	}
	objects, err := kopia.NewObjects(direct)
	if err != nil {
		_ = metadata.Close()
		_ = direct.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("open native object adapter: %w", err)
	}
	maintenance, err := kopia.NewMaintenance(direct)
	if err != nil {
		_ = metadata.Close()
		_ = direct.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("open native maintenance adapter: %w", err)
	}
	service, err := newService(ctx, metadata, roots, Options{Objects: objects, Maintenance: maintenance}, owner)
	if err != nil {
		_ = metadata.Close()
		_ = direct.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	transferred = true
	return &Local{Service: service, Token: token, metadata: metadata, repository: direct, runtimeLock: configPath + ".mlock", owner: owner}, nil
}

func (l *Local) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		serviceErr := l.Service.Close()
		metadataErr := l.metadata.Close()
		var repositoryErr error
		if l.repository != nil {
			repositoryErr = l.repository.Close(context.Background())
		}
		var lockErr error
		if l.runtimeLock != "" {
			lockErr = os.Remove(l.runtimeLock)
			if errors.Is(lockErr, os.ErrNotExist) {
				lockErr = nil
			}
		}
		var ownerErr error
		if l.owner != nil {
			ownerErr = l.owner.Close()
		}
		l.closeErr = errors.Join(serviceErr, metadataErr, repositoryErr, lockErr, ownerErr)
	})
	return l.closeErr
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func writePrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func readPrivateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%q must be a private regular file", path)
	}
	return os.ReadFile(path)
}

func syncLocalProjectDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}
