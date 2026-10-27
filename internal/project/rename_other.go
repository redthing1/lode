//go:build !linux

package project

import (
	"fmt"
	"runtime"
)

func renameNoReplace(_, _ string) error {
	return fmt.Errorf("atomic no-replace project publication is unsupported on %s", runtime.GOOS)
}
