package wf

import (
	"os"
	"path/filepath"
	"syscall"
)

const snapshotFDPath = "/proc/self/fd"

func openSnapshotComponent(parent *os.File, name string, flags int) (*os.File, error) {
	path := filepath.Join(parent.Name(), name)
	for {
		fd, err := syscall.Openat(int(parent.Fd()), name, flags|syscall.O_CLOEXEC, 0)
		switch err {
		case nil:
			return os.NewFile(uintptr(fd), path), nil
		case syscall.EINTR:
			continue
		default:
			return nil, &os.PathError{Op: "openat", Path: path, Err: err}
		}
	}
}
