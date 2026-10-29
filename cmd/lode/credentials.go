package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/redthing1/lode/internal/remote"
	"golang.org/x/term"
)

type credential struct {
	Origin   string `json:"origin"`
	Instance string `json:"instance"`
	Token    string `json:"token"`
}

func originOf(address string) (string, error) {
	u, err := url.Parse(address)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("use a server or project URL")
	}
	u.Path, u.RawPath = "", ""
	c, err := remote.New(u.String(), "")
	if err != nil {
		return "", err
	}
	defer c.HTTP.CloseIdleConnections()
	return c.Origin(), nil
}

func credentialPath(origin string) (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(origin))
	return filepath.Join(root, "lode", "credentials", hex.EncodeToString(h[:])+".json"), nil
}

func credentials(origin string) (credential, error) {
	file, err := credentialPath(origin)
	if err != nil {
		return credential{}, err
	}
	c := credential{Origin: origin}
	b, err := os.ReadFile(file)
	if err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return credential{}, err
		}
		if c.Origin != origin || c.Instance == "" || c.Token == "" {
			return credential{}, errors.New("invalid saved credentials; run lode login")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return credential{}, err
	}
	if override := os.Getenv("LODE_TOKEN"); override != "" {
		c.Token = override
	}
	if c.Token == "" {
		return credential{}, fmt.Errorf("sign in first: lode login %s", origin)
	}
	return c, nil
}

func authenticated(origin string) (*remote.Client, error) {
	cred, err := credentials(origin)
	if err != nil {
		return nil, err
	}
	c, err := remote.New(origin, cred.Token)
	if err != nil {
		return nil, err
	}
	c.InstanceID = cred.Instance
	return c, nil
}

func login(ctx context.Context, args []string, out io.Writer) error {
	f := flags("login", "Usage: lode login <server-or-project-url>\nCreate an access token in account settings. LODE_TOKEN may supply the token.", out)
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 {
		return errors.New("usage: lode login <server-or-project-url>")
	}
	origin, err := originOf(f.Arg(0))
	if err != nil {
		return err
	}
	token := os.Getenv("LODE_TOKEN")
	if token == "" {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprint(out, "Access token: ")
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(out)
			if err != nil {
				return err
			}
			token = string(b)
		} else {
			b, err := bufio.NewReader(io.LimitReader(os.Stdin, 513)).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			token = b
		}
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 512 {
		return errors.New("invalid access token")
	}
	c, err := remote.New(origin, token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	identity, err := c.Me(ctx)
	if err != nil {
		return err
	}
	if identity.Instance == "" || identity.User == "" {
		return errors.New("server returned an invalid identity")
	}
	file, err := credentialPath(origin)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(credential{Origin: origin, Instance: identity.Instance, Token: token})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".login-")
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
	if err := os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	fmt.Fprintf(out, "Signed in to %s as %s.\n", origin, identity.Username)
	return nil
}
