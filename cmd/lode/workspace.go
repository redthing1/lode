package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/workspace"
)

type paths []string

func (p *paths) String() string { return strings.Join(*p, ", ") }
func (p *paths) Set(value string) error {
	value = strings.TrimSuffix(value, "/")
	if value == "." {
		value = ""
	}
	if value != "" && !model.ValidPath(value) {
		return errors.New("use a project-relative path")
	}
	*p = append(*p, value)
	return nil
}

func connectWorkspace(ctx context.Context, command string, args []string, directory string, out io.Writer) error {
	f := flags(command, "Usage: lode "+command+" [options] <project-url> [directory]", out)
	var include, exclude paths
	f.Var(&include, "include", "include `path` and descendants (repeatable; default: all)")
	f.Var(&exclude, "exclude", "exclude `path` and descendants (repeatable)")
	branch := f.String("branch", "main", "branch `name` to connect")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() < 1 || f.NArg() > 2 {
		return fmt.Errorf("usage: lode %s [options] <project-url> [directory]", command)
	}
	selection, err := selectionOf(include, exclude)
	if err != nil {
		return err
	}
	origin, err := originOf(f.Arg(0), true)
	if err != nil {
		return err
	}
	c, err := authenticated(origin)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	target := "."
	if command == "clone" {
		u, _ := url.Parse(f.Arg(0))
		target = path.Base(strings.TrimSuffix(u.Path, "/"))
	}
	if f.NArg() == 2 {
		target = f.Arg(1)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(directory, target)
	}
	var w *workspace.Workspace
	if command == "clone" {
		w, err = workspace.Clone(ctx, f.Arg(0), c, target, workspace.ConnectOptions{Branch: *branch, Selection: selection})
	} else {
		w, err = workspace.Attach(ctx, f.Arg(0), c, target, workspace.ConnectOptions{Branch: *branch, Selection: selection})
	}
	if err != nil {
		return err
	}
	defer w.Close()
	s := w.State()
	fmt.Fprintf(out, "%s/%s · %s\n%s\n", s.Owner, s.Project, s.Branch, w.Directory())
	return nil
}

func selectionOf(include, exclude paths) (model.Selection, error) {
	selection := model.Selection{{Include: true}}
	if len(include) > 0 {
		selection = nil
	}
	for _, p := range include {
		selection = append(selection, model.Rule{Path: p, Include: true})
	}
	for _, p := range exclude {
		selection = append(selection, model.Rule{Path: p})
	}
	return selection, selection.Validate()
}

func workspaceCommand(ctx context.Context, command string, args []string, directory string, out io.Writer) error {
	usage := "Usage: lode " + command
	if command == "merge" {
		usage += " [options] <branch-or-version>"
	}
	f := flags(command, usage, out)
	message := ""
	limit := 20
	var local, incoming, include, exclude paths
	var none, full bool
	if command == "update" || command == "select" || command == "merge" {
		f.Var(&local, "local", "keep local contents at conflicting `path` (repeatable)")
		f.Var(&incoming, "incoming", "use incoming contents at conflicting `path` (repeatable)")
	}
	if command == "select" {
		f.Var(&include, "include", "include `path` and descendants (repeatable)")
		f.Var(&exclude, "exclude", "exclude `path` and descendants (repeatable)")
		f.BoolVar(&none, "none", false, "unload all clean selected paths")
	}
	if command == "save" || command == "merge" {
		f.StringVar(&message, "m", "", "version `message`")
	}
	if command == "log" {
		f.IntVar(&limit, "n", 20, "maximum `count` of versions (1–100)")
		f.BoolVar(&full, "full", false, "show complete version IDs")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if command == "merge" && f.NArg() != 1 || command != "merge" && f.NArg() != 0 {
		return errors.New(usage)
	}
	if none && len(include)+len(exclude) > 0 {
		return errors.New("use --none without include/exclude rules")
	}
	w, err := workspace.Open(directory)
	if err != nil {
		return err
	}
	defer w.Close()
	s := w.State()
	choices := map[string]model.Side{}
	for _, side := range []struct {
		paths paths
		side  model.Side
	}{{local, model.Local}, {incoming, model.Incoming}} {
		for _, p := range side.paths {
			if _, ok := choices[p]; ok {
				return fmt.Errorf("repeated conflict choice %q", p)
			}
			choices[p] = side.side
		}
	}
	if command == "select" && len(include) == 0 && len(exclude) == 0 && !none {
		if len(choices) > 0 {
			return errors.New("specify the selection along with conflict choices")
		}
		for _, rule := range s.Selection {
			kind := "exclude"
			if rule.Include {
				kind = "include"
			}
			p := rule.Path
			if p == "" {
				p = "."
			}
			fmt.Fprintf(out, "%s %s\n", kind, p)
		}
		if len(s.Selection) == 0 {
			fmt.Fprintln(out, "No paths selected.")
		}
		return nil
	}
	if command == "status" {
		changes, err := w.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s/%s · %s\n", s.Owner, s.Project, s.Branch)
		if len(changes) == 0 {
			fmt.Fprintln(out, "No selected changes.")
			return nil
		}
		before := map[string]model.Entry{}
		for _, e := range s.Baseline {
			before[e.Path] = e
		}
		for _, e := range changes {
			kind := "M"
			if e.Kind == model.Absent {
				kind = "D"
			} else if _, exists := before[e.Path]; !exists {
				kind = "A"
			}
			fmt.Fprintf(out, "%s  %s\n", kind, e.Path)
		}
		return nil
	}
	if command == "cancel" {
		// Local recovery does not need credentials. Only a pending publication
		// reaches the authenticated transport and asks us to load them.
		err := w.Cancel(ctx, "")
		if !errors.Is(err, remote.ErrCredentialsRequired) {
			if err == nil {
				fmt.Fprintln(out, "Pending operation settled.")
			}
			return err
		}
	}
	cred, err := workspaceCredentials(s)
	if err != nil {
		return err
	}
	w.AllowHTTP = cred.AllowHTTP
	switch command {
	case "merge":
		v, err := w.Merge(ctx, cred.Token, f.Arg(0), message, choices)
		var conflict *remote.Error
		if errors.As(err, &conflict) && conflict.Code == "conflicts" {
			for _, c := range conflict.Conflicts {
				fmt.Fprintf(out, "C  %s (target: %s, incoming: %s)\n", c.Path, c.Local, c.Incoming)
			}
			return fmt.Errorf("%w; retry lode merge --local <path> or --incoming <path> <source>", err)
		}
		var localConflict *workspace.ConflictsError
		if errors.As(err, &localConflict) {
			for _, c := range localConflict.Conflicts {
				fmt.Fprintf(out, "C  %s (local: %s, incoming: %s)\n", c.Path, c.Local, c.Incoming)
			}
			return fmt.Errorf("%w; resolve with lode update --local <path> or --incoming <path>", err)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Merged %s. Workspace ready.\n", model.ShortVersion(v.ID))
	case "update", "select":
		if command == "update" {
			err = w.Update(ctx, cred.Token, choices)
		} else {
			selection, selectionErr := selectionOf(include, exclude)
			if selectionErr != nil {
				return selectionErr
			}
			if none {
				selection = model.Selection{}
			}
			err = w.Select(ctx, cred.Token, selection, choices)
		}
		var conflicts *workspace.ConflictsError
		if errors.As(err, &conflicts) {
			for _, c := range conflicts.Conflicts {
				fmt.Fprintf(out, "C  %s (local: %s, incoming: %s)\n", c.Path, c.Local, c.Incoming)
			}
			return fmt.Errorf("resolve with lode %s --local <path> or --incoming <path>; repeat flags for multiple conflicts", command)
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "Workspace ready.")
	case "save":
		v, err := w.Save(ctx, cred.Token, message)
		if errors.Is(err, workspace.ErrNoChanges) {
			fmt.Fprintln(out, "No selected changes.")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Saved %s\n", v.ID)
	case "resume":
		if err := w.Resume(ctx, cred.Token); err != nil {
			return err
		}
		fmt.Fprintln(out, "Workspace ready.")
	case "cancel":
		if err := w.Cancel(ctx, cred.Token); err != nil {
			return err
		}
		fmt.Fprintln(out, "Pending operation settled.")
	case "log":
		if limit < 1 || limit > 100 {
			return errors.New("version limit must be 1–100")
		}
		c, err := w.Client(cred.Token)
		if err != nil {
			return err
		}
		defer c.HTTP.CloseIdleConnections()
		versions, err := c.History(ctx, s.Owner, s.Project, s.Branch, limit)
		if err != nil {
			return err
		}
		if len(versions.Versions) == 0 {
			fmt.Fprintln(out, "No saved versions.")
			return nil
		}
		rows := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		for _, v := range versions.Versions {
			id := model.ShortVersion(v.ID)
			if full {
				id = v.ID
			}
			availability := ""
			if !v.Retained {
				availability = " (contents expired)"
			}
			fmt.Fprintf(rows, "%s\t%s\t%q%s\n", id, v.Created.Local().Format("2006-01-02 15:04"), v.Message, availability)
		}
		if err := rows.Flush(); err != nil {
			return err
		}
		if versions.Boundary != "" {
			fmt.Fprintln(out, "Earlier history has been removed.")
		}
		return nil
	}
	return nil
}

func projects(ctx context.Context, args []string, out io.Writer) error {
	f := flags("projects", "Usage: lode projects [--create name] <server-url>", out)
	name := f.String("create", "", "create project `name`")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 {
		return errors.New("usage: lode projects [--create name] <server-url>")
	}
	origin, err := originOf(f.Arg(0), true)
	if err != nil {
		return err
	}
	c, err := authenticated(origin)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	if *name != "" {
		p, err := c.CreateProject(ctx, *name)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s/%s/%s\n", origin, p.Owner, p.Name)
		return nil
	}
	projects, err := c.Projects(ctx)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		fmt.Fprintln(out, "No projects.")
	}
	for _, p := range projects {
		fmt.Fprintf(out, "%s/%s/%s\n", origin, p.Owner, p.Name)
	}
	return nil
}
