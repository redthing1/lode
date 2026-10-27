//go:build linux

package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameAtNoReplace(oldParent *os.File, oldname string, newParent *os.File, newname string) error {
	return unix.Renameat2(int(oldParent.Fd()), oldname, int(newParent.Fd()), newname, unix.RENAME_NOREPLACE)
}
