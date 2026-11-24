package wftest_test

import (
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	DRun  struct{}
	DOK   struct{}
	DFail struct{}
)

type dExit int

// detachedDef is one keyed, idempotent detached command: exit 0 passes, 1 and
// 124 fail.
func detachedDef(max time.Duration) *wf.Def {
	d := wf.New("detached-sim")
	exits := wf.NewEnum(d, "d-exit", dExit(0), dExit(1), dExit(124))
	cmd := wf.NewCommand[DRun](d, "make")
	ok := wf.NewTerminal[DOK](d, wf.Success)
	fail := wf.NewTerminal[DFail](d, wf.Stopped)
	cmd.Key("build").Idempotent().Detached(max)
	cmd.RouteCases(wf.Cases[DRun, wf.RunResult](wf.ExitAs[dExit], exits,
		wf.On(dExit(0), cmd.To(ok, "passes")),
		wf.On(dExit(1), cmd.To(fail, "fails")),
		wf.On(dExit(124), cmd.To(fail, "timed out"))))
	return d
}

func kinds(tr *wftest.Trace, k wf.JournalKind) int {
	n := 0
	for _, r := range tr.Journal() {
		if r.Kind == k {
			n++
		}
	}
	return n
}

func TestFakeRunnerRunningLeavesTheRunOpen(t *testing.T) {
	fr := wftest.NewFakeRunner()
	tr := wftest.New(detachedDef(time.Hour), wftest.WithTB(t), wftest.WithEffectRunner(fr)).Run(wftest.Resume())
	if tr.Terminal() != "" || len(fr.Started()) != 1 {
		t.Fatalf("terminal %q, launches %d, want an open run with one launch", tr.Terminal(), len(fr.Started()))
	}
	if kinds(tr, wf.JournalEffectStarted) != 1 || kinds(tr, wf.JournalEffectResult) != 0 {
		t.Fatalf("journal has %d effect_started, %d effect_result, want 1 and 0", kinds(tr, wf.JournalEffectStarted), kinds(tr, wf.JournalEffectResult))
	}
}

// The first drive launches the command; each later one polls it.
func TestFakeRunnerPresentRoutesTheResult(t *testing.T) {
	for exit, want := range map[int]string{0: "DOK", 1: "DFail"} {
		fr := wftest.NewFakeRunner()
		fr.SetStatus(0, wf.DetachedPresent, wf.RunResult{Exit: exit})
		tr := wftest.New(detachedDef(time.Hour), wftest.WithTB(t), wftest.WithEffectRunner(fr)).Run(wftest.Resume(), wftest.Resume())
		if tr.Terminal() != want {
			t.Fatalf("exit %d: terminal %q, want %q", exit, tr.Terminal(), want)
		}
	}
}

func TestFakeRunnerPastMaxIsTerminated(t *testing.T) {
	fr := wftest.NewFakeRunner()
	// The launch's intent is stamped before its Start, so the next poll is 2h on,
	// past the hour's maximum.
	base := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	clock := wftest.WithClock(func() time.Time { return base.Add(2 * time.Hour * time.Duration(len(fr.Started()))) })
	tr := wftest.New(detachedDef(time.Hour), wftest.WithTB(t), wftest.WithEffectRunner(fr), clock).Run(wftest.Resume(), wftest.Resume())
	if got := fr.Terminated(); len(got) != 1 {
		t.Fatalf("terminated = %v, want one termination", got)
	}
	if tr.Terminal() != "DFail" {
		t.Fatalf("terminal %q, want DFail: a timed-out result is routed as exit 124", tr.Terminal())
	}
}

func TestFakeRunnerGroupEmptyHoldsARerun(t *testing.T) {
	for empty, launches := range map[bool]int{false: 1, true: 2} {
		fr := wftest.NewFakeRunner()
		fr.SetStatus(0, wf.DetachedDead, wf.RunResult{})
		fr.SetGroupEmpty(0, empty)
		wftest.New(detachedDef(time.Hour), wftest.WithTB(t), wftest.WithEffectRunner(fr)).Run(wftest.Resume(), wftest.Resume())
		if got := len(fr.Started()); got != launches {
			t.Fatalf("group empty %v: launches = %d, want %d", empty, got, launches)
		}
		if got := len(fr.Terminated()); got != map[bool]int{false: 1, true: 0}[empty] {
			t.Fatalf("group empty %v: terminations = %d", empty, got)
		}
	}
}

// The group rule: a dead leader with live members is DeadLive, and Terminate
// must take the members too. A Terminate that only cleared the leader would
// leave GroupEmpty false and fail here.
func TestFakeRunnerTerminateKillsMembers(t *testing.T) {
	fr := wftest.NewFakeRunner()
	p, err := fr.Start(wf.DetachedRequest{ResultPath: "/r/effects/9.json", ProcPath: "/r/effects/9.proc", LogPath: "/r/effects/9.log"})
	if err != nil {
		t.Fatal(err)
	}
	if rep, _ := fr.Status(p, wf.DetachedRequest{}); rep.Status != wf.DetachedRunning {
		t.Fatalf("status = %v, want running while the leader lives", rep.Status)
	}
	fr.SetLeader(9, false)
	fr.SetMembers(9, 2)
	if rep, _ := fr.Status(p, wf.DetachedRequest{}); rep.Status != wf.DetachedDeadLive {
		t.Fatalf("status = %v, want dead-live: leader gone, two members", rep.Status)
	}
	if empty, _ := fr.GroupEmpty(p, wf.DetachedRequest{}); empty {
		t.Fatal("group empty with two live members")
	}
	if err := fr.Terminate(p, wf.DetachedRequest{}); err != nil {
		t.Fatal(err)
	}
	if empty, _ := fr.GroupEmpty(p, wf.DetachedRequest{}); !empty {
		t.Fatal("group not empty after Terminate: the members survived a leader-only kill")
	}
	if rep, _ := fr.Status(p, wf.DetachedRequest{}); rep.Status != wf.DetachedDead {
		t.Fatalf("status = %v, want dead once leader and members are gone", rep.Status)
	}
}

func TestFakeRunnerLaunchedFollowsProcSetting(t *testing.T) {
	fr := wftest.NewFakeRunner()
	req := wf.DetachedRequest{ResultPath: "/r/effects/4.json", ProcPath: "/r/effects/4.proc", LogPath: "/r/effects/4.log"}
	if _, ok, _ := fr.Launched(req); ok {
		t.Fatal("Launched found a proc record before any Start")
	}
	p, _ := fr.Start(req)
	if got, ok, _ := fr.Launched(req); !ok || got != p || p.StartTicks != 1001 {
		t.Fatalf("Launched = %+v, %v; Start = %+v, want the same process with ticks 1001", got, ok, p)
	}
	fr.SetProc(4, false)
	if _, ok, _ := fr.Launched(req); ok {
		t.Fatal("Launched found a proc record after SetProc(4, false)")
	}
}
