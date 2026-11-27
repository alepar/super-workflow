package wf

import (
	"io/fs"
	"syscall"
)

func statIdentity(fi fs.FileInfo) (hashKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return hashKey{}, false
	}
	return hashKey{dev: uint64(st.Dev), ino: uint64(st.Ino), size: fi.Size(), mtime: st.Mtimespec.Nano(), ctime: st.Ctimespec.Nano()}, true
}
