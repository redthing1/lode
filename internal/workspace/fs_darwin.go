//go:build darwin

package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameAtNoReplace(oldParent *os.File, oldname string, newParent *os.File, newname string) error {
	return unix.RenameatxNp(int(oldParent.Fd()), oldname, int(newParent.Fd()), newname, unix.RENAME_EXCL)
}
