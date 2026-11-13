package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/workspace"
)

func retentionCommand(ctx context.Context, command string, args []string, directory string, out io.Writer) error {
	usage := "Usage: lode " + command
	if command != "collect" {
		usage += " [options]"
	}
	f := flags(command, usage, out)
	var keep, days int
	var token string
	if command == "retention" {
		f.IntVar(&keep, "keep", 0, "`count` of versions to keep per branch")
		f.IntVar(&days, "history-days", 0, "`days` of history metadata to keep")
	} else if command == "cleanup" {
		f.StringVar(&token, "apply", "", "apply reviewed cleanup `token`")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	w, err := workspace.Open(directory)
	if err != nil {
		return err
	}
	defer w.Close()
	s := w.State()
	cred, err := workspaceCredentials(ctx, s)
	if err != nil {
		return err
	}
	w.AllowHTTP = cred.AllowHTTP
	c, err := w.Client(cred.Token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	if command == "collect" {
		fmt.Fprintln(out, "Collecting storage; project reads and saves wait until collection finishes.")
		result, err := c.Collect(ctx, s.Owner, s.Project)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Content repository: %.1f MiB → %.1f MiB allocated.\n", float64(result.Before.Allocated)/(1<<20), float64(result.After.Allocated)/(1<<20))
		return nil
	}
	if command == "retention" {
		policy, err := c.Retention(ctx, s.Owner, s.Project)
		if err != nil {
			return err
		}
		changed := false
		f.Visit(func(option *flag.Flag) {
			changed = true
			if option.Name == "keep" {
				policy.Versions = keep
			} else {
				policy.HistoryDays = days
			}
		})
		if changed {
			if err := c.SetRetention(ctx, s.Owner, s.Project, policy); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "Contents: %d versions per branch; history: %d days. Tags remain protected.\n", policy.Versions, policy.HistoryDays)
		if changed {
			fmt.Fprintln(out, "Policy saved; run lode cleanup to review its consequences.")
		}
		return nil
	}
	p, err := c.Cleanup(ctx, s.Owner, s.Project, token)
	if err != nil {
		return err
	}
	if token != "" {
		fmt.Fprintf(out, "Contents: %d kept, %d expired; history: %d removed.\n", p.Retained, p.Expire, p.Forget)
		fmt.Fprintln(out, "Cleanup applied; run lode collect to reclaim unreferenced storage.")
		return nil
	}
	fmt.Fprintf(out, "Contents: %d kept, %d to expire; history: %d to remove.\n", p.Retained, p.Expire, p.Forget)
	for _, group := range []struct {
		label    string
		versions []model.Version
		total    int
	}{{"Expire", p.ExpireExamples, p.Expire}, {"Forget", p.ForgetExamples, p.Forget}} {
		for _, v := range group.versions {
			fmt.Fprintf(out, "%s  %s  %q\n", group.label, model.ShortVersion(v.ID), v.Message)
		}
		if group.total > len(group.versions) {
			fmt.Fprintf(out, "%s: showing %d of %d versions.\n", group.label, len(group.versions), group.total)
		}
	}
	if p.Forget > 0 {
		fmt.Fprintln(out, "Removed history can prevent later automatic merges.")
	}
	fmt.Fprintln(out, "Local files are unchanged.")
	fmt.Fprintf(out, "Apply within one hour: lode cleanup --apply %s\n", p.Token)
	return nil
}
