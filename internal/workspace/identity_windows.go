package workspace

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func directoryIdentity(f *os.File) (string, error) {
	c, err := f.SyscallConn()
	if err != nil {
		return "", err
	}
	var s windows.ByHandleFileInformation
	var readErr error
	if err := c.Control(func(fd uintptr) { readErr = windows.GetFileInformationByHandle(windows.Handle(fd), &s) }); err != nil {
		return "", err
	}
	if readErr != nil {
		return "", readErr
	}
	return fmt.Sprintf("%x:%x:%x:%x:%x", s.VolumeSerialNumber, s.FileIndexHigh, s.FileIndexLow, s.CreationTime.HighDateTime, s.CreationTime.LowDateTime), nil
}
