package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"

	"github.com/redthing1/lode/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "lode:", err)
		os.Exit(1)
	}
}

const usage = `Usage: lode [-C directory] [--no-progress] <command>

  login         Save an access token for a server
  projects      List or create projects
  attach        Connect an existing folder to an empty project
  clone         Download a project, optionally selecting paths
  status        Show selected local changes (works offline)
  save          Publish local changes
  update        Reconcile the latest shared version with local work
  select        Show or change the paths present locally
  branch        List, create or delete shared branches
  tag           Name and protect a saved version
  retention     Show or change the project's cleanup policy
  cleanup       Preview or apply content expiry and history pruning
  collect       Reclaim unreferenced content storage
  switch        Work on another branch
  merge         Integrate a saved branch or version into the current branch
  restore       Restore saved contents as local changes
  resume        Continue an interrupted operation
  cancel        Settle a pending save or reverse an unfinished local operation
  log           Show saved versions
  init          Create a server instance (Linux)
  serve         Run a server instance (Linux)
  backup        Copy and verify a stopped instance (Linux)
  recover-user  Reset an account on a stopped server (Linux)
  version       Print the version

Run lode <command> --help for command help.
`

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "--version" {
		args = []string{"version"}
	}
	global := flags("lode", usage, out)
	directory := global.String("C", ".", "working `directory`")
	noProgress := global.Bool("no-progress", false, "disable interactive progress")
	if err := global.Parse(args); err != nil {
		return err
	}
	args = global.Args()
	if len(args) == 0 || len(args) == 1 && args[0] == "help" {
		_, err := io.WriteString(out, usage)
		return err
	}
	if args[0] == "help" {
		args = append(args[1:], "--help")
	}
	ctx, out, finish := startProgress(ctx, out, args[0], *noProgress)
	defer finish()
	switch args[0] {
	case "version":
		if len(args) != 1 {
			return errors.New("usage: lode version")
		}
		fmt.Fprintln(out, "Lode", version.Current)
		return nil
	case "login":
		return login(ctx, args[1:], out)
	case "projects":
		return projects(ctx, args[1:], out)
	case "attach", "clone":
		return connectWorkspace(ctx, args[0], args[1:], *directory, out)
	case "status", "save", "update", "select", "merge", "resume", "cancel", "log":
		return workspaceCommand(ctx, args[0], args[1:], *directory, out)
	case "branch", "tag", "switch", "restore":
		return historyCommand(ctx, args[0], args[1:], *directory, out)
	case "retention", "cleanup", "collect":
		return retentionCommand(ctx, args[0], args[1:], *directory, out)
	case "init", "serve", "backup", "recover-user":
		return serverCommand(ctx, args[0], args[1:], *directory, out)
	default:
		return fmt.Errorf("unknown command %q; run lode --help", args[0])
	}
}

func flags(name, usage string, out io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.Usage = func() {
		fmt.Fprintln(out, strings.TrimSpace(usage))
		fmt.Fprintln(out)
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		f.VisitAll(func(option *flag.Flag) {
			prefix := "--"
			if len(option.Name) == 1 {
				prefix = "-"
			}
			value, description := flag.UnquoteUsage(option)
			label := prefix + option.Name
			if value != "" {
				label += " " + value
			}
			if option.DefValue != "" && option.DefValue != "0" && option.DefValue != "false" {
				description += " (default: " + option.DefValue + ")"
			}
			fmt.Fprintf(w, "  %s\t%s\n", label, description)
		})
		fmt.Fprintln(w, "  -h, --help\tShow help")
		w.Flush()
	}
	return f
}
