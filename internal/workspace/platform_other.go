//go:build !linux

package workspace

import (
	"errors"
	"os"
)

func readFlags() int          { return os.O_RDONLY }
func noFollow() int           { return 0 }
func lockFile(*os.File) error { return errors.New("workspaces currently require Linux") }
