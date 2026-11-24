package wf_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// the run supervisor (the regression case rewind1, effect 2314): while a command effect runs,
// the run's status says so. It names no person and asks nobody to settle
// it: a coordinator that read "waiting on human: verify effect N" stopped
// and asked one.
func TestInFlightEffectReasonSaysRunningWait(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	got := h.status(run).NothingReadyReason
	if want := "effect " + strconv.FormatInt(intent, 10) + " is running, wait"; got != want {
		t.Errorf("status reason = %q, want %q", got, want)
	}
	noPersonOrSettle(t, got)
}

func noPersonOrSettle(t *testing.T, got string) {
	t.Helper()
	for _, bad := range []string{"human", "person", "settle", "verify"} {
		if strings.Contains(got, bad) {
			t.Errorf("status reason = %q mentions %q while the effect may still be running", got, bad)
		}
	}
}

// Review round 1 (2): a runner still going past the command's maximum is not
// waited for: only a driving call enforces the maximum, so status sends the
// caller to next.
func TestOverdueDetachedEffectSaysRunNext(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	h.now = h.now.Add(2 * time.Minute)
	got := h.status(run).NothingReadyReason
	if want := "effect " + strconv.FormatInt(intent, 10) + " is past its maximum duration: run next to stop it"; got != want {
		t.Errorf("status reason = %q, want %q", got, want)
	}
	noPersonOrSettle(t, got)
}

// Review round 1 (1): a detached runner that has ended (a result file, or a
// dead group) is not running, and no held lock makes it so: the next call
// records or recovers it.
func TestEndedDetachedEffectSaysRunNext(t *testing.T) {
	for _, st := range []wf.DetachedStatus{wf.DetachedPresent, wf.DetachedDead} {
		fr, w := newDetRunner(), newWorld()
		h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
		fr.set(intent, st, wf.RunResult{Exit: 0, Stdout: "built"})
		got := h.status(run).NothingReadyReason
		if want := "effect " + strconv.FormatInt(intent, 10) + " has ended: run next to record it"; got != want {
			t.Errorf("runner status %d: reason = %q, want %q", st, got, want)
		}
	}
}

// fileUncertainRun is the the regression case shape on a file journal: an inline effect
// (a bd dep_add) whose intent is journaled with no result.
func fileUncertainRun(t *testing.T) (*crashHarness, string, wf.RunID, int64) {
	t.Helper()
	dir := t.TempDir()
	d, _ := effectDef(nil, mergeEffect(wf.NonIdempotent("writes a ledger")))
	w := newWorld()
	h := newHarness(t, d, w)
	h.j = wf.FileJournal(dir)
	cj := &crashJournal{Journal: h.j, kind: wf.JournalEffectResult}
	h.e = wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
	c := &crashHarness{harness: h, cj: cj, w: w, d: d}
	run := c.run(d, nil)
	c.crashAt(run, `{}`)
	return c, dir, run, c.kind(run, wf.JournalEffectIntent)[0].Seq
}

// Review round 1 (1): another call holding the run lock does not prove it is
// applying this intent (a reader, a suspended harvest), so status says the
// effect may be running and sends the caller to next, which waits its turn
// and then shows what the effect needs. With the lock let go and no result,
// it is the coordinator's to settle again.
func TestInlineEffectUnderAHeldLockMayBeRunning(t *testing.T) {
	c, dir, run, intent := fileUncertainRun(t)
	maybe := "effect " + strconv.FormatInt(intent, 10) + " may be running: another call holds the run; run next, which waits for it"

	unlock, err := wf.FileJournal(dir).Lock(run) // another process's open of the lock
	if err != nil {
		t.Fatal(err)
	}
	if got := c.status(run).NothingReadyReason; got != maybe {
		t.Errorf("lock held by another journal: reason = %q, want %q", got, maybe)
	}
	unlock()

	// The same journal value, held by another call in this process: status
	// reads lock-free, so the hold is never its own.
	unlock, err = c.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.status(run).NothingReadyReason; got != maybe {
		t.Errorf("lock held through the engine's own journal: reason = %q, want %q", got, maybe)
	}
	unlock()

	if got := c.status(run).NothingReadyReason; !strings.HasPrefix(got, "waiting on the coordinator: settle effect") {
		t.Errorf("lock let go: reason = %q, want the coordinator's settle", got)
	}
}

// Review round 1 (3): the probe takes no lock of its own, so it can never
// make a writer's nonblocking acquire fail. It reads the kernel's lock table
// by the lock file's inode: with the file unreadable, it still sees a hold.
func TestTheLockProbeTakesNoLock(t *testing.T) {
	c, dir, run, intent := fileUncertainRun(t)
	path := filepath.Join(dir, "runs", string(run), "lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if g, err := os.Open(path); err == nil {
		g.Close()
		t.Skip("the lock file opens with no permissions (running as root)")
	}
	want := "effect " + strconv.FormatInt(intent, 10) + " may be running"
	if got := c.status(run).NothingReadyReason; !strings.HasPrefix(got, want) {
		t.Errorf("reason = %q, want it to start %q from the lock table alone", got, want)
	}
}

// Review round 2: the launcher started the command and wrote its proc
// record, then the effect_started append failed. Past the start grace, status
// reads the proc and result files as next does (Launched, Status), so a live
// command is running, and one whose result file is there has ended.
func TestLaunchedEffectWithLostStartedReadsFromTheProcRecord(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, fr))
	withDroppedStarted(t, h, fr, d)
	run := h.run(d, map[string]string{"topic": "build"})
	if _, err := h.e.Next(run, ""); err == nil {
		t.Fatal("Next succeeded, want the lost effect_started append to fail it")
	}
	intent := lastIntent(t, h, run)
	if len(startedOf(t, h, run)) != 0 {
		t.Fatal("effect_started was journaled; the repro needs it lost")
	}
	n := "effect " + strconv.FormatInt(intent, 10)
	h.now = h.now.Add(15 * time.Second)
	got := h.status(run).NothingReadyReason
	if want := n + " is running, wait"; got != want {
		t.Errorf("live launch at +15s: reason = %q, want %q", got, want)
	}
	noPersonOrSettle(t, got)

	fr.set(intent, wf.DetachedPresent, wf.RunResult{Exit: 0, Stdout: "built"})
	if got, want := h.status(run).NothingReadyReason, n+" has ended: run next to record it"; got != want {
		t.Errorf("result file there: reason = %q, want %q", got, want)
	}
}
