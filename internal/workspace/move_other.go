//go:build !linux

package workspace

import (
	"errors"
	"os"
)

var errWorkspaceMoveUnsupported = errors.New("workspace no-replace moves are only supported on Linux")

func moveNoReplace(root *os.Root, from, to string) error { return errWorkspaceMoveUnsupported }

func linkNoReplace(root *os.Root, from, to string) error { return errWorkspaceMoveUnsupported }
