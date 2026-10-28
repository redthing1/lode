package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"github.com/redthing1/lode/internal/remote"
	"golang.org/x/term"
)

type credential struct {
	Token    string `json:"token"`
	Instance string `json:"instance"`
	User     string `json:"user"`
}
type credentialFile struct {
	Credentials map[string]credential `json:"credentials"`
}

func credentialPaths() (string, string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(base, "lode")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("credential directory is not a private directory")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "credentials.json"), filepath.Join(dir, "credentials.lock"), nil
}

func updateCredentials(edit func(map[string]credential) error) error {
	path, lockPath, err := credentialPaths()
	if err != nil {
		return err
	}
	lock := flock.New(lockPath)
	if li, le := os.Lstat(lockPath); le == nil && (!li.Mode().IsRegular() || li.Mode()&os.ModeSymlink != 0) {
		return errors.New("credential lock is not a regular file")
	} else if le != nil && !errors.Is(le, os.ErrNotExist) {
		return le
	}
	if err = lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	items := map[string]credential{}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("credential file is not a regular file")
		}
		if info.Mode().Perm()&0077 != 0 {
			return errors.New("credential file permissions must be 0600")
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		var saved credentialFile
		if e = json.Unmarshal(data, &saved); e != nil {
			return fmt.Errorf("read credentials: %w", e)
		}
		if saved.Credentials != nil {
			items = saved.Credentials
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := edit(items); err != nil {
		return err
	}
	data, err := json.MarshalIndent(credentialFile{Credentials: items}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), "credentials-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	return err
}

func credentialKey(raw string) (string, error) { return remote.CanonicalURL(raw) }
func originKey(raw string) (string, error) {
	key, err := remote.CanonicalURL(raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(key)
	if err != nil {
		return "", err
	}
	u.Path = ""
	return strings.TrimSuffix(u.String(), "/"), nil
}

func findCredential(raw string) (credential, bool, error) {
	key, err := credentialKey(raw)
	if err != nil {
		return credential{}, false, err
	}
	origin, err := originKey(raw)
	if err != nil {
		return credential{}, false, err
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return credential{}, false, err
	}
	dir := filepath.Join(base, "lode")
	di, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return credential{}, false, nil
	}
	if err != nil {
		return credential{}, false, err
	}
	if !di.IsDir() || di.Mode()&os.ModeSymlink != 0 {
		return credential{}, false, errors.New("credential directory is not a private directory")
	}
	path := filepath.Join(dir, "credentials.json")
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return credential{}, false, nil
	}
	if err != nil {
		return credential{}, false, err
	}
	if !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
		return credential{}, false, errors.New("credential file is not a regular file")
	}
	if fi.Mode().Perm()&0077 != 0 {
		return credential{}, false, errors.New("credential file permissions must be 0600")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return credential{}, false, err
	}
	var saved credentialFile
	if err = json.Unmarshal(b, &saved); err != nil {
		return credential{}, false, fmt.Errorf("read credentials: %w", err)
	}
	c, ok := saved.Credentials[key]
	if !ok && origin != key {
		c, ok = saved.Credentials[origin]
	}
	return c, ok, nil
}

func verifiedCredential(ctx context.Context, raw string) (credential, bool, error) {
	c, ok, err := findCredential(raw)
	if err != nil || !ok {
		return c, ok, err
	}
	if c.User == "" || c.Instance == "" {
		return credential{}, false, errors.New("saved credential lacks identity metadata; run lode login again")
	}
	identity, err := remote.WhoAmI(ctx, raw, c.Token)
	if err != nil {
		return credential{}, false, err
	}
	if identity.ID != c.User || identity.Instance != c.Instance {
		return credential{}, false, errors.New("saved credential resolved to a different account or instance; run lode login again")
	}
	return c, true, nil
}

func saveCredential(key string, c credential) error {
	return updateCredentials(func(items map[string]credential) error { items[key] = c; return nil })
}
func removeCredential(key string) error {
	return updateCredentials(func(items map[string]credential) error { delete(items, key); return nil })
}

func inputToken(in io.Reader, out io.Writer) (string, error) {
	if token := os.Getenv("LODE_TOKEN"); token != "" {
		return strings.TrimSpace(token), nil
	}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(out, "Access token: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		return strings.TrimSpace(string(b)), err
	}
	fmt.Fprint(out, "Access token: ")
	b, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(b), nil
}

func login(ctx context.Context, raw string, in io.Reader, out io.Writer) error {
	key, err := credentialKey(raw)
	if err != nil {
		return err
	}
	token, err := inputToken(in, out)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("empty access token")
	}
	identity, err := remote.WhoAmI(ctx, key, token)
	if err != nil {
		return err
	}
	if remote.HasProjectPath(key) {
		c, err := remote.New(key, token, "")
		if err != nil {
			return err
		}
		if _, err = c.Info(ctx); err != nil {
			return fmt.Errorf("project access check: %w", err)
		}
	}
	if err := saveCredential(key, credential{Token: token, Instance: identity.Instance, User: identity.ID}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in as %s at %s.\n", identity.Username, key)
	return nil
}

func logout(raw string, out io.Writer) error {
	key, err := credentialKey(raw)
	if err != nil {
		return err
	}
	if err := removeCredential(key); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged out from %s.\n", key)
	return nil
}
