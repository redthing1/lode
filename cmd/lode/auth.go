package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/workspace"
	"golang.org/x/term"
)

func authCommand(ctx context.Context, args []string, directory string, out io.Writer) error {
	const usage = "Usage: lode auth <login | status | logout> [options] [server-or-project-url]\nOmit the URL inside a workspace. Status outside a workspace lists saved accounts."
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(out, usage)
		return nil
	}
	action := args[0]
	if action != "login" && action != "status" && action != "logout" {
		return errors.New(usage)
	}
	f := flags("auth "+action, "Usage: lode auth "+action+" [options] [server-or-project-url]", out)
	var allowHTTP, all bool
	if action == "login" {
		f.BoolVar(&allowHTTP, "allow-http", false, "trust HTTP for this origin on a protected private network")
	}
	if action == "status" {
		f.BoolVar(&all, "all", false, "list all saved accounts without contacting servers")
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() > 1 || all && f.NArg() != 0 {
		return errors.New("supply at most one server or project URL")
	}
	if all {
		return listAccounts(ctx, out)
	}
	address := f.Arg(0)
	var state workspace.State
	if address == "" {
		w, err := workspace.Open(directory)
		if err != nil {
			if action == "status" && errors.Is(err, workspace.ErrNotWorkspace) {
				return listAccounts(ctx, out)
			}
			return fmt.Errorf("supply a server or project URL: %w", err)
		}
		state = w.State()
		w.Close()
		address = projectAddress(state)
	}
	origin, project, err := authTarget(address)
	if err != nil {
		return err
	}
	switch action {
	case "login":
		return authLogin(ctx, origin, project, state, allowHTTP, out)
	case "logout":
		if os.Getenv("LODE_TOKEN") != "" {
			return errors.New("LODE_TOKEN is set; unset it before logging out saved credentials")
		}
		var username string
		if err := updateCredentials(origin, func(store *credentialStore) error {
			if state.InstanceID != "" && store.Instance != "" && store.Instance != state.InstanceID {
				return errors.New("saved credentials belong to another instance")
			}
			id, account, err := store.account(project, selectedUser(ctx))
			if err != nil {
				return err
			}
			delete(store.Accounts, id)
			// Keep the account choices so other projects never fall back to a
			// different identity after this account's credentials are forgotten.
			username = account.Username
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(out, "Signed out %s from %s.\n", username, origin)
		return nil
	default:
		cred, err := credentials(ctx, address)
		if err != nil {
			return err
		}
		if state.InstanceID != "" && cred.Instance != "" && state.InstanceID != cred.Instance {
			return errors.New("saved credentials belong to another instance")
		}
		c, err := remote.NewWithHTTP(origin, cred.Token, cred.AllowHTTP)
		if err != nil {
			return err
		}
		defer c.HTTP.CloseIdleConnections()
		c.InstanceID = cred.Instance
		if state.InstanceID != "" {
			c.InstanceID = state.InstanceID
		}
		identity, err := c.Me(ctx)
		if err != nil {
			if cred.Username != "" {
				fmt.Fprintf(out, "%s @ %s (saved; verification failed)\n", cred.Username, origin)
			}
			return err
		}
		if err := checkIdentity(identity); err != nil {
			return err
		}
		if cred.User != "" && cred.User != identity.User {
			return errors.New("saved token belongs to another account; sign in again")
		}
		fmt.Fprintf(out, "%s @ %s (verified; %s)\n", identity.Username, origin, cred.Source)
		if project != "" {
			fmt.Fprintln(out, "Project:", project)
		}
		return nil
	}
}

func authLogin(ctx context.Context, origin, project string, state workspace.State, allowHTTP bool, out io.Writer) error {
	store, err := readCredentials(origin)
	if err != nil {
		return err
	}
	allowHTTP = allowHTTP || store.AllowHTTP
	if _, err := originOf(origin, allowHTTP); err != nil {
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
	c, err := remote.NewWithHTTP(origin, token, allowHTTP)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	c.InstanceID = state.InstanceID
	identity, err := c.Me(ctx)
	if err != nil {
		return err
	}
	if err := checkIdentity(identity); err != nil {
		return err
	}
	if user := selectedUser(ctx); user != "" && user != identity.Username {
		return errors.New("token belongs to a different user")
	}
	if project != "" {
		parts := strings.Split(project, "/")
		c.InstanceID, c.ProjectID = identity.Instance, state.ProjectID
		if _, err := c.Project(ctx, parts[0], parts[1]); err != nil {
			return err
		}
	}
	if err := updateCredentials(origin, func(store *credentialStore) error {
		if store.Instance != "" && store.Instance != identity.Instance {
			store.Accounts, store.Selected = map[string]savedAccount{}, map[string]string{}
		}
		store.Instance = identity.Instance
		store.AllowHTTP = allowHTTP && strings.HasPrefix(origin, "http://")
		account := store.Accounts[identity.User]
		account.Username = identity.Username
		if account.Tokens == nil {
			account.Tokens = map[string]string{}
		}
		account.Tokens[project] = token
		for scope, saved := range account.Tokens {
			if scope != "" && saved == account.Tokens[""] {
				delete(account.Tokens, scope)
			}
		}
		store.Accounts[identity.User] = account
		store.Selected[project] = identity.User
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Signed in to %s as %s.\n", origin+scopeSuffix(project), identity.Username)
	return nil
}

func listAccounts(ctx context.Context, out io.Writer) error {
	if os.Getenv("LODE_TOKEN") != "" {
		fmt.Fprintln(out, "LODE_TOKEN is set; supply a URL to verify its account.")
	}
	file, err := credentialPath("")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(out, "No saved accounts.")
		return nil
	}
	if err != nil {
		return err
	}
	var rows []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || entry.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(filepath.Dir(file), entry.Name()))
		if err != nil {
			return err
		}
		var header struct{ Origin string }
		if err := json.Unmarshal(b, &header); err != nil {
			return errors.New("invalid saved credentials")
		}
		origin, err := originOf(header.Origin, true)
		if err != nil || origin != header.Origin {
			return errors.New("invalid saved origin")
		}
		expected, err := credentialPath(origin)
		if err != nil {
			return err
		}
		if filepath.Base(expected) != entry.Name() {
			return errors.New("invalid saved credential filename")
		}
		store, err := readCredentials(origin)
		if err != nil {
			return err
		}
		for id, account := range store.Accounts {
			if user := selectedUser(ctx); user != "" && user != account.Username {
				continue
			}
			for project := range account.Tokens {
				scope, use := project, ""
				if scope == "" {
					scope = "server"
				}
				if store.Selected[project] == id {
					use = "selected"
				}
				rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\n", origin, account.Username, scope, use))
			}
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "No saved accounts.")
		return nil
	}
	slices.Sort(rows)
	fmt.Fprintln(out, "Saved accounts:")
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tUSER\tSCOPE\tUSE")
	for _, row := range rows {
		fmt.Fprint(w, row)
	}
	return w.Flush()
}
