// Package cli provides Lode's command-line interface.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transport"
	"github.com/redthing1/lode/internal/workspace"
)

const usage = `Lode versions large binary projects.

Usage:
  lode <command> [options]

Commands:
  init STORE                         Initialize a local project store
  backup STORE DESTINATION           Copy a stopped project to a fresh directory
  serve [--listen ADDRESS] STORE     Serve a project on loopback HTTP
  clone [options] URL FOLDER         Create a selected workspace
  attach [options] URL FOLDER        Attach an existing folder to an empty branch
  status [-C DIR]                    Show selected local changes
  commit [-C DIR] --author NAME -m MESSAGE
                                     Publish selected local changes
  resume [-C DIR]                    Resume an incomplete workspace operation
  abort [-C DIR]                     Abort an incomplete workspace operation
  log [-C DIR] [--limit N]           Show first-parent history
  update [-C DIR]                    Reconcile the workspace with its branch tip
  switch [-C DIR] BRANCH             Switch the workspace branch
  load [-C DIR] PATH...              Add paths to the workspace selection
  unload [-C DIR] PATH...            Remove clean paths from the selection
  restore [-C DIR] --from COMMIT PATH...
                                     Restore selected paths from history
  merge [-C DIR] [--preview] [--ours PATH | --theirs PATH ...] [--author NAME -m MESSAGE] BRANCH
  merge [-C DIR] [--preview] [--ours PATH | --theirs PATH ...] [--author NAME -m MESSAGE] --from COMMIT
                                     Merge another branch tip or exact commit
  branch create [-C DIR] [--from COMMIT] NAME
                                     Create a branch at a commit (default: workspace base)
  branch delete [-C DIR] NAME        Delete a branch
  branch list [-C DIR]               List branches
  tag create [-C DIR] [--from COMMIT] NAME
  tag delete [-C DIR] NAME
  tag list [-C DIR]                  Manage immutable retained version names
  retention show|set|preview|apply|why
                                     Review and apply payload retention policy
  storage status [-C DIR]            Show native storage measurements
  storage collect [-C DIR]           Run bounded physical collection
  storage capacity [-C DIR]          Show or set the reserved free-space floor
  help                               Show this help
  version                            Show the build version

Workspace options (clone and attach):
  --token-file FILE     Private file containing the API token (required)
  --branch NAME         Branch to use (default: main)
  --select PATH         Include this path and its subtree; repeat as needed

Choice options (update, switch, restore, resume, and merge):
  --ours PATH           Choose this side for a whole entry/subtree; repeat as needed
  --theirs PATH         Choose the other side for a whole entry/subtree; repeat as needed
  Use . to choose the project root. More specific paths override ancestor choices.

The server binds only to a numeric loopback address or localhost. Plain HTTP
is intended for local use or an authenticated SSH tunnel.
Place command options before positional arguments.
`

const tokenFileLimit = 4096

// Run executes a command and returns its process exit status.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	return RunContext(context.Background(), args, stdout, stderr, version)
}

// RunContext executes a command that observes ctx during long-running work.
func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	command := "help"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	var err error
	switch command {
	case "help", "-h", "--help":
		if len(args) != 0 {
			err = fmt.Errorf("%s takes no arguments", command)
		} else {
			_, err = io.WriteString(stdout, usage)
		}
	case "version", "--version":
		if len(args) != 0 {
			err = fmt.Errorf("%s takes no arguments", command)
		} else {
			_, err = fmt.Fprintf(stdout, "lode %s\n", version)
		}
	case "init":
		err = runInit(ctx, args, stdout, stderr)
	case "backup":
		err = runBackup(ctx, args, stdout, stderr)
	case "serve":
		err = runServe(ctx, args, stdout, stderr)
	case "clone":
		err = runAttachOrClone(ctx, "clone", args, false, stdout, stderr)
	case "attach":
		err = runAttachOrClone(ctx, "attach", args, true, stdout, stderr)
	case "status":
		err = runStatus(ctx, args, stdout, stderr)
	case "commit":
		err = runCommit(ctx, args, stdout, stderr)
	case "resume":
		err = runResume(ctx, args, stdout, stderr)
	case "abort":
		err = runAbort(ctx, args, stdout, stderr)
	case "log":
		err = runLog(ctx, args, stdout, stderr)
	case "update":
		err = runUpdate(ctx, args, stdout, stderr)
	case "switch":
		err = runSwitch(ctx, args, stdout, stderr)
	case "load":
		err = runSelection(ctx, "load", args, stdout, stderr)
	case "unload":
		err = runSelection(ctx, "unload", args, stdout, stderr)
	case "restore":
		err = runRestore(ctx, args, stdout, stderr)
	case "merge":
		err = runMerge(ctx, args, stdout, stderr)
	case "branch":
		err = runBranch(ctx, args, stdout, stderr)
	case "tag":
		err = runTag(ctx, args, stdout, stderr)
	case "retention":
		err = runRetention(ctx, args, stdout, stderr)
	case "storage":
		err = runStorage(ctx, args, stdout, stderr)
	default:
		return report(stderr, 2, "unknown command %q\nRun 'lode help' for usage.", command)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return report(stderr, 1, "%v", err)
	}
	return 0
}

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("init STORE", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("init requires exactly one STORE directory")
	}
	store := fs.Arg(0)
	if err := project.InitializeLocal(ctx, store); err != nil {
		return fmt.Errorf("initialize project: %w", err)
	}
	_, err := fmt.Fprintf(stdout, "initialized project at %s\nAPI token stored in %s/token\n", store, store)
	return err
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) (runErr error) {
	fs := newFlags("serve [--listen ADDRESS] STORE", stderr)
	listen := fs.String("listen", "127.0.0.1:8021", "loopback listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("serve requires exactly one STORE directory")
	}
	address, err := loopbackAddress(ctx, *listen)
	if err != nil {
		return err
	}
	local, err := project.OpenLocal(ctx, fs.Arg(0))
	if err != nil {
		return fmt.Errorf("open project: %w", err)
	}
	defer func() {
		if closeErr := local.Close(); closeErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close project: %w", closeErr))
		}
	}()
	if err := local.Service.Recover(ctx); err != nil {
		return fmt.Errorf("recover project operations: %w", err)
	}
	handler, err := transport.NewHandler(local.Service, local.Token)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	_, writeErr := fmt.Fprintf(stdout, "listening on %s\n", listener.Addr().String())
	if writeErr != nil {
		_ = listener.Close()
		return writeErr
	}
	gate := &handlerGate{next: handler}
	server := &http.Server{
		Handler:           gate,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	defer func() {
		_ = server.Close()
		gate.CloseAndWait()
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	select {
	case serveErr := <-serveDone:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", serveErr)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			_ = server.Close()
		}
		serveErr := <-serveDone
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return errors.Join(shutdownErr, serveErr)
		}
		return errors.Join(ctx.Err(), shutdownErr, serveErr)
	}
}

// handlerGate prevents service calls from starting once shutdown begins and
// tracks in-flight calls so the local project can stay open until they finish.
type handlerGate struct {
	mu      sync.Mutex
	closing bool
	active  sync.WaitGroup
	next    http.Handler
}

func (g *handlerGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	g.active.Add(1)
	g.mu.Unlock()
	defer g.active.Done()
	g.next.ServeHTTP(w, r)
}

func (g *handlerGate) CloseAndWait() {
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()
	g.active.Wait()
}

func loopbackAddress(ctx context.Context, address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: expected HOST:PORT", address)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return "", fmt.Errorf("invalid listen port %q", port)
	}
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		// Do not let resolver configuration turn localhost into a non-loopback bind.
		host = "127.0.0.1"
	} else {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("serve address must use a numeric loopback IP or localhost")
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		host = ip.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(portNumber)), nil
}

func runAttachOrClone(ctx context.Context, command string, args []string, adopt bool, stdout, stderr io.Writer) error {
	fs := newFlags(command+" [options] URL FOLDER", stderr)
	tokenPath := fs.String("token-file", "", "private file containing the API token")
	branch := fs.String("branch", "main", "branch name")
	var selected stringList
	fs.Var(&selected, "select", "include a path and its subtree (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("%s requires URL and FOLDER positional arguments", command)
	}
	if *tokenPath == "" {
		return errors.New("--token-file is required")
	}
	token, err := readToken(*tokenPath)
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}
	var selection []project.SelectionRule
	if len(selected) > 0 {
		var rules model.Selection
		for _, value := range selected {
			path, err := model.ParsePath(value)
			if err != nil {
				return fmt.Errorf("invalid --select path %q: %w", value, err)
			}
			rules = rules.Include(path)
		}
		for _, rule := range rules.Rules() {
			selection = append(selection, project.SelectionRule{Path: rule.Path.String(), Included: rule.Included})
		}
	}
	w, err := workspace.Create(ctx, workspace.Options{
		Directory: fs.Arg(1), Remote: fs.Arg(0), Token: token,
		Branch: *branch, Selection: selection, Adopt: adopt,
	})
	if err != nil {
		return fmt.Errorf("%s workspace: %w", command, err)
	}
	closeErr := w.Close()
	if closeErr != nil {
		return fmt.Errorf("close workspace: %w", closeErr)
	}
	_, err = fmt.Fprintf(stdout, "%s workspace %s on branch %s\n", command, fs.Arg(1), *branch)
	return err
}

func readToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("token must come from a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, tokenFileLimit+1))
	if err != nil {
		return "", err
	}
	if len(data) > tokenFileLimit {
		return "", fmt.Errorf("token file exceeds %d bytes", tokenFileLimit)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("token file is empty")
	}
	return token, nil
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("status [-C DIR]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("status takes no positional arguments")
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	if pending := w.Pending(); pending != "" {
		_, err := fmt.Fprintf(stdout, "incomplete %s operation; run 'lode resume -C %s'\n", pending, *directory)
		return err
	}
	changes, err := w.Status(ctx)
	if err != nil {
		return fmt.Errorf("read workspace status: %w", err)
	}
	if len(changes) == 0 {
		_, err = io.WriteString(stdout, "clean\n")
		return err
	}
	for _, change := range changes {
		kind := "directory"
		switch change.Kind {
		case model.File:
			kind = "file"
		case model.Absent:
			kind = "delete"
		}
		if _, err := fmt.Fprintf(stdout, "%s %q\n", kind, change.Path); err != nil {
			return err
		}
	}
	return nil
}

func runCommit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("commit [-C DIR] --author NAME -m MESSAGE", stderr)
	directory := fs.String("C", ".", "workspace directory")
	author := fs.String("author", "", "commit author")
	message := fs.String("m", "", "commit message")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("commit takes no positional arguments")
	}
	if strings.TrimSpace(*author) == "" || strings.TrimSpace(*message) == "" {
		return errors.New("commit requires non-empty --author and -m values")
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	commit, err := w.Commit(ctx, *author, *message)
	if err != nil {
		return fmt.Errorf("commit workspace: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "committed %s\n", commit.ID)
	return err
}

func runResume(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("resume [-C DIR]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	var ours, theirs stringList
	fs.Var(&ours, "ours", "choose current branch contents for a whole entry/subtree")
	fs.Var(&theirs, "theirs", "choose source commit contents for a whole entry/subtree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("resume takes no positional arguments")
	}
	choices, err := makeChoices(ours, theirs)
	if err != nil {
		return err
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	pending := w.Pending()
	commit, err := w.ResumeWithChoices(ctx, choices)
	if err != nil {
		return fmt.Errorf("resume workspace: %w", err)
	}
	if pending == "abandon" {
		_, err = fmt.Fprintf(stdout, "Stopped materialization; current files kept; recovery data: %s\n", w.RecoveryDirectory())
		return err
	}
	if pending == "checkout" {
		_, err = io.WriteString(stdout, "resumed checkout\n")
		return err
	}
	if pending == "transition" {
		_, err = io.WriteString(stdout, "completed workspace transition\n")
		return err
	}
	_, err = fmt.Fprintf(stdout, "resumed; committed %s\n", commit.ID)
	return err
}

func runAbort(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("abort [-C DIR]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("abort takes no positional arguments")
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	base := w.Base()
	pending := w.Pending()
	if err := w.Abort(ctx); err != nil {
		return fmt.Errorf("abort workspace operation: %w", err)
	}
	if pending == "checkout" || pending == "transition" || pending == "abandon" {
		_, err = fmt.Fprintf(stdout, "Stopped materialization; current files kept; recovery data: %s\n", w.RecoveryDirectory())
		return err
	}
	if w.Base() != base {
		_, err := fmt.Fprintf(stdout, "operation was already accepted; workspace advanced to %s\n", w.Base())
		return err
	}
	_, err = io.WriteString(stdout, "aborted incomplete operation\n")
	return err
}

func runLog(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("log [-C DIR] [--limit N]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	limit := fs.Int("limit", 20, "maximum commits to show (1–1000)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("log takes no positional arguments")
	}
	if *limit < 1 || *limit > 1000 {
		return errors.New("--limit must be between 1 and 1000")
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	commits, _, err := w.History(ctx, *limit)
	if err != nil {
		return fmt.Errorf("read history: %w", err)
	}
	if len(commits) == 0 {
		_, err = io.WriteString(stdout, "no commits\n")
		return err
	}
	ids := make([]string, 0, len(commits))
	for _, commit := range commits {
		ids = append(ids, commit.ID)
	}
	availability, err := w.PayloadStatuses(ctx, ids)
	if err != nil {
		return fmt.Errorf("read history availability: %w", err)
	}
	states := make(map[string]string, len(availability))
	for _, item := range availability {
		states[item.CommitID] = item.State
	}
	for _, commit := range commits {
		state := states[commit.ID]
		if state != "retained" && state != "expired" {
			return errors.New("remote returned invalid history availability")
		}
		if _, err := fmt.Fprintf(stdout, "%s %s %s %q %q\n", commit.ID, state, commit.CreatedAt.UTC().Format(time.RFC3339), commit.Author, commit.Message); err != nil {
			return err
		}
	}
	return nil
}

func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("update [-C DIR]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	var ours, theirs stringList
	fs.Var(&ours, "ours", "choose workspace side for a whole entry/subtree")
	fs.Var(&theirs, "theirs", "choose branch side for a whole entry/subtree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("update takes no positional arguments")
	}
	choices, err := makeChoices(ours, theirs)
	if err != nil {
		return err
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	if err = w.UpdateWithChoices(ctx, choices); err != nil {
		return fmt.Errorf("update workspace: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "updated to %s on branch %s\n", w.Base(), w.Branch())
	return err
}

func runSwitch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("switch [-C DIR] BRANCH", stderr)
	directory := fs.String("C", ".", "workspace directory")
	var ours, theirs stringList
	fs.Var(&ours, "ours", "choose current workspace side for a whole entry/subtree")
	fs.Var(&theirs, "theirs", "choose target branch side for a whole entry/subtree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return errors.New("switch requires exactly one BRANCH")
	}
	choices, err := makeChoices(ours, theirs)
	if err != nil {
		return err
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	if err = w.SwitchWithChoices(ctx, fs.Arg(0), choices); err != nil {
		return fmt.Errorf("switch workspace: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "switched to branch %s at %s\n", w.Branch(), w.Base())
	return err
}

func runSelection(ctx context.Context, operation string, args []string, stdout, stderr io.Writer) error {
	fs := newFlags(operation+" [-C DIR] PATH...", stderr)
	directory := fs.String("C", ".", "workspace directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("%s requires at least one PATH", operation)
	}
	paths := append([]string(nil), fs.Args()...)
	for i, value := range paths {
		if value == "." {
			paths[i] = ""
		}
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	if operation == "load" {
		err = w.Load(ctx, paths)
	} else {
		err = w.Unload(ctx, paths)
	}
	if err != nil {
		return fmt.Errorf("%s workspace paths: %w", operation, err)
	}
	_, err = fmt.Fprintf(stdout, "%s selection updated\n", operation)
	return err
}

func runRestore(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("restore [-C DIR] --from COMMIT PATH...", stderr)
	directory := fs.String("C", ".", "workspace directory")
	from := fs.String("from", "", "source commit ID")
	var ours, theirs stringList
	fs.Var(&ours, "ours", "choose workspace side for a whole entry/subtree")
	fs.Var(&theirs, "theirs", "choose historical side for a whole entry/subtree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || fs.NArg() == 0 {
		return errors.New("restore requires --from COMMIT and at least one PATH")
	}
	choices, err := makeChoices(ours, theirs)
	if err != nil {
		return err
	}
	paths := append([]string(nil), fs.Args()...)
	for i, value := range paths {
		if value == "." {
			paths[i] = ""
		}
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()
	if err = w.RestoreWithChoices(ctx, *from, paths, choices); err != nil {
		return fmt.Errorf("restore workspace paths: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "restored paths from %s\n", *from)
	return err
}

func runMerge(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("merge [-C DIR] [--preview] [--from COMMIT] [--ours PATH] [--theirs PATH] [--author NAME -m MESSAGE] [BRANCH]", stderr)
	directory := fs.String("C", ".", "workspace directory")
	from := fs.String("from", "", "source commit ID instead of a branch name")
	previewOnly := fs.Bool("preview", false, "show common base and unresolved conflict paths")
	author := fs.String("author", "", "merge author")
	message := fs.String("m", "", "merge commit message")
	var ours, theirs stringList
	fs.Var(&ours, "ours", "choose workspace side for a whole entry/subtree")
	fs.Var(&theirs, "theirs", "choose other side for a whole entry/subtree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*from != "" && fs.NArg() != 0) || (*from == "" && fs.NArg() != 1) {
		return errors.New("merge requires exactly one BRANCH or --from COMMIT")
	}
	if fs.NArg() == 1 && fs.Arg(0) == "" {
		return errors.New("merge branch must be non-empty")
	}
	if !*previewOnly && (strings.TrimSpace(*author) == "" || strings.TrimSpace(*message) == "") {
		return errors.New("merge requires non-empty --author and -m values unless --preview is used")
	}
	choices, err := makeChoices(ours, theirs)
	if err != nil {
		return err
	}
	w, err := workspace.Open(*directory)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = w.Close() }()

	otherTip := *from
	if otherTip == "" {
		branch, err := w.BranchReference(ctx, fs.Arg(0))
		if err != nil {
			return fmt.Errorf("resolve merge branch: %w", err)
		}
		otherTip = branch.Tip
	}
	if *previewOnly {
		var commonBase string
		var total int
		for after := ""; ; {
			page, err := w.PreviewMerge(ctx, otherTip, choices, after, 256)
			if err != nil {
				return fmt.Errorf("preview merge: %w", err)
			}
			if after == "" {
				commonBase, total = page.CommonBase, page.TotalConflicts
				if _, err := fmt.Fprintf(stdout, "common base: %s\nconflicts: %d\n", commonBase, total); err != nil {
					return err
				}
			}
			for _, path := range page.Conflicts {
				if _, err := fmt.Fprintf(stdout, "%q\n", path); err != nil {
					return err
				}
			}
			if page.Next == "" {
				return nil
			}
			after = page.Next
		}
	}
	commit, err := w.Merge(ctx, otherTip, *author, *message, choices)
	if errors.Is(err, project.ErrMergeConflict) {
		return errors.New("merge has unresolved conflicts; use --preview and resolve them with --ours/--theirs")
	}
	if err != nil {
		return fmt.Errorf("merge workspace: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "merged %s into %s as %s\n", otherTip, w.Branch(), commit.ID)
	return err
}

func makeChoices(ours, theirs stringList) ([]project.Choice, error) {
	choices := make([]project.Choice, 0, len(ours)+len(theirs))
	appendSide := func(paths []string, side model.Side) {
		for _, path := range paths {
			if path == "." {
				path = ""
			}
			choices = append(choices, project.Choice{Path: path, Side: side})
		}
	}
	appendSide(ours, model.LocalSide)
	appendSide(theirs, model.TargetSide)
	if _, err := project.Choices(choices); err != nil {
		return nil, err
	}
	return choices, nil
}

func runBranch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("branch requires create, delete, or list")
	}
	switch args[0] {
	case "--help", "-h", "help":
		if len(args) != 1 {
			return errors.New("branch help takes no arguments")
		}
		_, err := io.WriteString(stdout, "Usage: lode branch create|delete|list [options]\nUse 'lode branch create --help', 'lode branch delete --help', or 'lode branch list --help' for options.\n")
		return err
	case "create":
		fs := newFlags("branch create [-C DIR] [--from COMMIT] NAME", stderr)
		directory := fs.String("C", ".", "workspace directory")
		from := fs.String("from", "", "source commit ID (default: workspace base)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || fs.Arg(0) == "" {
			return errors.New("branch create requires exactly one NAME")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		if err = w.CreateBranch(ctx, fs.Arg(0), *from); err != nil {
			return fmt.Errorf("create branch: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "created branch %s\n", fs.Arg(0))
		return err
	case "delete":
		fs := newFlags("branch delete [-C DIR] NAME", stderr)
		directory := fs.String("C", ".", "workspace directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || fs.Arg(0) == "" {
			return errors.New("branch delete requires exactly one NAME")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		if err = w.DeleteBranch(ctx, fs.Arg(0)); err != nil {
			return fmt.Errorf("delete branch: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "deleted branch %s\n", fs.Arg(0))
		return err
	case "list":
		fs := newFlags("branch list [-C DIR]", stderr)
		directory := fs.String("C", ".", "workspace directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("branch list takes no positional arguments")
		}
		w, err := workspace.Open(*directory)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer func() { _ = w.Close() }()
		for after := ""; ; {
			branches, next, listErr := w.Branches(ctx, after, 256)
			if listErr != nil {
				return fmt.Errorf("list branches: %w", listErr)
			}
			for _, branch := range branches {
				if _, err = fmt.Fprintf(stdout, "%s %s\n", branch.Name, branch.Tip); err != nil {
					return err
				}
			}
			if next == "" {
				return nil
			}
			after = next
		}
	default:
		return fmt.Errorf("unknown branch command %q", args[0])
	}
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func newFlags(usage string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("lode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: lode %s\n", usage)
		fs.PrintDefaults()
	}
	return fs
}

func report(stderr io.Writer, code int, format string, values ...any) int {
	_, _ = fmt.Fprintf(stderr, "lode: "+format+"\n", values...)
	return code
}
