package wf

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// an earlier revision: under a full parallel test run, a lock this process held read as
// "a process this host cannot name": /proc/locks did not list it. The table
// is a seq_file the kernel hands out a page per read, each read its own walk
// of the system-wide lock list, so locks removed between two reads shift the
// walk past entries that never moved. Here a table of several pages that
// churns: hundreds of flocks held, others taken and dropped at once, while a
// held lock's holder is read again and again with the holder's own record
// as the candidate. Every read must name it.
func TestLockTableNamesAHeldLockUnderChurn(t *testing.T) {
	if _, err := os.Stat(procLocksPath); err != nil {
		t.Skip("no /proc/locks")
	}
	dir := t.TempDir()
	open := func(name string) *os.File {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// The fixture fits the descriptor budget: held, plus four churners,
	// within the soft limit less a margin for the test binary's own files.
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	budget := int(min(lim.Cur, 1<<20)) - 128
	nHeld, nChurn := min(600, budget*3/7), min(200, budget/7)
	if nHeld < 100 || nChurn < 25 {
		t.Skipf("descriptor limit %d is too small for a multi-page lock table", lim.Cur)
	}
	var held []*os.File
	for i := range nHeld {
		f := open(fmt.Sprintf("held-%d", i))
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		held = append(held, f)
	}
	defer func() {
		for _, f := range held {
			f.Close()
		}
	}()
	// The lock under test is in the middle of the table.
	mine := held[nHeld/2]
	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var fs []*os.File
			for i := range nChurn {
				fs = append(fs, open(fmt.Sprintf("churn-%d-%d", w, i)))
			}
			defer func() {
				for _, f := range fs {
					f.Close()
				}
			}()
			for !stop.Load() {
				for _, f := range fs {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
				}
				for _, f := range fs {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				}
			}
		}()
	}
	misses := 0
	deadline := time.Now().Add(1500 * time.Millisecond)
	n := 0
	for ; time.Now().Before(deadline); n++ {
		var st syscall.Stat_t
		if err := syscall.Fstat(int(mine.Fd()), &st); err != nil {
			t.Fatal(err)
		}
		// The holder's own record names it, as the lock file's record does.
		if held, pid, _ := lockTableHolder(st, os.Getpid()); !held || pid != os.Getpid() {
			misses++
		}
	}
	stop.Store(true)
	wg.Wait()
	if misses > 0 {
		t.Fatalf("%d of %d reads of the lock table did not name this process holding its lock", misses, n)
	}
}

// an earlier revision round 2: a child handed its parent's already-locked fd shares the
// open file description, so its fdinfo lists the flock, but under the
// parent's pid: the kernel's owner. Asked with the child as the candidate,
// the holder is the parent, from the table, never the child.
func TestASharedLockedFdDoesNotMakeItsHolder(t *testing.T) {
	if _, err := os.Stat(procLocksPath); err != nil {
		t.Skip("no /proc/locks")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	held, pid, _ := lockTableHolder(st, cmd.Process.Pid)
	if !held || pid != os.Getpid() {
		t.Fatalf("holder with the fd-sharing child %d as candidate = held %v pid %d, want this process %d", cmd.Process.Pid, held, pid, os.Getpid())
	}
}
