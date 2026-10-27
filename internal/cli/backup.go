package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/redthing1/lode/internal/project"
)

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("backup STORE DESTINATION", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("backup requires a stopped STORE and a fresh DESTINATION directory")
	}
	if err := project.BackupLocal(ctx, fs.Arg(0), fs.Arg(1)); err != nil {
		return fmt.Errorf("back up project: %w", err)
	}
	_, err := fmt.Fprintf(stdout, "project copied to %s\n", fs.Arg(1))
	return err
}
