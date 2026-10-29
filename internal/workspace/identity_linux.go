package workspace

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Directory identity survives moves and child changes. Birth time, when
// available, also distinguishes a deleted directory from a reused inode.
func directoryIdentity(f *os.File) (string, error) {
	var s unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &s); err != nil {
		return "", err
	}
	id := fmt.Sprintf("%x:%x", s.Dev, s.Ino)
	var extended unix.Statx_t
	if unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &extended) == nil && extended.Mask&unix.STATX_BTIME != 0 {
		id += fmt.Sprintf(":%x:%x", extended.Btime.Sec, extended.Btime.Nsec)
	}
	return id, nil
}
