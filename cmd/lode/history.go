package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/workspace"
)

func historyCommand(ctx context.Context, command string, args []string, directory string, out io.Writer) error {
	action := ""
	usage := "Usage: lode " + command + " <branch>"
	if command == "branch" || command == "tag" {
		usage = "Usage: lode " + command + " [create [-from reference] <name> | delete <name>]"
		if len(args) > 0 && (args[0] == "create" || args[0] == "delete") {
			action, args = args[0], args[1:]
		}
	} else if command == "restore" {
		usage = "Usage: lode restore <reference> [path ...]"
	}
	f := flags(command, usage, out)
	from := ""
	if action == "create" {
		f.StringVar(&from, "from", "", "start from a branch, tag or version (default: workspace version)")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if (command == "branch" || command == "tag") && action == "" && f.NArg() != 0 || command == "restore" && f.NArg() < 1 || (command == "switch" || action != "") && f.NArg() != 1 {
		return errors.New(usage)
	}
	w, err := workspace.Open(directory)
	if err != nil {
		return err
	}
	defer w.Close()
	s := w.State()
	cred, err := credentials(s.Server)
	if err != nil {
		return err
	}
	if cred.Instance != "" && cred.Instance != s.InstanceID {
		return errors.New("saved credentials belong to another instance; check this workspace's server")
	}
	if command == "switch" {
		if err := w.Switch(ctx, cred.Token, f.Arg(0)); err != nil {
			return err
		}
		fmt.Fprintf(out, "On branch %s.\n", w.State().Branch)
		return nil
	}
	if command == "restore" {
		paths := append([]string(nil), f.Args()[1:]...)
		for i, name := range paths {
			name = strings.TrimSuffix(name, "/")
			if name == "." {
				name = ""
			}
			paths[i] = name
		}
		if err := w.Restore(ctx, cred.Token, f.Arg(0), paths); err != nil {
			return err
		}
		fmt.Fprintln(out, "Restored working files; use lode status to review before saving.")
		return nil
	}
	c, err := w.Client(cred.Token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	switch action {
	case "create":
		version := s.Version
		if from != "" {
			v, err := c.Reference(ctx, s.Owner, s.Project, from)
			if err != nil {
				return err
			}
			version = v.ID
		}
		ref := model.Reference{Name: f.Arg(0), Version: version}
		if command == "tag" {
			_, err = c.CreateTag(ctx, s.Owner, s.Project, ref)
		} else {
			_, err = c.CreateBranch(ctx, s.Owner, s.Project, ref)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s created.\n", command, ref.Name)
	case "delete":
		if command == "branch" && f.Arg(0) == s.Branch {
			return errors.New("switch to another branch before deleting this one")
		}
		v, err := c.Reference(ctx, s.Owner, s.Project, command+":"+f.Arg(0))
		if err != nil {
			return err
		}
		ref := model.Reference{Name: f.Arg(0), Version: v.ID}
		if command == "tag" {
			err = c.DeleteTag(ctx, s.Owner, s.Project, ref)
		} else {
			err = c.DeleteBranch(ctx, s.Owner, s.Project, ref)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s deleted. Saved versions remain subject to retention.\n", command, f.Arg(0))
	default:
		rows := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		after := ""
		for {
			var refs []model.Reference
			var next string
			if command == "tag" {
				page, e := c.Tags(ctx, s.Owner, s.Project, after)
				refs, next, err = page.Tags, page.Next, e
			} else {
				page, e := c.Branches(ctx, s.Owner, s.Project, after)
				refs, next, err = page.Branches, page.Next, e
			}
			if err != nil {
				return err
			}
			for _, b := range refs {
				marker := " "
				if command == "branch" && b.Name == s.Branch {
					marker = "*"
				}
				fmt.Fprintf(rows, "%s %s\t%s\n", marker, b.Name, model.ShortVersion(b.Version))
			}
			if next == "" {
				break
			}
			if next <= after {
				return errors.New("reference listing did not advance")
			}
			after = next
		}
		return rows.Flush()
	}
	return nil
}
