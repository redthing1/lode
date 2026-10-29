package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/redthing1/lode/internal/workspace"
)

func remoteCommand(ctx context.Context, args []string, directory string, out io.Writer) error {
	const usage = "Usage: lode remote [set-url <project-url>]\nShow the connection, or change its URL to the same project. Sign in to the new origin first."
	f := flags("remote", usage, out)
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 && (f.NArg() != 2 || f.Arg(0) != "set-url") {
		return errors.New(usage)
	}
	w, err := workspace.Open(directory)
	if err != nil {
		return err
	}
	defer w.Close()
	if f.NArg() != 0 {
		origin, err := originOf(f.Arg(1), true)
		if err != nil {
			return err
		}
		c, err := authenticated(origin)
		if err != nil {
			return err
		}
		defer c.HTTP.CloseIdleConnections()
		if err := w.SetRemote(ctx, f.Arg(1), c); err != nil {
			return err
		}
	}
	s := w.State()
	fmt.Fprintf(out, "%s/%s/%s\n", s.Server, s.Owner, s.Project)
	return nil
}
