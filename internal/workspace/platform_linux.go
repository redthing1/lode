//go:build linux

package workspace

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func readFlags() int { return os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK }
func noFollow() int  { return unix.O_NOFOLLOW }
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrBusy
	}
	return err
}
