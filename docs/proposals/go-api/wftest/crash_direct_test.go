package wftest_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// The crash points a Sim script cannot reach (a bad token, a legacy record, a
// definition change, a lost session) drive the engine directly. A drv is a
// coordinator with its own fake clock over a CrashJournal: at runs one action
// with the crash armed, then restarts the engine on the same journal, the way a
// new process would.

type drv struct {
	t     *testing.T
	crash bool
	cj    *wftest.CrashJournal
	reg   *wf.Adapters
	world *wftest.World
	now   time.Time
	def   *wf.Def
	opts  wf.EngineOptions
	e     wf.Executor
	id    wf.RunID
}

// newDrv starts a run of d (with options o) on a fresh journal. With crash set,
// at arms and fires crashes; without it at just runs the action, so the same
// scenario function yields the uncrashed reference outcome.
func newDrv(t *testing.T, crash bool, d *wf.Def, o any, mut ...func(*wf.EngineOptions)) *drv {
	t.Helper()
	h := &drv{t: t, crash: crash, cj: wftest.NewCrashJournal(wf.MemJournal()), reg: wf.NewAdapters(), world: wftest.NewWorld(),
		now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), def: d}
	if err := h.reg.Register(h.world); err != nil {
		t.Fatal(err)
	}
	h.opts = wf.EngineOptions{Defs: []*wf.Def{d}, RunID: "crash-run", Clock: func() time.Time { return h.now }}
	for _, m := range mut {
		m(&h.opts)
	}
	h.e = wf.NewEngine(h.cj, h.reg, h.opts)
	id, err := h.e.Run(d, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	h.id = id
	return h
}

// restart is a new process: a fresh engine over the same journal, advanced past
// every hand-out, spawn and backoff window the dead one left open.
func (h *drv) restart() {
	h.t.Helper()
	h.now = h.now.Add(10 * time.Minute)
	h.e = wf.NewEngine(h.cj, h.reg, h.opts)
	if _, err := h.e.Run(h.def, nil, nil); err != nil {
		h.t.Fatal(err)
	}
}

// at runs f. With crash set it arms kind first, requires the crash to fire in
// f and restarts the engine; f's own errors are the caller's to check, since a
// crashed f stops where the crash cut it.
func (h *drv) at(kind wf.JournalKind, f func()) {
	h.t.Helper()
	if !h.crash {
		f()
		return
	}
	h.cj.Arm(kind)
	if !wftest.Survive(f) {
		h.t.Fatalf("crash point %s was not reached", kind)
	}
	h.restart()
}

// atBefore is at for a death just before kind's append: the record is lost.
func (h *drv) atBefore(kind wf.JournalKind, f func()) {
	h.t.Helper()
	if !h.crash {
		f()
		return
	}
	h.cj.ArmBefore(kind)
	if !wftest.Survive(f) {
		h.t.Fatalf("crash point before %s was not reached", kind)
	}
	h.restart()
}

func (h *drv) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *drv) next() []wf.Instruction {
	h.t.Helper()
	ins, err := h.e.Next(h.id, "")
	h.must(err)
	return slices.DeleteFunc(slices.Clone(ins), func(in wf.Instruction) bool { return in.Kind == wf.InstructionEnded })
}

func (h *drv) records() []wf.JournalRecord {
	h.t.Helper()
	unlock, err := h.cj.Lock(h.id)
	h.must(err)
	defer unlock()
	recs, err := h.cj.Records(h.id)
	h.must(err)
	return recs
}

// outcome is what a crash must not change: how the run ended, which terminal
// and how many retries it spent.
func (h *drv) outcome() string {
	h.t.Helper()
	st, err := h.e.Status(h.id)
	h.must(err)
	spent := 0
	for _, r := range h.records() {
		var d wf.RefusedData
		if r.Kind == wf.JournalRefused && json.Unmarshal(r.Data, &d) == nil && d.Spent {
			spent++
		}
	}
	return fmt.Sprintf("%s/%s retries=%d", st.Phase, st.Terminal, spent)
}

// append writes a record of kind to the root instance the way an external
// writer (an older engine, an operator tool) would.
func (h *drv) append(kind wf.JournalKind, data any) {
	h.t.Helper()
	raw, err := json.Marshal(data)
	h.must(err)
	unlock, err := h.cj.Lock(h.id)
	h.must(err)
	defer unlock()
	h.must(h.cj.Append(wf.JournalRecord{Kind: kind, Run: h.id, DefHash: h.def.Hash(), At: h.now, Data: raw}))
}

// finish answers every open instruction with out until none is left.
func (h *drv) finish(out string) {
	h.t.Helper()
	for range 50 {
		ins := h.next()
		if len(ins) == 0 {
			return
		}
		for _, in := range ins {
			_, err := h.e.Submit(h.id, wf.Submission{Token: in.Token, Child: in.Child, Output: json.RawMessage(out)})
			h.must(err)
		}
	}
	h.t.Fatal("finish did not settle")
}

// scenario runs fn uncrashed and crashed and requires equal outcomes.
func scenario(kind wf.JournalKind, build func(t *testing.T, crash bool) *drv, fn func(h *drv)) func(t *testing.T) {
	return func(t *testing.T) {
		t.Helper()
		ref := build(t, false)
		fn(ref)
		want := ref.outcome()
		h := build(t, true)
		fn(h)
		if got := h.outcome(); got != want {
			t.Errorf("after a crash at %s the run ended %q, want %q (uncrashed)", kind, got, want)
		}
		if fired := h.cj.Fired(); len(fired) == 0 || fired[0] != kind {
			t.Errorf("crash fired at %v, want %s", fired, kind)
		}
	}
}

// Definitions. One agent to a terminal is the base; each odd point adds what
// it needs.

type (
	crA    struct{}
	crDone struct{}
	crOut  struct{ OK bool }
)

func crSimpleDef() *wf.Def {
	d := wf.New("cr-simple").Version(1)
	a := wf.NewAgent[crA, crOut](d)
	done := wf.NewTerminal[crDone](d, wf.Success)
	next := a.To(done, "done")
	a.Spec(wf.PromptText("do it"), wf.Retry[crA](wf.Const(2), next))
	a.Route(func(crOut, wf.Ctx) wf.Edge[crA] { return next }, next)
	return d
}

func simpleRun(t *testing.T, crash bool) *drv { return newDrv(t, crash, crSimpleDef(), nil) }

// A submission with a token the run never issued is rejected and audited; the
// right token then finishes the run.
func rejectedPoint(h *drv) {
	h.next()
	h.at(wf.JournalRejected, func() {
		_, err := h.e.Submit(h.id, wf.Submission{Token: "no-such-token", Output: json.RawMessage(`{}`)})
		if err != nil && !errors.Is(err, wf.ErrRejected) {
			h.t.Fatal(err)
		}
	})
	h.finish(`{"OK":true}`)
}

type (
	crWork struct{}
	crEnd  struct{}
)

// crComputeDef is one compute that fails until *fail is cleared.
func crComputeDef(fail *bool) *wf.Def {
	d := wf.New("cr-compute").Version(1)
	c := wf.NewCompute[crWork, int](d, func(wf.Ctx) (int, error) {
		if *fail {
			return 0, errors.New("compute broke")
		}
		return 1, nil
	})
	end := wf.NewTerminal[crEnd](d, wf.Success)
	done := c.To(end, "done")
	c.Route(func(int, wf.Ctx) wf.Edge[crWork] { return done }, done)
	return d
}

type (
	crMOld struct{}
	crMNew struct{}
	crMEnd struct{}
)

func crMigDef(v2 bool) *wf.Def {
	d := wf.New("cr-mig")
	if v2 {
		d.Version(2)
		a := wf.NewAgent[crMNew, crOut](d)
		end := wf.NewTerminal[crMEnd](d, wf.Success)
		next := a.To(end, "done")
		a.Spec(wf.PromptText("new"), wf.Retry[crMNew](wf.Const(2), next))
		a.Route(func(crOut, wf.Ctx) wf.Edge[crMNew] { return next }, next)
		return d
	}
	a := wf.NewAgent[crMOld, crOut](d)
	end := wf.NewTerminal[crMEnd](d, wf.Success)
	next := a.To(end, "done")
	a.Spec(wf.PromptText("old"), wf.Retry[crMOld](wf.Const(2), next))
	a.Route(func(crOut, wf.Ctx) wf.Edge[crMOld] { return next }, next)
	return d
}

type (
	crSA   struct{}
	crSB   struct{}
	crSEnd struct{}
)

// crScopedDef is A -> B -> end where A's edge increments round, which resets
// the counter scoped to it.
func crScopedDef() *wf.Def {
	d := wf.New("cr-scoped").Version(1)
	round := d.Counter("round")
	re := d.Counter("redispatch", wf.ScopedTo(round))
	a := wf.NewCommand[crSA](d, "a")
	b := wf.NewCommand[crSB](d, "b")
	end := wf.NewTerminal[crSEnd](d, wf.Success)
	ab := a.To(b, "next").With(re.Inc()).With(round.Inc())
	a.Route(func(wf.RunResult, wf.Ctx) wf.Edge[crSA] { return ab }, ab)
	bd := b.To(end, "done")
	b.Route(func(wf.RunResult, wf.Ctx) wf.Edge[crSB] { return bd }, bd)
	wf.MaxVisits(wf.Const(1).Plus(wf.Count(re)), bd)
	return d
}

type (
	crOffA     struct{}
	crOffDone  struct{}
	crOffBad   struct{}
	crOffOther struct{}
	crOffItem  struct{ Lang string }
	crOffRes   struct{ OK bool }
)

// crRouteOffDef routes outside its declared candidates.
func crRouteOffDef() *wf.Def {
	d := wf.New("cr-off")
	a := wf.NewAgent[crOffA, crOut](d)
	done := wf.NewTerminal[crOffDone](d, wf.Success)
	bad := wf.NewTerminal[crOffBad](d, wf.Stopped)
	other := wf.NewTerminal[crOffOther](d, wf.Stopped)
	a.Spec(wf.PromptText("p"), wf.Retry[crOffA](wf.Const(2), a.To(bad, "x")))
	listed := a.To(done, "listed")
	unlisted := a.To(other, "unlisted")
	a.Route(func(crOut, wf.Ctx) wf.Edge[crOffA] { return unlisted }, listed)
	return d
}

// crPromptOffDef resolves its prompt to a path outside its declared ones.
func crPromptOffDef() *wf.Def {
	d := wf.New("cr-poff")
	wf.Typed[crOffItem, crOffRes](d)
	a := wf.NewAgent[crOffA, crOut](d)
	done := wf.NewTerminal[crOffDone](d, wf.Success)
	bad := wf.NewTerminal[crOffBad](d, wf.Stopped)
	a.Spec(wf.PromptFor(func(crOffItem) string { return "prompts/nope.md" }, []string{"prompts/a.md", "prompts/b.md"}),
		wf.Retry[crOffA](wf.Const(2), a.To(bad, "x")))
	next := a.To(done, "done")
	a.Route(func(crOut, wf.Ctx) wf.Edge[crOffA] { return next }, next)
	return d
}

type (
	crBWork  struct{}
	crBDone  struct{}
	crBPause struct{}
	crBFail  struct{}
	crBOut   struct{ Down bool }
)

// crBackoffDef is an agent whose Unavailable refusals back off 10s, then 20s.
func crBackoffDef() *wf.Def {
	d := wf.New("cr-backoff")
	a := wf.NewAgent[crBWork, crBOut](d)
	done := wf.NewTerminal[crBDone](d, wf.Success)
	pause := wf.NewTerminal[crBPause](d, wf.Stopped)
	fail := wf.NewTerminal[crBFail](d, wf.Stopped)
	a.Spec(wf.PromptText("Work."),
		wf.Retry(wf.Const(5), a.To(fail, "exhausted")).
			OnClassBackoff(wf.Unavailable, a.To(pause, "pause"), 10*time.Second, 20*time.Second),
		wf.Checkf("up", func(o crBOut, _ wf.Ctx) error {
			if o.Down {
				return fmt.Errorf("%w: tool", wf.ErrUnavailable)
			}
			return nil
		}))
	ok := a.To(done, "ok")
	a.Route(func(crBOut, wf.Ctx) wf.Edge[crBWork] { return ok }, ok)
	return d
}

// Map definitions: pagedDef for the plain units, crFanDef for a quorum join.
type (
	crFan    struct{}
	crFWork  struct{}
	crFMerge struct{}
	crFPart  struct{}
)

func crFanDef(items int, join wf.JoinPolicy) *wf.Def {
	cd := wf.New("cr-item")
	work := wf.NewAgent[crFWork, pgOut](cd)
	ok := wf.NewTerminal[pgOK](cd, wf.Success)
	toOK := work.To(ok, "ok")
	work.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[crFWork](wf.Const(1), toOK))
	work.Route(func(pgOut, wf.Ctx) wf.Edge[crFWork] { return toOK }, toOK)
	wf.Yield(ok, func(wf.Ctx) pgRes { return pgRes{} })
	child := wf.Typed[pgItem, pgRes](cd)

	d := wf.New("cr-quorum")
	var xs []pgItem
	for i := 1; i <= items; i++ {
		xs = append(xs, pgItem{i})
	}
	m := wf.NewMap[crFan, pgItem, string, pgRes](d, wf.FromValues(xs...), func(i pgItem) string { return fmt.Sprint(i.N) }, child)
	merged := wf.NewTerminal[crFMerge](d, wf.Success)
	partial := wf.NewTerminal[crFPart](d, wf.Stopped)
	m.Parallel(items).Join(join, m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// units is what a coordinator session does with a hand-out: spawn, start and
// prompt each unit as session, and finish them when done is set.
func (h *drv) units(session string, n int, done bool) []wf.Instruction {
	h.t.Helper()
	ins, err := h.e.HandOut(h.id, n)
	h.must(err)
	var units []wf.Instruction
	for _, in := range ins {
		if in.Name == "" {
			continue
		}
		units = append(units, in)
		h.must(h.e.Started(h.id, in.Name, wf.Claim{Harness: "wftest", Session: session, Agent: "agent-" + in.Name}))
		_, err := h.e.Prompt(h.id, in.Name)
		h.must(err)
		if done {
			_, err := h.e.Done(h.id, in.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
			h.must(err)
		}
	}
	return units
}

// pump is one coordinator session that runs the paged run to its end: it
// reports each dead session first, then hands out, starts and finishes units,
// letting the clock pass the windows whenever nothing is handed.
func (h *drv) pump(session string, dead ...string) {
	h.t.Helper()
	for _, d := range dead {
		h.must(h.e.Heartbeat(h.id, wf.HeartbeatTarget{Session: d}, false))
	}
	for range 60 {
		st, err := h.e.Status(h.id)
		if err != nil || st.Phase != wf.RunRunning {
			h.must(err)
			return
		}
		// A lease that ran out asks the coordinator; the pump's children
		// are gone, so it answers died.
		for _, u := range st.Units {
			if u.State == "stalled" {
				h.must(h.e.Heartbeat(h.id, wf.HeartbeatTarget{Name: u.Name}, false))
			}
		}
		if len(h.units(session, 3, true)) == 0 {
			h.now = h.now.Add(5 * time.Minute)
		}
	}
	h.t.Fatal("pump did not settle")
}

func pagedRun(t *testing.T, crash bool) *drv {
	d, _, _, _ := pagedDef()
	return newDrv(t, crash, d, nil)
}

// pagedPoint crashes the first session's pump at kind; a second session
// reports it dead and finishes the run.
func pagedPoint(kind wf.JournalKind) func(t *testing.T) {
	return scenario(kind, pagedRun, func(h *drv) {
		h.at(kind, func() { h.pump("s1") })
		h.pump("s2", "s1")
	})
}

// deadPoint leaves a session's units running, then crashes the next session's
// recovery of them at kind; a third session finishes the run.
func deadPoint(kind wf.JournalKind) func(t *testing.T) {
	return scenario(kind, pagedRun, func(h *drv) {
		h.units("s1", 2, false)
		h.at(kind, func() { h.pump("s2", "s1") })
		h.pump("s3", "s1", "s2")
	})
}

// quorumRun is three seats under Quorum(1) with every seat handed out.
func quorumRun(t *testing.T, crash bool) *drv {
	return newDrv(t, crash, crFanDef(3, wf.Quorum(1)), nil)
}

// quorumPoint decides the join with the first Done: the handed seat is
// withdrawn and the started one's Done is a late vote.
func quorumPoint(kind wf.JournalKind, late bool) func(t *testing.T) {
	return scenario(kind, quorumRun, func(h *drv) {
		seats, err := h.e.HandOut(h.id, 3)
		h.must(err)
		if len(seats) != 3 {
			h.t.Fatalf("hand-out gave %d seats, want 3", len(seats))
		}
		for _, s := range seats[:2] {
			h.must(h.e.Started(h.id, s.Name, wf.Claim{Harness: "wftest", Session: "s1", Agent: "a-" + s.Name}))
			_, err := h.e.Prompt(h.id, s.Name)
			h.must(err)
		}
		done := func(name string) func() {
			return func() {
				_, err := h.e.Done(h.id, name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
				h.must(err)
			}
		}
		if late {
			done(seats[0].Name)()
			h.at(kind, done(seats[1].Name))
		} else {
			h.at(kind, done(seats[0].Name))
		}
		h.finish(`{"OK":true}`)
	})
}

func init() {
	crashPoints[wf.JournalRejected] = scenario(wf.JournalRejected, simpleRun, rejectedPoint)
	// The kinds below are written by no current verb: older engines and operator
	// tools wrote them, and the fold still reads them. The crash is a death
	// right after such a writer's append.
	crashPoints[wf.JournalNote] = scenario(wf.JournalNote, simpleRun, func(h *drv) {
		h.at(wf.JournalNote, func() { h.append(wf.JournalNote, wf.NoteData{Text: "operator note"}) })
		h.finish(`{"OK":true}`)
	})
	crashPoints[wf.JournalPostDoneMutation] = scenario(wf.JournalPostDoneMutation, simpleRun, func(h *drv) {
		h.at(wf.JournalPostDoneMutation, func() {
			h.append(wf.JournalPostDoneMutation, wf.PostDoneMutationData{Unit: "u", TaskRef: "task-x", RefTarget: "b", Commit: "a"})
		})
		h.finish(`{"OK":true}`)
	})
	crashPoints[wf.JournalPaused] = scenario(wf.JournalPaused, simpleRun, func(h *drv) {
		h.at(wf.JournalPaused, func() { h.append(wf.JournalPaused, wf.PausedData{Reason: "operator"}) })
		if ins := h.next(); len(ins) != 0 {
			h.t.Fatalf("a paused run issued %+v", ins)
		}
		h.append(wf.JournalResumed, wf.ResumedData{})
		h.finish(`{"OK":true}`)
	})
	crashPoints[wf.JournalResumed] = scenario(wf.JournalResumed, simpleRun, func(h *drv) {
		h.append(wf.JournalPaused, wf.PausedData{Reason: "operator"})
		h.at(wf.JournalResumed, func() { h.append(wf.JournalResumed, wf.ResumedData{}) })
		h.finish(`{"OK":true}`)
	})
	crashPoints[wf.JournalLeaseClaimed] = scenario(wf.JournalLeaseClaimed, simpleRun, func(h *drv) {
		h.next()
		h.at(wf.JournalLeaseClaimed, func() {
			h.append(wf.JournalLeaseClaimed, wf.LeaseClaimedData{Holder: "old-engine", Expires: h.now.Add(time.Hour)})
		})
		h.finish(`{"OK":true}`)
	})
	// An expired lease is the one of these the engine still writes: it
	// re-issues the attempt, spending no retry.
	crashPoints[wf.JournalLeaseExpired] = scenario(wf.JournalLeaseExpired, simpleRun, func(h *drv) {
		// A hand-out of n = 0 issues the instruction without claiming it
		// as a unit, the only state an old lease applies to.
		_, err := h.e.HandOut(h.id, 0)
		h.must(err)
		h.append(wf.JournalLeaseClaimed, wf.LeaseClaimedData{Holder: "old-engine", Expires: h.now.Add(time.Minute)})
		h.now = h.now.Add(2 * time.Minute)
		h.at(wf.JournalLeaseExpired, func() { _, err := h.e.HandOut(h.id, 0); h.must(err) })
		h.finish(`{"OK":true}`)
	})
	crashPoints[wf.JournalRetried] = func(t *testing.T) {
		run := func(crash bool) string {
			fail := true
			h := newDrv(t, crash, crComputeDef(&fail), nil)
			h.next() // the compute fails and the run blocks
			fail = false
			h.at(wf.JournalRetried, func() { h.must(h.e.Retry(h.id, "", false)) })
			h.next()
			return h.outcome()
		}
		if got, want := run(true), run(false); got != want {
			t.Errorf("after a crash at retried the run ended %q, want %q", got, want)
		}
	}
	// unit_held is old-journal only (C2 removed the held fallback): a hold an
	// older engine journaled still folds, and wf retry releases it.
	crashPoints[wf.JournalUnitHeld] = scenario(wf.JournalUnitHeld, simpleRun, func(h *drv) {
		unit := h.units("s1", 1, false)[0]
		h.at(wf.JournalUnitHeld, func() {
			h.append(wf.JournalUnitHeld, wf.UnitHeldData{Name: unit.Name, Cause: "BLOCKED_AUTH"})
		})
		h.must(h.e.Retry(h.id, "", false))
		h.finish(`{"OK":true}`)
	})
	// submission_refused is old-journal only (C5: a refusal spends the
	// attempt): an open refusal an older engine journaled still folds, and
	// the same child's fixed output is accepted on that attempt.
	crashPoints[wf.JournalSubmissionRefused] = scenario(wf.JournalSubmissionRefused, simpleRun, func(h *drv) {
		in := h.next()[0]
		h.at(wf.JournalSubmissionRefused, func() {
			h.append(wf.JournalSubmissionRefused, wf.SubmissionRefusedData{Token: in.Token, Reasons: []string{"undecodable submission: old"}, Left: 2, Fingerprint: "decode:old"})
		})
		h.finish(`{"OK":true}`)
	})
	crashPoints[wf.JournalMigrated] = func(t *testing.T) {
		run := func(crash bool) string {
			v1, v2 := crMigDef(false), crMigDef(true)
			v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: map[string]string{"crMOld": "crMNew"}})
			h := newDrv(t, crash, v1, nil)
			h.next()
			h.def, h.opts.Defs = v2, []*wf.Def{v2}
			h.e = wf.NewEngine(h.cj, h.reg, h.opts)
			if crash {
				h.cj.Arm(wf.JournalMigrated)
				if !wftest.Survive(func() { _, err := h.e.Run(v2, nil, nil); h.must(err) }) {
					t.Fatal("crash point migrated was not reached")
				}
				h.restart()
			} else if _, err := h.e.Run(v2, nil, nil); err != nil {
				t.Fatal(err)
			}
			h.finish(`{"OK":true}`)
			return h.outcome()
		}
		if got, want := run(true), run(false); got != want {
			t.Errorf("after a crash at migrated the run ended %q, want %q", got, want)
		}
	}
	crashPoints[wf.JournalScopeReset] = scenario(wf.JournalScopeReset,
		func(t *testing.T, crash bool) *drv { return newDrv(t, crash, crScopedDef(), nil) },
		func(h *drv) {
			h.at(wf.JournalScopeReset, func() { h.next() })
			h.next()
		})
	crashPoints[wf.JournalRouteOffCandidates] = scenario(wf.JournalRouteOffCandidates,
		func(t *testing.T, crash bool) *drv { return newDrv(t, crash, crRouteOffDef(), nil) },
		func(h *drv) {
			in := h.next()[0]
			h.at(wf.JournalRouteOffCandidates, func() {
				_, err := h.e.Submit(h.id, wf.Submission{Token: in.Token, Output: json.RawMessage(`{}`)})
				h.must(err)
			})
			h.next()
		})
	crashPoints[wf.JournalPromptOffCandidates] = scenario(wf.JournalPromptOffCandidates,
		func(t *testing.T, crash bool) *drv { return newDrv(t, crash, crPromptOffDef(), nil) },
		func(h *drv) {
			h.at(wf.JournalPromptOffCandidates, func() { h.next() })
			h.next()
		})
	crashPoints[wf.JournalBackoffScheduled] = scenario(wf.JournalBackoffScheduled,
		func(t *testing.T, crash bool) *drv { return newDrv(t, crash, crBackoffDef(), nil) },
		func(h *drv) {
			in := h.next()[0]
			h.at(wf.JournalBackoffScheduled, func() {
				_, err := h.e.Submit(h.id, wf.Submission{Token: in.Token, Output: json.RawMessage(`{"Down":true}`)})
				h.must(err)
			})
			h.now = h.now.Add(time.Minute) // past the 10s backoff
			h.finish(`{"Down":false}`)
		})
	crashPoints[wf.JournalBudgetStopped] = scenario(wf.JournalBudgetStopped,
		func(t *testing.T, crash bool) *drv {
			d, _, _, _ := loopDef()
			return newDrv(t, crash, d, nil, func(o *wf.EngineOptions) { o.RunBudget = wf.RunBudget{Turns: 1} })
		},
		func(h *drv) {
			in := h.next()[0]
			fail := wf.Submission{Token: in.Token, Output: json.RawMessage(`{"Verdict":"fail"}`)}
			// The refusal spends the attempt (C5) and would reissue the
			// draft, but the turn budget is spent, so the reissue stops the
			// run instead.
			h.at(wf.JournalBudgetStopped, func() {
				_, err := h.e.Submit(h.id, fail)
				h.must(err)
			})
			h.next()
		})
	// The coordinator reports one unit dead: the heartbeat is durable before
	// the loss, and the next session reports the rest.
	crashPoints[wf.JournalHeartbeat] = scenario(wf.JournalHeartbeat, pagedRun, func(h *drv) {
		units := h.units("s1", 2, false)
		h.at(wf.JournalHeartbeat, func() {
			h.must(h.e.Heartbeat(h.id, wf.HeartbeatTarget{Name: units[0].Name}, false))
		})
		h.pump("s2", "s1")
	})
	crashPoints[wf.JournalAttemptLost] = deadPoint(wf.JournalAttemptLost)
	// The child's prompt claims the unit; the coordinator's started then
	// attaches its metadata late.
	crashPoints[wf.JournalMetadataAttached] = scenario(wf.JournalMetadataAttached, pagedRun, func(h *drv) {
		ins, err := h.e.HandOut(h.id, 1)
		h.must(err)
		for _, in := range ins {
			if in.Name == "" {
				continue
			}
			_, err := h.e.Prompt(h.id, in.Name)
			h.must(err)
			h.at(wf.JournalMetadataAttached, func() {
				h.must(h.e.Started(h.id, in.Name, wf.Claim{Harness: "wftest", Session: "s1", Agent: "agent-" + in.Name}))
			})
		}
		h.pump("s2", "s1")
	})
	// A child reports a sandbox escalation with its unit (loud-failures D1):
	// the degraded record is durable across the crash and status still lists it.
	// F1: an ended child's recovery offer, then the coordinator's restart,
	// survive a death right after either record.
	crashPoints[wf.JournalRecoveryOffered] = scenario(wf.JournalRecoveryOffered, simpleRun, func(h *drv) {
		unit := h.units("s1", 1, false)[0]
		h.at(wf.JournalRecoveryOffered, func() {
			h.append(wf.JournalRecoveryOffered, wf.RecoveryOfferedData{Unit: unit.Name, Cause: "the child ended with no done"})
		})
		h.must(h.e.Recover(h.id, unit.Name, wf.RecoveryRestart))
		h.finish(`{"OK":true}`)
	})
	crashPoints[wf.JournalRecoveryAnswered] = scenario(wf.JournalRecoveryAnswered, simpleRun, func(h *drv) {
		unit := h.units("s1", 1, false)[0]
		h.append(wf.JournalRecoveryOffered, wf.RecoveryOfferedData{Unit: unit.Name, Cause: "the child ended with no done"})
		h.at(wf.JournalRecoveryAnswered, func() { h.must(h.e.Recover(h.id, unit.Name, wf.RecoveryRestart)) })
		h.finish(`{"OK":true}`)
	})
	crashPoints[wf.JournalDegraded] = scenario(wf.JournalDegraded, pagedRun, func(h *drv) {
		ins, err := h.e.HandOut(h.id, 1)
		h.must(err)
		esc := wf.DegradedData{Kind: wf.DegradedEscalated, Cause: "go test ./...: no network", Action: "ran"}
		for _, in := range ins {
			if in.Name == "" {
				continue
			}
			_, err := h.e.Prompt(h.id, in.Name)
			h.must(err)
			h.at(wf.JournalDegraded, func() {
				h.must(h.e.(*wf.Engine).Degrade(h.id, in.Name, []wf.DegradedData{esc}))
			})
			st, err := h.e.Status(h.id)
			h.must(err)
			if esc.Unit = in.Name; len(st.Degraded) != 1 || st.Degraded[0] != esc {
				h.t.Errorf("status Degraded = %+v, want [%+v]", st.Degraded, esc)
			}
		}
		h.pump("s2", "s1")
	})
	for _, k := range []wf.JournalKind{wf.JournalChildSpawned, wf.JournalChildSettled, wf.JournalHandedOut, wf.JournalAttemptStarted, wf.JournalPromptFetched} {
		crashPoints[k] = pagedPoint(k)
	}
	crashPoints[wf.JournalWithdrawn] = quorumPoint(wf.JournalWithdrawn, false)
	crashPoints[wf.JournalLateVote] = quorumPoint(wf.JournalLateVote, true)
}

// A crash between a requery-path withdrawn record and the child's
// child_settled leaves a withdrawn, unsettled unit. The resumed engine finishes
// the cancel instead of leaving the Map join waiting on it.
func TestCrashAtWithdrawnOnRequery(t *testing.T) {
	d, work, final, done := globDef()
	files := []string{"a.json", "b.json", "c.json"}
	w := globWorld(&files, func(read int) {
		if read == 3 && len(files) == 3 {
			files = []string{"a.json", "c.json"}
		}
	})
	tr := wftest.New(d, wftest.WithTB(t), wftest.WithWorld(w), wftest.Cap(1)).Run(
		wftest.Submit(work, pgOut{OK: true}),
		wftest.CrashAt(wf.JournalWithdrawn),
		wftest.Resume(),
		wftest.Submit(work, pgOut{OK: true}),
		wftest.Submit(final, pgOut{OK: true}),
	)
	wftest.AssertTerminal(t, tr, done)
	wd := withdrawnOf(t, tr)
	if len(wd) != 1 || !strings.Contains(wd[0].Name, "b.json") {
		t.Fatalf("withdrawn %+v, want exactly b.json's unit", wd)
	}
	settled := 0
	for _, r := range tr.Journal() {
		if r.Kind != wf.JournalChildSettled {
			continue
		}
		var s wf.ChildSettledData
		if err := json.Unmarshal(r.Data, &s); err != nil {
			t.Fatal(err)
		}
		if s.Cancelled {
			settled++
		}
	}
	if settled != 1 {
		t.Errorf("%d cancelled child_settled records, want 1 (b.json's child)", settled)
	}
}

// ArmBefore kills the writer just before the append: the record is lost, the
// crash is recorded, and the next append of the kind is written normally.
func TestCrashJournalArmBefore(t *testing.T) {
	cj := wftest.NewCrashJournal(wf.MemJournal())
	const run wf.RunID = "arm-before"
	unlock, err := cj.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	note := wf.JournalRecord{Kind: wf.JournalNote, Run: run}
	cj.ArmBefore(wf.JournalNote)
	if !wftest.Survive(func() { _ = cj.Append(note) }) {
		t.Fatal("the armed append did not crash")
	}
	if recs, err := cj.Records(run); !errors.Is(err, wf.ErrUnknownRun) {
		t.Fatalf("Records = %d records, %v after a crash before the write, want ErrUnknownRun", len(recs), err)
	}
	if fired := cj.Fired(); len(fired) != 1 || fired[0] != wf.JournalNote {
		t.Fatalf("Fired = %v, want [note]", fired)
	}
	if wftest.Survive(func() { must0(t, cj.Append(note)) }) {
		t.Fatal("a second append of the kind crashed: ArmBefore must fire once")
	}
	if recs, _ := cj.Records(run); len(recs) != 1 {
		t.Fatalf("journal holds %d records after the unarmed append, want 1", len(recs))
	}
	cj.ArmBefore(wf.JournalNote)
	cj.Arm(wf.JournalNote) // Arm resets the before mode: the record is written, then the crash
	if !wftest.Survive(func() { _ = cj.Append(note) }) {
		t.Fatal("the re-armed append did not crash")
	}
	if recs, _ := cj.Records(run); len(recs) != 2 {
		t.Fatalf("journal holds %d records after Arm, want 2 (crash after the write)", len(recs))
	}
}

func must0(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type (
	crDet     struct{}
	crDetOK   struct{}
	crDetFail struct{}
)

// crDetachedDef is one keyed, idempotent detached command: exit 0 passes, any
// other exit stops. The hour outlasts restart's ten-minute jump.
func crDetachedDef() *wf.Def {
	d := wf.New("cr-detached")
	exits := wf.NewEnum(d, "cr-exit", crExit(0), crExit(1))
	cmd := wf.NewCommand[crDet](d, "make")
	ok := wf.NewTerminal[crDetOK](d, wf.Success)
	fail := wf.NewTerminal[crDetFail](d, wf.Stopped)
	cmd.Key("build").Idempotent().Detached(time.Hour)
	cmd.RouteCases(wf.Cases[crDet, wf.RunResult](wf.ExitAs[crExit], exits,
		wf.On(crExit(0), cmd.To(ok, "passes")),
		wf.On(crExit(1), cmd.To(fail, "fails"))))
	return d
}

type crExit int

func kindCount(recs []wf.JournalRecord, k wf.JournalKind) int {
	n := 0
	for _, r := range recs {
		if r.Kind == k {
			n++
		}
	}
	return n
}

// A process that dies after the detached runner's Start and before the
// effect_started append leaves a proc record and no launch record. The resumed
// engine finds the launch through the proc record and, while its group lives,
// launches nothing: the command runs once and its result settles the run.
func TestCrashBeforeEffectStartedNoRelaunch(t *testing.T) {
	run := func(crash bool) (*drv, *wftest.FakeRunner) {
		fr := wftest.NewFakeRunner()
		dir := t.TempDir()
		h := newDrv(t, crash, crDetachedDef(), nil, func(o *wf.EngineOptions) {
			o.EffectRunner = fr
			o.RunDir = func(wf.RunID) string { return dir }
		})
		h.atBefore(wf.JournalEffectStarted, func() { h.next() })
		if crash && kindCount(h.records(), wf.JournalEffectStarted) != 0 {
			t.Fatal("the crash left an effect_started record: it did not come before the append")
		}
		for range 2 {
			h.now = h.now.Add(time.Minute) // past the start grace; the group still lives
			if ins := h.next(); len(ins) != 1 || ins[0].Kind != wf.InstructionWait {
				t.Fatalf("a live detached command issued %+v, want one wait", ins)
			}
			if n := len(fr.Started()); n != 1 {
				t.Fatalf("%d launches while the first launch's group lives, want 1", n)
			}
		}
		fr.SetStatus(0, wf.DetachedPresent, wf.RunResult{Exit: 0})
		h.next()
		return h, fr
	}
	ref, _ := run(false)
	h, fr := run(true)
	if got, want := h.outcome(), ref.outcome(); got != want {
		t.Errorf("after a crash before effect_started the run ended %q, want %q (uncrashed)", got, want)
	}
	if got := h.outcome(); got != "ended/crDetOK retries=0" {
		t.Errorf("outcome %q, want the Success terminal with no retry", got)
	}
	if n := len(fr.Started()); n != 1 {
		t.Errorf("%d launches, want 1", n)
	}
	// The proc record stood in for the lost record; the resumed engine journals
	// the launch it found, once.
	if n := kindCount(h.records(), wf.JournalEffectStarted); n != 1 {
		t.Errorf("%d effect_started records, want 1 (the recovered launch)", n)
	}
	if fired := h.cj.Fired(); len(fired) != 1 || fired[0] != wf.JournalEffectStarted {
		t.Errorf("crash fired at %v, want effect_started", fired)
	}
}

// The same requery-path crash as above, against an uncrashed run of the same
// script: both end at the same terminal and the crash spends no retry.
func TestCrashAtWithdrawnOnRequeryMatchesUncrashed(t *testing.T) {
	run := func(crash bool) *wftest.Trace {
		d, work, final, done := globDef()
		files := []string{"a.json", "b.json", "c.json"}
		w := globWorld(&files, func(read int) {
			if read == 3 && len(files) == 3 {
				files = []string{"a.json", "c.json"}
			}
		})
		steps := []wftest.Step{wftest.Submit(work, pgOut{OK: true})}
		if crash {
			steps = append(steps, wftest.CrashAt(wf.JournalWithdrawn), wftest.Resume())
		}
		steps = append(steps, wftest.Submit(work, pgOut{OK: true}), wftest.Submit(final, pgOut{OK: true}))
		tr := wftest.New(d, wftest.WithTB(t), wftest.WithWorld(w), wftest.Cap(1)).Run(steps...)
		wftest.AssertTerminal(t, tr, done)
		if len(files) != 2 {
			t.Fatal("the directory never shrank: the scenario did not exercise the requery")
		}
		return tr
	}
	want, got := run(false), run(true)
	if c := got.Crashes(); len(c) != 1 || c[0] != wf.JournalWithdrawn {
		t.Fatalf("crash fired at %v, want [withdrawn]", c)
	}
	if got.Terminal() != want.Terminal() {
		t.Errorf("terminal %q after the crash, want %q (uncrashed)", got.Terminal(), want.Terminal())
	}
	if got.RetriesSpent() != 0 || want.RetriesSpent() != 0 {
		t.Errorf("retries spent: crashed %d, uncrashed %d, want 0 and 0", got.RetriesSpent(), want.RetriesSpent())
	}
}

type (
	crSelect  struct{}
	crWrite   struct{}
	crWritten struct{}
	crPayload struct {
		Files string `json:"findingFiles"`
	}
)

const crFiles = "f1\t{\"id\":1}\nf2\t{\"id\":2}\n"

// crWriterDef mirrors the roast round's judgeItems and writer: a compute
// selects the findings and a keyed command writes them from its stdin.
func crWriterDef() *wf.Def {
	d := wf.New("cr-writer")
	sel := wf.NewCompute[crSelect, crPayload](d, func(wf.Ctx) (crPayload, error) { return crPayload{Files: crFiles}, nil })
	w := shell.Command[crWrite](d, shell.Run("write-findings", "dir").WithStdin("{findingFiles}")).Key("write").Idempotent()
	end := wf.NewTerminal[crWritten](d, wf.Success)
	toW := sel.To(w, "write")
	sel.Route(func(crPayload, wf.Ctx) wf.Edge[crSelect] { return toW }, toW)
	toEnd := w.To(end, "written")
	w.Route(func(wf.RunResult, wf.Ctx) wf.Edge[crWrite] { return toEnd }, toEnd)
	return d
}

// A death after the selecting compute's accepted record and before the
// writer's effect_intent loses no work: the resumed engine does not recompute,
// and the writer runs once with the compute's payload on its stdin.
func TestCrashBetweenComputeAndStdinWriter(t *testing.T) {
	run := func(crash bool) *drv {
		h := newDrv(t, crash, crWriterDef(), nil)
		h.atBefore(wf.JournalEffectIntent, func() { h.next() })
		if crash && kindCount(h.records(), wf.JournalEffectIntent) != 0 {
			t.Fatal("the crash left an effect_intent: it did not come before the append")
		}
		for range 5 {
			h.next()
		}
		return h
	}
	ref, h := run(false), run(true)
	if got, want := h.outcome(), ref.outcome(); got != want {
		t.Errorf("after a crash before the writer's intent the run ended %q, want %q (uncrashed)", got, want)
	}
	if got := h.outcome(); got != "ended/crWritten retries=0" {
		t.Errorf("outcome %q, want the Success terminal with no retry", got)
	}
	if n := kindCount(h.records(), wf.JournalAccepted); n != kindCount(ref.records(), wf.JournalAccepted) {
		t.Errorf("%d accepted records, uncrashed run has %d: the compute ran again", n, kindCount(ref.records(), wf.JournalAccepted))
	}
	applied := wftest.Applied(h.world)
	if len(applied) != 1 || applied[0].Adapter != "shell" || applied[0].Op != "run" {
		t.Fatalf("applied %+v, want exactly one shell.run", applied)
	}
	if got := applied[0].Args["stdin"]; got != crFiles {
		t.Errorf("writer stdin %q, want the compute's payload %q", got, crFiles)
	}
}
