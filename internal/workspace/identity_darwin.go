package workspace

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func directoryIdentity(f *os.File) (string, error) {
	var s unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &s); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x:%x", s.Dev, s.Ino, s.Btim.Sec, s.Btim.Nsec), nil
}
