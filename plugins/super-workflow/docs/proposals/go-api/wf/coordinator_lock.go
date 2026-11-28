package wf

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// A coordinator is live while its blocking next runs: the call holds a flock on
// runs/<run>/coordinators/<session>.lock for as long as it blocks. A
// next from a newer session that finds an earlier session's lock held refuses
// instead of re-offering that live coordinator's children. A coordinator
// that died lets go of its lock with its process, so a restart is refused at
// most until an orphaned blocking next of the dead one times out. The lock is a
// liveness hint, never run state: nothing is journaled from it.

// coordinatorLockPath is session's lock file of run, or "" without a run dir.
func (e *Engine) coordinatorLockPath(run RunID, session string) string {
	if e.opts.RunDir == nil || session == "" {
		return ""
	}
	dir := e.opts.RunDir(run)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "coordinators", strings.ReplaceAll(session, string(filepath.Separator), "_")+".lock")
}

// holdCoordinatorLock takes session's lock for the length of a blocking next,
// writing this process's pid into it; release lets go. A lock it cannot take
// (another call of the same session holds it) is not an error: release is a
// no-op then.
func (e *Engine) holdCoordinatorLock(run RunID, session string) (release func()) {
	path := e.coordinatorLockPath(run, session)
	if path == "" {
		return func() {}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return func() {}
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() { f.Close() } // closing the only descriptor releases the flock
}

// coordinatorLive reports whether session's blocking next is running now, and
// the pid it wrote.
func (e *Engine) coordinatorLive(run RunID, session string) (bool, string) {
	path := e.coordinatorLockPath(run, session)
	if path == "" {
		return false, ""
	}
	f, err := os.Open(path)
	if err != nil {
		return false, ""
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, ""
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, ""
	}
	raw, _ := os.ReadFile(path)
	return true, strings.TrimSpace(string(raw))
}
