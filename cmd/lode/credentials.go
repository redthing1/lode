package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/workspace"
)

type credential struct {
	Origin, Instance, User, Username, Token, Source string
	AllowHTTP                                       bool
}

type savedAccount struct {
	Username string            `json:"username"`
	Tokens   map[string]string `json:"tokens"`
}

type credentialStore struct {
	Origin    string                  `json:"origin"`
	Instance  string                  `json:"instance"`
	AllowHTTP bool                    `json:"allow_http,omitempty"`
	Accounts  map[string]savedAccount `json:"accounts"`
	Selected  map[string]string       `json:"selected"`
}

type authUserKey struct{}

func selectedUser(ctx context.Context) string {
	user, _ := ctx.Value(authUserKey{}).(string)
	return user
}

func originOf(address string, allowHTTP bool) (string, error) {
	u, err := url.Parse(address)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("use a server or project URL")
	}
	u.Path, u.RawPath = "", ""
	c, err := remote.NewWithHTTP(u.String(), "", allowHTTP)
	if err != nil {
		return "", err
	}
	defer c.HTTP.CloseIdleConnections()
	return c.Origin(), nil
}

func authTarget(address string) (origin, project string, err error) {
	origin, err = originOf(address, true)
	if err != nil {
		return "", "", err
	}
	u, _ := url.Parse(address)
	project = strings.Trim(u.Path, "/")
	if !validAuthScope(project) {
		return "", "", errors.New("use a server URL or a project URL ending in /owner/project")
	}
	return origin, project, nil
}

func validAuthScope(project string) bool {
	if project == "" {
		return true
	}
	parts := strings.Split(project, "/")
	return len(parts) == 2 && model.ValidPath(parts[0]) && model.ValidPath(parts[1])
}

func scopeSuffix(project string) string {
	if project == "" {
		return ""
	}
	parts := strings.Split(project, "/")
	return "/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
}

func projectAddress(s workspace.State) string {
	return s.Server + scopeSuffix(s.Owner+"/"+s.Project)
}

func credentialPath(origin string) (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(origin))
	return filepath.Join(root, "lode", "credentials", hex.EncodeToString(h[:])+".json"), nil
}

func readCredentials(origin string) (credentialStore, error) {
	store := credentialStore{Origin: origin, Accounts: map[string]savedAccount{}, Selected: map[string]string{}}
	file, err := credentialPath(origin)
	if err != nil {
		return store, err
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return store, err
	}
	if err := json.Unmarshal(b, &store); err != nil || store.Origin != origin || store.Instance == "" || store.Accounts == nil || store.Selected == nil {
		return store, errors.New("invalid saved credentials")
	}
	for id, account := range store.Accounts {
		if id == "" || !model.ValidPath(account.Username) || strings.Contains(account.Username, "/") || len(account.Tokens) == 0 {
			return store, errors.New("invalid saved account")
		}
		for project, token := range account.Tokens {
			if token == "" || len(token) > 512 {
				return store, errors.New("invalid saved token")
			}
			if !validAuthScope(project) {
				return store, errors.New("invalid saved project")
			}
		}
	}
	for project, id := range store.Selected {
		if id == "" {
			return store, errors.New("invalid saved account selection")
		}
		if !validAuthScope(project) {
			return store, errors.New("invalid saved project selection")
		}
	}
	return store, nil
}

func updateCredentials(origin string, update func(*credentialStore) error) error {
	file, err := credentialPath(origin)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	lock := flock.New(file + ".lock")
	defer lock.Close()
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("credentials are in use; retry the command")
	}
	store, err := readCredentials(origin)
	if err != nil {
		return err
	}
	if err := update(&store); err != nil {
		return err
	}
	b, err := json.Marshal(store)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".auth-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

func (s credentialStore) account(project, username string) (string, savedAccount, error) {
	id, bound := s.Selected[project]
	if !bound {
		id = s.Selected[""]
	}
	if username != "" {
		id = ""
		for candidate, account := range s.Accounts {
			if account.Username == username {
				if id != "" {
					return "", savedAccount{}, errors.New("ambiguous saved account; sign in again")
				}
				id = candidate
			}
		}
	}
	account, ok := s.Accounts[id]
	if !ok {
		return "", savedAccount{}, fmt.Errorf("sign in first: lode auth login %s", s.Origin+scopeSuffix(project))
	}
	return id, account, nil
}

func credentials(ctx context.Context, address string) (credential, error) {
	origin, project, err := authTarget(address)
	if err != nil {
		return credential{}, err
	}
	store, err := readCredentials(origin)
	if err != nil {
		return credential{}, err
	}
	c := credential{Origin: origin, Instance: store.Instance, AllowHTTP: store.AllowHTTP}
	if override := os.Getenv("LODE_TOKEN"); override != "" {
		if selectedUser(ctx) != "" {
			return credential{}, errors.New("use --user or LODE_TOKEN, not both")
		}
		c.Token, c.Source = override, "LODE_TOKEN"
		return c, nil
	}
	id, account, err := store.account(project, selectedUser(ctx))
	if err != nil {
		return credential{}, err
	}
	c.User, c.Username, c.Source = id, account.Username, "server"
	c.Token = account.Tokens[project]
	if project != "" && c.Token != "" {
		c.Source = "project"
	} else {
		c.Token = account.Tokens[""]
	}
	if c.Token == "" {
		return credential{}, fmt.Errorf("no saved token for %s here; run lode auth login %s", c.Username, address)
	}
	if selectedUser(ctx) != "" {
		c.Source = "--user"
	}
	return c, nil
}

func workspaceCredentials(ctx context.Context, s workspace.State) (credential, error) {
	c, err := credentials(ctx, projectAddress(s))
	if err == nil && c.Instance != "" && c.Instance != s.InstanceID {
		err = errors.New("saved credentials belong to another instance; check this workspace's server")
	}
	return c, err
}

func authenticated(ctx context.Context, address string) (*remote.Client, error) {
	cred, err := credentials(ctx, address)
	if err != nil {
		return nil, err
	}
	c, err := remote.NewWithHTTP(cred.Origin, cred.Token, cred.AllowHTTP)
	if err != nil {
		return nil, err
	}
	c.InstanceID = cred.Instance
	return c, nil
}

func checkIdentity(identity remote.Identity) error {
	if identity.Instance == "" || identity.User == "" || !model.ValidPath(identity.Username) || strings.Contains(identity.Username, "/") {
		return errors.New("server returned an invalid identity")
	}
	return nil
}
