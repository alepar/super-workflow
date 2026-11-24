package wftest_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// These scenarios run loopDef on the real engine through wftest.Sim.

func TestSuccessPath(t *testing.T) {
	d, draft, _, done := loopDef()
	w := wftest.NewWorld()
	wftest.Set(w, git.Head("."), "abc123")
	tr := wftest.New(d, wftest.WithOptions(Opts{Autonomous: true}), wftest.WithWorld(w)).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	wftest.AssertRoute(t, tr, "DraftS", "CheckS", "ApproveS", "CIS", "DoneS")
	wftest.AssertTerminal(t, tr, done)
}

func TestRejectedThenAccepted(t *testing.T) {
	d, draft, _, _ := loopDef()
	tr := wftest.New(d).Run(
		wftest.Submit(draft, Draft{Verdict: Fail}),
		wftest.Submit(draft, Draft{Verdict: Pass}),
	)
	wftest.AssertClass(t, tr, "DraftS", wf.Refused)
	wftest.AssertClass(t, tr, "DraftS", wf.Accepted)
}

func TestNoProgressStalled(t *testing.T) {
	d, draft, _, _ := loopDef()
	// Each refusal spends an attempt (C5). The stall after two attempts routes
	// the stalled edge (C3: no halt and no delegated retry, whatever the run's
	// options).
	tr := wftest.New(d).Run(wftest.SubmitTimes(draft, 2, Draft{Verdict: Fail}))
	wftest.AssertRoute(t, tr, "DraftS", "GaveUpS")
}

// A crash between bd.create's intent and its result leaves the effect's
// outcome unknown; the engine never re-applies it, and with no
// OnClass(Uncertain) route on Draft the run waits for a manual settlement.
func TestCrashBetweenIntentAndResult(t *testing.T) {
	d, draft, _, _ := loopDef()
	w := wftest.NewWorld()
	wftest.EffectUnknown(w, "bd", "create", bd.IssueKey("loop", "draft"))
	tr := wftest.New(d, wftest.WithWorld(w)).Run(
		wftest.Submit(draft, Draft{Verdict: Pass}),
		wftest.CrashAfterIntent("bd", "create"),
		wftest.Resume(),
	)
	wftest.AssertJournal(t, tr,
		wf.JournalRunStarted,
		wf.JournalInstructionIssued,
		wf.JournalHandedOut, // the wave Next hands the unit out and claims it
		wf.JournalAttemptStarted,
		wf.JournalPromptFetched,
		wf.JournalSubmitted,
		wf.JournalAccepted,
		wf.JournalEffectIntent, // bd.create: no result follows
	)
	if got := wftest.Applied(w); len(got) != 0 {
		t.Errorf("applied = %v, want nothing: an unknown effect is never re-applied", got)
	}
}

func TestGateAnsweredByPolicy(t *testing.T) {
	d, draft, _, _ := loopDef()
	w := wftest.NewWorld()
	wftest.Set(w, git.Head("."), "abc123")
	tr := wftest.New(d, wftest.WithOptions(Opts{Autonomous: true}), wftest.WithWorld(w)).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	wftest.AssertJournal(t, tr,
		wf.JournalRunStarted,
		wf.JournalInstructionIssued, // DraftS
		wf.JournalHandedOut,         // the wave Next hands the unit out and claims it
		wf.JournalAttemptStarted,
		wf.JournalPromptFetched,
		wf.JournalSubmitted,
		wf.JournalAccepted,
		wf.JournalEffectIntent, // bd.create on "drafted"
		wf.JournalEffectResult,
		wf.JournalTransition,   // DraftS -> CheckS
		wf.JournalEffectIntent, // CheckS: the command
		wf.JournalEffectResult,
		wf.JournalAccepted, // CheckS: exit 0
		wf.JournalTransition,
		wf.JournalAutoAnswered, // ApproveS
		wf.JournalTransition,
		wf.JournalObserved, // CIS: git.head
		wf.JournalCompletedByObservation,
		wf.JournalTransition, // CIS -> DoneS
		wf.JournalRunEnded,
	)
}

// staleDef is a one-agent definition whose check observes git.Head: the
// submission is classed Stale when the head moved since the instruction was
// issued, and Review then stops in StaleS.
func staleDef() (*wf.Def, *wf.Agent[ReviewS, Draft]) {
	d := wf.New("stale-loop").Version(1)
	review := wf.NewAgent[ReviewS, Draft](d)
	stale := wf.NewTerminal[StaleS](d, wf.Stopped)
	ok := wf.NewTerminal[ReviewedS](d, wf.Success)
	toOK := review.To(ok, "reviewed")
	review.Spec(wf.PromptText("review"),
		wf.Retry(wf.Const(2), review.To(stale, "gave up")).OnClass(wf.Stale, review.To(stale, "head moved")),
		wf.Checkf("head-unchanged", func(Draft, wf.Ctx) error { return nil }).Observes(git.Head(".")))
	review.Route(func(Draft, wf.Ctx) wf.Edge[ReviewS] { return toOK }, toOK)
	return d, review
}

type (
	ReviewS   struct{}
	StaleS    struct{}
	ReviewedS struct{}
)

func TestStaleObservation(t *testing.T) {
	d, review := staleDef()
	w := wftest.NewWorld()
	wftest.Stale(w, git.Head("."))
	tr := wftest.New(d, wftest.WithWorld(w)).Run(wftest.Submit(review, Draft{Verdict: Pass}))
	wftest.AssertClass(t, tr, "ReviewS", wf.Stale)
	wftest.AssertRoute(t, tr, "ReviewS", "StaleS")
}

// The paged-coordinator scenarios run pagedDef: a Map of three agent children,
// then an agent, then a terminal.

type (
	pgFan     struct{}
	pgWork    struct{}
	pgFinal   struct{}
	pgOK      struct{}
	pgDone    struct{}
	pgPartial struct{}
)

type (
	pgItem struct{ N int }
	pgOut  struct{ OK bool }
	pgRes  struct{ N int }
)

func pagedDef() (*wf.Def, *wf.Agent[pgWork, pgOut], *wf.Agent[pgFinal, pgOut], *wf.Terminal[pgDone]) {
	cd := wf.New("pg-item")
	work := wf.NewAgent[pgWork, pgOut](cd)
	ok := wf.NewTerminal[pgOK](cd, wf.Success)
	toOK := work.To(ok, "ok")
	work.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[pgWork](wf.Const(1), toOK))
	work.Route(func(pgOut, wf.Ctx) wf.Edge[pgWork] { return toOK }, toOK)
	wf.Yield(ok, func(wf.Ctx) pgRes { return pgRes{} })
	child := wf.Typed[pgItem, pgRes](cd)

	d := wf.New("pg-paged")
	m := wf.NewMap[pgFan, pgItem, string, pgRes](d, wf.FromValues(pgItem{1}, pgItem{2}, pgItem{3}),
		func(i pgItem) string { return fmt.Sprint(i.N) }, child)
	final := wf.NewAgent[pgFinal, pgOut](d)
	partial := wf.NewTerminal[pgPartial](d, wf.Stopped)
	done := wf.NewTerminal[pgDone](d, wf.Success)
	m.Parallel(3).Join(wf.AllSettled(), m.To(final, "met"), m.To(partial, "partial"))
	toDone := final.To(done, "done")
	final.Spec(wf.PromptText("finish"), wf.Retry[pgFinal](wf.Const(1), toDone))
	final.Route(func(pgOut, wf.Ctx) wf.Edge[pgFinal] { return toDone }, toDone)
	return d, work, final, done
}

// pagedScript finishes pagedDef: three children, then the final agent.
func pagedScript(work *wf.Agent[pgWork, pgOut], final *wf.Agent[pgFinal, pgOut], children int) []wftest.Step {
	var steps []wftest.Step
	for range children {
		steps = append(steps, wftest.Submit(work, pgOut{OK: true}))
	}
	return append(steps, wftest.Submit(final, pgOut{OK: true}))
}

func TestCapOneTwoThreeSameTerminal(t *testing.T) {
	var routes [][]string
	for _, n := range []int{1, 2, 3} {
		d, work, final, done := pagedDef()
		tr := wftest.New(d, wftest.WithTB(t), wftest.Cap(n)).Run(pagedScript(work, final, 3)...)
		wftest.AssertTerminal(t, tr, done)
		if got := tr.Lost(); len(got) != 0 {
			t.Errorf("cap %d lost %v, want nothing", n, got)
		}
		if got := tr.Sessions(); !slices.Equal(got, []string{"wftest-1"}) {
			t.Errorf("cap %d sessions = %v, want [wftest-1]", n, got)
		}
		routes = append(routes, tr.Route())
	}
	for i := 1; i < len(routes); i++ {
		if !slices.Equal(routes[0], routes[i]) {
			t.Errorf("cap %d route %v differs from cap 1 route %v", i+1, routes[i], routes[0])
		}
	}
}

// killScenario kills the coordinator at at, starts a new one and finishes the
// run: the dead session's loss is recovered with no retry spent.
func killScenario(t *testing.T, at wftest.KillPoint, submitsBeforeNew int, wantCause string) {
	t.Helper()
	d, work, final, done := pagedDef()
	steps := []wftest.Step{wftest.KillCoordinator(at)}
	rest := pagedScript(work, final, 3)
	// AfterDone dies inside the first Submit, so that Submit precedes NewCoordinator.
	steps = append(steps, rest[:submitsBeforeNew]...)
	steps = append(steps, wftest.NewCoordinator())
	steps = append(steps, rest[submitsBeforeNew:]...)
	tr := wftest.New(d, wftest.WithTB(t), wftest.Cap(1)).Run(steps...)
	wftest.AssertTerminal(t, tr, done)
	if n := tr.RetriesSpent(); n != 0 {
		t.Errorf("retries spent = %d, want 0", n)
	}
	lost := tr.Lost()
	switch {
	case wantCause == "" && len(lost) != 0:
		t.Errorf("lost = %v, want nothing", lost)
	case wantCause != "" && (len(lost) != 1 || !strings.HasSuffix(lost[0], ": "+wantCause)):
		t.Errorf("lost = %v, want exactly one with cause %q", lost, wantCause)
	}
	if got := tr.Sessions(); !slices.Equal(got, []string{"wftest-1", "wftest-2"}) {
		t.Errorf("sessions = %v, want wftest-1 and wftest-2", got)
	}
}

func TestKillCoordinatorAfterNext(t *testing.T) {
	killScenario(t, wftest.AfterNext, 0, "")
}
func TestKillCoordinatorAfterSpawn(t *testing.T) {
	killScenario(t, wftest.AfterSpawn, 0, "")
}
func TestKillCoordinatorAfterStarted(t *testing.T) {
	killScenario(t, wftest.AfterStarted, 0, wf.LostCauseSpawn)
}
func TestKillCoordinatorWhileRunning(t *testing.T) {
	killScenario(t, wftest.WhileRunning, 0, wf.LostCauseOrphan)
}
func TestKillCoordinatorAfterDone(t *testing.T) { killScenario(t, wftest.AfterDone, 1, "") }

func TestStallReportedThenActive(t *testing.T) {
	d, work, final, done := pagedDef()
	tr := wftest.New(d, wftest.WithTB(t), wftest.Cap(2)).Run(
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Stall("pgWork"),
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(final, pgOut{OK: true}),
	)
	wftest.AssertTerminal(t, tr, done)
	var stalled, active int
	for _, r := range tr.Journal() {
		if r.Kind != wf.JournalHeartbeat {
			continue
		}
		var h wf.HeartbeatData
		if err := json.Unmarshal(r.Data, &h); err != nil {
			t.Fatal(err)
		}
		switch {
		case h.Status == "stalled":
			stalled++
		case h.Status == "active" && h.By == "coordinator":
			active++
		}
	}
	if stalled != 1 || active != 1 {
		t.Errorf("stalled heartbeats = %d, coordinator active heartbeats = %d, want 1 and 1", stalled, active)
	}
	if got := tr.Lost(); len(got) != 0 {
		t.Errorf("lost = %v, want nothing: a stall is never a loss", got)
	}
}

func TestLoseSessionReissues(t *testing.T) {
	d, work, final, done := pagedDef()
	tr := wftest.New(d, wftest.WithTB(t), wftest.Cap(3)).Run(
		wftest.Submit(work, pgOut{OK: true}),
		wftest.LoseSession(),
		wftest.NewCoordinator(),
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(final, pgOut{OK: true}),
	)
	wftest.AssertTerminal(t, tr, done)
	lost := tr.Lost()
	if len(lost) != 2 {
		t.Fatalf("lost = %v, want the two orphans of the vanished session", lost)
	}
	for _, l := range lost {
		if !strings.HasSuffix(l, ": "+wf.LostCauseOrphan) {
			t.Errorf("lost %q, want cause %q", l, wf.LostCauseOrphan)
		}
	}
	if n := tr.RetriesSpent(); n != 0 {
		t.Errorf("retries spent = %d, want 0", n)
	}
}

// The dynamic-source scenarios run globDef: a Requery Map with one agent child
// per file of a directory, read through fsx.Glob and answered by World.OnRead.

type (
	gbFan   struct{}
	gbWork  struct{}
	gbFinal struct{}
	gbOK    struct{}
	gbDone  struct{}
	gbPart  struct{}
)

func globDef() (*wf.Def, *wf.Agent[gbWork, pgOut], *wf.Agent[gbFinal, pgOut], *wf.Terminal[gbDone]) {
	cd := wf.New("gb-file")
	work := wf.NewAgent[gbWork, pgOut](cd)
	ok := wf.NewTerminal[gbOK](cd, wf.Success)
	toOK := work.To(ok, "ok")
	work.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[gbWork](wf.Const(1), toOK))
	work.Route(func(pgOut, wf.Ctx) wf.Edge[gbWork] { return toOK }, toOK)
	wf.Yield(ok, func(wf.Ctx) pgRes { return pgRes{} })
	child := wf.Typed[fsx.File, pgRes](cd)

	d := wf.New("gb-glob")
	m := wf.NewMap[gbFan, fsx.File, string, pgRes](d, wf.FromObservation(fsx.Glob("/dir", "*.json")),
		func(f fsx.File) string { return f.Name }, child)
	final := wf.NewAgent[gbFinal, pgOut](d)
	partial := wf.NewTerminal[gbPart](d, wf.Stopped)
	done := wf.NewTerminal[gbDone](d, wf.Success)
	m.Requery().Join(wf.AllSettled(), m.To(final, "met"), m.To(partial, "partial"))
	toDone := final.To(done, "done")
	final.Spec(wf.PromptText("finish"), wf.Retry[gbFinal](wf.Const(1), toDone))
	final.Route(func(pgOut, wf.Ctx) wf.Edge[gbFinal] { return toDone }, toDone)
	return d, work, final, done
}

// globWorld answers fsx.Glob from a file list the test changes, calling change
// with the read count before each answer.
func globWorld(files *[]string, change func(read int)) *wftest.World {
	w := wftest.NewWorld()
	reads := 0
	wftest.OnRead(w, func(d wf.ObservationDesc) (json.RawMessage, bool) {
		if d.Adapter != "fsx" || d.Op != "glob" {
			return nil, false
		}
		reads++
		change(reads)
		out := []fsx.File{}
		for _, n := range *files {
			out = append(out, fsx.File{Name: n, Path: "/dir/" + n})
		}
		raw, _ := json.Marshal(out)
		return raw, true
	})
	return w
}

func withdrawnOf(t *testing.T, tr *wftest.Trace) []wf.WithdrawnData {
	t.Helper()
	var out []wf.WithdrawnData
	for _, r := range tr.Journal() {
		if r.Kind != wf.JournalWithdrawn {
			continue
		}
		var d wf.WithdrawnData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// A file that appears while the Map runs gets a child: the run needs three
// work submissions although the directory held one file when it started.
func TestGlobMapGainsAFileWhileRunning(t *testing.T) {
	d, work, final, done := globDef()
	files := []string{"a.json"}
	w := globWorld(&files, func(read int) {
		if read == 3 && len(files) == 1 {
			files = append(files, "b.json", "c.json")
		}
	})
	tr := wftest.New(d, wftest.WithTB(t), wftest.WithWorld(w), wftest.Cap(1)).Run(
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(final, pgOut{OK: true}),
	)
	wftest.AssertTerminal(t, tr, done)
	if len(files) != 3 {
		t.Fatal("the directory never grew: the scenario did not exercise the requery")
	}
	if wd := withdrawnOf(t, tr); len(wd) != 0 {
		t.Errorf("withdrawn %+v, want nothing: the set only grew", wd)
	}
}

// A file that leaves the directory before its child was handed out is
// withdrawn, never handed out stale: two submissions finish the run.
func TestGlobMapWithdrawsAFileThatLeft(t *testing.T) {
	d, work, final, done := globDef()
	files := []string{"a.json", "b.json", "c.json"}
	w := globWorld(&files, func(read int) {
		if read == 3 && len(files) == 3 {
			files = []string{"a.json", "c.json"}
		}
	})
	tr := wftest.New(d, wftest.WithTB(t), wftest.WithWorld(w), wftest.Cap(1)).Run(
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(final, pgOut{OK: true}),
	)
	wftest.AssertTerminal(t, tr, done)
	wd := withdrawnOf(t, tr)
	if len(wd) != 1 || !strings.Contains(wd[0].Name, "b.json") {
		t.Errorf("withdrawn %+v, want exactly b.json's unit", wd)
	}
}

// Coordinator 1 starts a unit and dies. Coordinator 2 reports it died, which
// loses the unit without spending a retry, and the unit is reissued under a
// new name. The orphan, still running somewhere, finishes late: its Done is
// rejected and changes nothing, and the reissue's Done finishes the run.
func TestOrphanCompletesAfterCrash(t *testing.T) {
	h := pagedRun(t, false)
	orphan := h.units("1", 1, false)
	if len(orphan) != 1 {
		t.Fatalf("coordinator 1 holds %d units, want 1", len(orphan))
	}
	// Coordinator 1 dies here; coordinator 2 runs `died --session 1`.
	h.must(h.e.Heartbeat(h.id, wf.HeartbeatTarget{Session: "1"}, false))
	reissue := h.units("2", 1, false)
	if len(reissue) != 1 || reissue[0].Name == orphan[0].Name {
		t.Fatalf("reissue = %+v, want one unit under a new name (orphan was %s)", reissue, orphan[0].Name)
	}
	before := len(h.records())
	if _, err := h.e.Done(h.id, orphan[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("the orphan's late Done = %v, want ErrRejected", err)
	}
	for _, r := range h.records()[before:] {
		if r.Kind == wf.JournalAccepted || r.Kind == wf.JournalTransition {
			t.Errorf("the rejected Done journaled a %s", r.Kind)
		}
	}
	_, err := h.e.Done(h.id, reissue[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
	h.must(err)
	h.pump("2")
	if got, want := h.outcome(), "ended/pgDone retries=0"; got != want {
		t.Errorf("outcome = %q, want %q", got, want)
	}
}

// ValidateAt checks an output as done would and spends nothing: the refused
// draft is fixed on attempt 1.
func TestValidateAtSpendsNoAttempt(t *testing.T) {
	d, draft, _, _ := loopDef()
	tr := wftest.New(d).Run(wftest.ValidateAt("DraftS", Draft{Verdict: Fail}), wftest.Submit(draft, Draft{Verdict: Pass}))
	v := tr.Validations()
	if len(v) != 1 || v[0].Result.Class != wf.Refused || len(v[0].Result.Refusal) == 0 {
		t.Fatalf("validations = %+v, want one refusal", v)
	}
	for _, r := range tr.Journal() {
		if r.Kind == wf.JournalRefused {
			t.Fatalf("validate journaled a refusal: %+v", r)
		}
	}
	wftest.AssertClass(t, tr, "DraftS", wf.Accepted)
}
