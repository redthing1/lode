package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/workspace"
)

const usage = `Lode — version large binary projects

Usage: lode [-C workspace] COMMAND [options]

  init DIR                     Create a server project; print its access token
  serve [-listen HOST:PORT] DIR Serve a project on a loopback address
  attach [-include PATH] URL [DIR]  Track a folder in an empty project
  clone [-include PATH] URL DIR    Download a selected workspace
  status                       Show selected local changes
  save -m MESSAGE [-author NAME]   Save selected changes
  update [-ours PATH | -theirs PATH]  Update while preserving local edits
  select [-include PATH] [-exclude PATH] [-none]  Inspect or replace selection
  log [-n COUNT]               List versions on the workspace branch
  resume                       Finish an interrupted operation
  cancel                       Cancel a save or roll back a local transition

Use LODE_TOKEN for attach/clone and LODE_AUTHOR for the default author.
Repeat -include to select multiple paths; omission selects the whole project.
Options precede positional arguments.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "lode:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	global := flag.NewFlagSet("lode", flag.ContinueOnError)
	global.SetOutput(diagnostics)
	directory := global.String("C", ".", "workspace directory")
	global.Usage = func() { fmt.Fprint(diagnostics, usage) }
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = global.Args()
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(out, usage)
		return nil
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	var selection model.Selection
	var choices []model.Choice
	var emptySelection bool
	var message, author, listen string
	limit := 20
	switch command {
	case "attach", "clone", "select":
		addRule := func(include bool) func(string) error {
			return func(p string) error {
				if p == "." {
					p = ""
				}
				selection = append(selection, model.Rule{Path: p, Include: include})
				return selection.Validate()
			}
		}
		flags.Func("include", "select a file or directory (repeatable)", addRule(true))
		flags.Func("exclude", "exclude a file or directory (repeatable)", addRule(false))
		if command == "select" {
			flags.BoolVar(&emptySelection, "none", false, "unload all selected paths")
		}
	case "update":
		addChoice := func(ours bool) func(string) error {
			return func(p string) error {
				if p == "." {
					p = ""
				}
				if p != "" && !model.ValidPath(p) {
					return errors.New("invalid choice path")
				}
				choices = append(choices, model.Choice{Path: p, Ours: ours})
				return nil
			}
		}
		flags.Func("ours", "keep the local entry/subtree (repeatable)", addChoice(true))
		flags.Func("theirs", "take the remote entry/subtree (repeatable)", addChoice(false))
	case "save":
		flags.StringVar(&message, "m", "", "version message")
		flags.StringVar(&author, "author", os.Getenv("LODE_AUTHOR"), "author name")
	case "serve":
		flags.StringVar(&listen, "listen", "127.0.0.1:8473", "loopback listen address")
	case "log":
		flags.IntVar(&limit, "n", 20, "maximum versions")
	case "init", "status", "resume", "cancel":
	default:
		return fmt.Errorf("unknown command %q; use lode help", command)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = flags.Args()
	if emptySelection && len(selection) > 0 {
		return errors.New("-none cannot be combined with selection rules")
	}
	if len(selection) > 0 {
		hasInclude, hasRoot := false, false
		for _, r := range selection {
			hasInclude = hasInclude || r.Include
			hasRoot = hasRoot || r.Path == ""
		}
		if !hasInclude && !hasRoot {
			selection = append(model.Selection{{Include: true}}, selection...)
		}
	}
	switch command {
	case "init", "serve":
		if len(args) != 1 {
			return fmt.Errorf("usage: lode %s [options] DIR", command)
		}
		if command == "serve" {
			return serve(ctx, args[0], listen, diagnostics)
		}
		p, err := project.Create(ctx, args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Project: %s\nToken: %s\n", p.ID(), p.Token())
		return p.Close()
	case "attach", "clone":
		if len(args) < 1 || len(args) > 2 || (command == "clone" && len(args) != 2) {
			return fmt.Errorf("usage: lode %s [-include PATH] URL DIR", command)
		}
		target := *directory
		if len(args) == 2 {
			target = args[1]
		}
		token := os.Getenv("LODE_TOKEN")
		if token == "" {
			return errors.New("set LODE_TOKEN to the project's access token")
		}
		var w *workspace.Workspace
		var err error
		if command == "attach" {
			w, err = workspace.Attach(ctx, args[0], token, target, selection)
		} else {
			w, err = workspace.Clone(ctx, args[0], token, target, selection)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Workspace: %s\n", target)
		return w.Close()
	}
	if len(args) != 0 {
		return fmt.Errorf("unexpected arguments for %s", command)
	}
	if command == "save" && message == "" {
		return errors.New("save requires -m MESSAGE")
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return err
	}
	defer w.Close()
	switch command {
	case "status":
		changes, err := w.Status()
		if err != nil {
			return err
		}
		previous := map[string]bool{}
		for _, e := range w.State().Baseline {
			previous[e.Path] = true
		}
		for _, e := range changes {
			label := "A"
			if previous[e.Path] {
				label = "M"
			}
			if e.Kind == model.Absent {
				label = "D"
			}
			fmt.Fprintf(out, "%s %s\n", label, e.Path)
		}
		if len(changes) == 0 {
			fmt.Fprintln(out, "No selected changes.")
		}
	case "save":
		commit, err := w.Save(ctx, author, message)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, commit.ID)
	case "update":
		if err := w.Update(ctx, choices); err != nil {
			return err
		}
		fmt.Fprintln(out, "Workspace updated.")
	case "select":
		if selection == nil && !emptySelection {
			for _, r := range w.State().Selection {
				prefix, p := "-", r.Path
				if r.Include {
					prefix = "+"
				}
				if p == "" {
					p = "."
				}
				fmt.Fprintf(out, "%s %s\n", prefix, p)
			}
			return nil
		}
		if emptySelection {
			selection = model.Selection{}
		}
		if err := w.Select(ctx, selection); err != nil {
			return err
		}
		fmt.Fprintln(out, "Workspace selection updated.")
	case "resume":
		if err := w.Resume(ctx); err != nil {
			return err
		}
		fmt.Fprintln(out, "Operation completed.")
	case "cancel":
		if err := w.Cancel(ctx); err != nil {
			return err
		}
		fmt.Fprintln(out, "Operation cancelled.")
	case "log":
		state := w.State()
		client, err := remote.New(state.URL, state.Token, state.Project)
		if err != nil {
			return err
		}
		commits, err := client.History(ctx, state.Branch, limit)
		if err != nil {
			return err
		}
		for _, c := range commits {
			fmt.Fprintf(out, "%s  %s  %s\n", c.ID, c.Created.Format(time.RFC3339), c.Message)
		}
	}
	return nil
}

func serve(ctx context.Context, directory, address string, out io.Writer) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("serve requires a loopback address; use a TLS reverse proxy for remote access")
	}
	p, err := project.Open(ctx, directory)
	if err != nil {
		return err
	}
	defer p.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: remote.Handler(p), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx }}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	fmt.Fprintf(out, "Serving on %s\n", listener.Addr())
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
