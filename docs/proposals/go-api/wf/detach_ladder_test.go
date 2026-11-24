package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// ladderRunner is detRunner on a host whose probe says systemd. It records the
// mode of every Start, failed ones included, and fails those listed in fail.
type ladderRunner struct {
	*detRunner
	modes  []string
	fail   map[string]error
	late   bool // a failed Start still leaves a launch, as a timed-out systemd-run's late unit does
	probes int
	mode   string         // the probed mode; "" is systemd
	fact   *wf.DetachFact // the whole probed fact, over mode
	// events is every Start ("start <mode>") and fence ("fence <intent>") in
	// order; lateAtFence makes the failed launch's unit write its record just
	// before the fence lands.
	events      []string
	failed      []wf.DetachedRequest
	lateAtFence bool
}

// Terminate with a zero process is the fence; anything else is detRunner's.
func (l *ladderRunner) Terminate(p wf.DetachedProcess, req wf.DetachedRequest) error {
	if p.Pgid != 0 {
		return l.detRunner.Terminate(p, req)
	}
	l.events = append(l.events, fmt.Sprint("fence ", intentOf(req.ResultPath)))
	if l.lateAtFence {
		for _, f := range l.failed {
			if f.ResultPath == req.ResultPath {
				_, _ = l.detRunner.Start(f)
			}
		}
	}
	return nil
}

func (l *ladderRunner) DetachMode() wf.DetachFact {
	l.probes++
	if l.fact != nil {
		return *l.fact
	}
	if l.mode != "" {
		return wf.DetachFact{Mode: l.mode}
	}
	return wf.DetachFact{Mode: wf.DetachSystemd}
}

func (l *ladderRunner) Start(req wf.DetachedRequest) (wf.DetachedProcess, error) {
	l.modes = append(l.modes, req.Mode)
	l.events = append(l.events, "start "+req.Mode)
	if err := l.fail[req.Mode]; err != nil {
		l.failed = append(l.failed, req)
		if l.late {
			_, _ = l.detRunner.Start(req)
		}
		return wf.DetachedProcess{}, err
	}
	return l.detRunner.Start(req)
}

func degradedOf(t *testing.T, h *harness, run wf.RunID) []wf.DegradedData {
	t.Helper()
	var out []wf.DegradedData
	for _, r := range h.kind(run, wf.JournalDegraded) {
		var d wf.DegradedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// Item 98: a detached launch that dies steps the run down one detach mode and
// re-runs once there, systemd, then process, then inline. No mode is launched
// twice, nothing settles runner-failed, each step-down is a degraded record,
// and the lowered mode holds for the run on every later replay.
func TestDetachLadderStepsDownOnDeath(t *testing.T) {
	lr, w := &ladderRunner{detRunner: newDetRunner()}, newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, lr))
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	for i := 1; i <= 2; i++ {
		intent := lastIntent(t, h, run)
		lr.setReason(intent, "log line\nboom "+strconv.Itoa(i))
		lr.set(intent, wf.DetachedDead, wf.RunResult{})
		if i == 2 {
			// The process launch's group outlives its leader: the inline re-run
			// terminates it and waits; once it is empty the re-run runs.
			lr.set(intent, wf.DetachedDeadLive, wf.RunResult{})
			lr.setStubborn(intent, true)
			h.next(run)
			if got := lr.terminations(); w.appliedCount() != 0 || len(got) == 0 || got[0] != intent {
				t.Fatalf("inline applies %d, terminated %v, want 0 and %d terminated while the dead group lives", w.appliedCount(), got, intent)
			}
			lr.setStubborn(intent, false)
			lr.set(intent, wf.DetachedDead, wf.RunResult{})
		}
		h.next(run)
	}
	if want := []string{wf.DetachSystemd, wf.DetachProcess}; !slices.Equal(lr.modes, want) {
		t.Fatalf("Start modes = %v, want %v: one launch per mode, inline last", lr.modes, want)
	}
	seqs, data := intentsOf(t, h, run)
	if len(data) != 3 || data[0].Mode != wf.DetachSystemd || data[1].Mode != wf.DetachProcess || data[1].Rerun == nil || data[1].Rerun.Of != seqs[0] ||
		data[2].Mode != wf.DetachNone || data[2].Rerun == nil || data[2].Rerun.Of != seqs[1] || data[2].Max != 0 {
		t.Fatalf("effect_intent records = %v %+v, want systemd, process, then an inline re-run", seqs, data)
	}
	if w.appliedCount() != 1 || len(h.kind(run, wf.JournalEffectStarted)) != 2 {
		t.Fatalf("inline applies %d, effect_started %d, want 1 and 2", w.appliedCount(), len(h.kind(run, wf.JournalEffectStarted)))
	}
	if res := resultsOf(t, h, run); len(res) != 1 || res[0].Outcome == "runner failed" || res[0].Intent != seqs[2] {
		t.Fatalf("effect_result = %+v, want the inline re-run's own result", res)
	}
	want := []wf.DegradedData{
		{Unit: "dRun", Kind: wf.DegradedDetach, Cause: "boom 1", Action: "stepped down systemd -> process"},
		{Unit: "dRun", Kind: wf.DegradedDetach, Cause: "boom 2", Action: "stepped down process -> none"},
	}
	if got := degradedOf(t, h, run); !slices.Equal(got, want) {
		t.Fatalf("degraded = %+v, want %+v", got, want)
	}
	st := h.status(run)
	statusOf(t, st, wf.RunEnded, "dOK")
	if st.Detach != wf.DetachNone {
		t.Fatalf("status detach = %q, want none: the step-down is sticky", st.Detach)
	}
}

// A Start that launched nothing steps down one mode within the same call:
// systemd-run refusing goes to process, a double fork that left nothing
// running goes inline. Neither settles the step unavailable.
func TestDetachLadderStepsDownWhenNothingLaunched(t *testing.T) {
	for _, c := range []struct {
		host, down string
		modes      []string
		started    int
	}{
		{wf.DetachSystemd, wf.DetachProcess, []string{wf.DetachSystemd, wf.DetachProcess}, 1},
		{wf.DetachProcess, wf.DetachNone, []string{wf.DetachProcess}, 0},
	} {
		t.Run(c.host, func(t *testing.T) {
			err := fmt.Errorf("%w: launcher failed", wf.ErrNotLaunched)
			lr, w := &ladderRunner{detRunner: newDetRunner(), mode: c.host, fail: map[string]error{c.host: err}}, newWorld()
			d := detDef(idempotentDetached(time.Minute))
			h := newHarnessOpts(t, d, w, detOpts(t, lr))
			run := h.run(d, map[string]string{"topic": "build"})
			h.next(run)
			if !slices.Equal(lr.modes, c.modes) {
				t.Fatalf("Start modes = %v, want %v", lr.modes, c.modes)
			}
			seqs, data := intentsOf(t, h, run)
			if len(data) != 2 || data[1].Mode != c.down || data[1].Rerun == nil || data[1].Rerun.Of != seqs[0] {
				t.Fatalf("effect_intent records = %v %+v, want a %s re-run of %d", seqs, data, c.down, seqs[0])
			}
			got := degradedOf(t, h, run)
			if c.host == wf.DetachProcess && len(got) > 0 && got[0].Action == "detach process at preflight" {
				got = got[1:] // a process host starts a rung down
			}
			if len(got) != 1 || got[0].Cause != err.Error() || got[0].Action != "stepped down "+c.host+" -> "+c.down {
				t.Fatalf("degraded = %+v, want one %s -> %s step-down caused by %q", got, c.host, c.down, err)
			}
			if n := len(h.kind(run, wf.JournalEffectStarted)); n != c.started || w.appliedCount() != 1-c.started {
				t.Fatalf("effect_started %d, inline applies %d, want %d and %d", n, w.appliedCount(), c.started, 1-c.started)
			}
			for _, r := range resultsOf(t, h, run) {
				if r.Outcome == "unavailable" {
					t.Fatalf("effect_result %+v: a Start that launched nothing settled unavailable", r)
				}
			}
		})
	}
}

// The inline re-run's wait for the dead group is bounded by the command's
// maximum, as a detached re-run's is: a group that never ends cannot stall
// the step.
func TestDetachLadderInlineWaitIsBounded(t *testing.T) {
	lr, w := &ladderRunner{detRunner: newDetRunner(), fail: map[string]error{wf.DetachSystemd: wf.ErrNotLaunched}}, newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, lr))
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	intent := lastIntent(t, h, run)
	lr.set(intent, wf.DetachedDeadLive, wf.RunResult{})
	lr.setStubborn(intent, true)
	h.next(run)
	h.now = h.now.Add(30 * time.Second)
	h.next(run)
	if w.appliedCount() != 0 {
		t.Fatalf("inline applies %d inside the maximum, want 0", w.appliedCount())
	}
	h.now = h.now.Add(31 * time.Second)
	h.next(run)
	if w.appliedCount() != 1 {
		t.Fatalf("inline applies %d past the maximum, want 1", w.appliedCount())
	}
	statusOf(t, h.status(run), wf.RunEnded, "dOK")
}

// A systemd-run that timed out journals nothing and consults no Check: past
// the start grace, the intent's own proc record decides. With none, nothing
// ran and the step goes down to process, whatever the command's recovery
// (idempotent, keyed with an unknown Check, keyed with a failing Check), so a
// hold cannot leave it re-launching on systemd.
func TestDetachLadderTimeoutStepsDownAfterTheGrace(t *testing.T) {
	timeout := fmt.Errorf("%w: systemd-run did not return within 5s", wf.ErrUnavailable)
	for _, c := range []struct {
		name string
		mod  func(*wf.Command[dRun])
		w    func(*world)
	}{
		{"idempotent", idempotentDetached(time.Minute), func(*world) {}},
		{"keyed, Check unknown", func(c *wf.Command[dRun]) { c.Detached(time.Minute) }, func(w *world) { w.status["build/build"] = wf.EffectUnknown }},
		{"keyed, Check error", func(c *wf.Command[dRun]) { c.Detached(time.Minute) }, func(w *world) { w.checkErr = []error{errors.New("tool down")} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			lr, w := &ladderRunner{detRunner: newDetRunner(), fail: map[string]error{wf.DetachSystemd: timeout}}, newWorld()
			c.w(w)
			d := detDef(c.mod)
			h := newHarnessOpts(t, d, w, detOpts(t, lr))
			run := h.run(d, map[string]string{"topic": "build"})
			h.next(run)
			h.now = h.now.Add(5 * time.Second)
			h.next(run)
			if seqs, _ := intentsOf(t, h, run); len(seqs) != 1 || len(degradedOf(t, h, run)) != 0 || !slices.Equal(lr.modes, []string{wf.DetachSystemd}) {
				t.Fatalf("intents %v, degraded %+v, Start modes %v inside the grace, want the one intent and nothing else", seqs, degradedOf(t, h, run), lr.modes)
			}
			h.now = h.now.Add(6 * time.Second)
			h.next(run)
			if want := []string{wf.DetachSystemd, wf.DetachProcess}; !slices.Equal(lr.modes, want) || w.checks != 0 {
				t.Fatalf("Start modes %v, checks %d, want %v and none", lr.modes, w.checks, want)
			}
			if got := degradedOf(t, h, run); len(got) != 1 || got[0].Action != "stepped down systemd -> process" {
				t.Fatalf("degraded = %+v, want one systemd -> process step-down", got)
			}
			if len(h.kind(run, wf.JournalEffectStarted)) != 1 || len(resultsOf(t, h, run)) != 0 {
				t.Fatalf("effect_started %d, results %+v, want the process launch running", len(h.kind(run, wf.JournalEffectStarted)), resultsOf(t, h, run))
			}
		})
	}
}

// A timed-out systemd-run whose unit started late is that intent's launch: a
// running one is adopted, a finished one's result counts, and nothing steps
// down.
func TestDetachLadderTimeoutAdoptsALateUnit(t *testing.T) {
	timeout := fmt.Errorf("%w: systemd-run did not return within 5s", wf.ErrUnavailable)
	for _, finished := range []bool{false, true} {
		t.Run(fmt.Sprint("finished=", finished), func(t *testing.T) {
			lr, w := &ladderRunner{detRunner: newDetRunner(), late: true, fail: map[string]error{wf.DetachSystemd: timeout}}, newWorld()
			d := detDef(idempotentDetached(time.Minute))
			h := newHarnessOpts(t, d, w, detOpts(t, lr))
			run := h.run(d, map[string]string{"topic": "build"})
			h.next(run)
			intent := lastIntent(t, h, run)
			if finished {
				lr.set(intent, wf.DetachedPresent, wf.RunResult{Exit: 0})
			}
			h.now = h.now.Add(11 * time.Second)
			h.next(run)
			if !slices.Equal(lr.modes, []string{wf.DetachSystemd}) || len(degradedOf(t, h, run)) != 0 || w.appliedCount() != 0 {
				t.Fatalf("Start modes %v, degraded %+v, inline applies %d, want the late unit alone", lr.modes, degradedOf(t, h, run), w.appliedCount())
			}
			if finished {
				res := resultsOf(t, h, run)
				if len(res) != 1 || res[0].Intent != intent || res[0].Outcome != "applied" {
					t.Fatalf("effect_result = %+v, want the late unit's result for %d", res, intent)
				}
				statusOf(t, h.status(run), wf.RunEnded, "dOK")
				return
			}
			if st := startedOf(t, h, run); len(st) != 1 || st[0].Intent != intent {
				t.Fatalf("effect_started = %+v, want the late unit adopted for %d", st, intent)
			}
		})
	}
}

// ladderChild is a Map child whose only state is a keyed detached command,
// idempotent when asked.
func ladderChild(idempotent bool) wf.Child[chItem, chRes] {
	d := wf.New("c-cmd")
	exits := wf.NewEnum(d, "d-exit", dExit(0), dExit(1))
	cmd := wf.NewCommand[dRun](d, "make")
	ok := wf.NewTerminal[dOK](d, wf.Success)
	fail := wf.NewTerminal[dFail](d, wf.Stopped)
	cmd.Key("build", "x").Detached(time.Minute)
	if idempotent {
		cmd.Idempotent()
	}
	cmd.RouteCases(wf.Cases[dRun, wf.RunResult](wf.ExitAs[dExit], exits,
		wf.On(dExit(0), cmd.To(ok, "passes")),
		wf.On(dExit(1), cmd.To(fail, "fails"))))
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(fail, res)
	return wf.Typed[chItem, chRes](d)
}

// fanAtNone runs two children of ladderChild(idempotent): the first one's
// effect_started is lost (its proc record hidden), the other steps the run down
// to none. It returns the harness, run, runner, world and the lost intent.
func fanAtNone(t *testing.T, idempotent bool, w *world) (*harness, wf.RunID, *ladderRunner, int64) {
	t.Helper()
	d := wf.New("ladder-fan")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, ladderChild(idempotent))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	lr := &ladderRunner{detRunner: newDetRunner()}
	h := newHarnessOpts(t, d, w, detOpts(t, lr))
	opts := detOpts(t, lr)
	opts.Clock, opts.Defs = func() time.Time { return h.now }, []*wf.Def{d}
	h.e = wf.NewEngine(&dropStarted{Journal: h.j, drop: 1}, h.reg, opts)
	run := h.run(d, nil)
	if _, err := h.e.Next(run, ""); err == nil {
		t.Fatal("Next succeeded, want the lost effect_started append to fail it")
	}
	lost := intentOf(lr.request(0).ResultPath)
	lr.setHideProc(lost, true)
	h.next(run)
	if lr.startCount() != 2 {
		t.Fatalf("launches = %d, want the other child launched beside the lost one", lr.startCount())
	}
	lr.set(intentOf(lr.request(1).ResultPath), wf.DetachedDead, wf.RunResult{})
	h.next(run)
	lr.set(intentOf(lr.request(2).ResultPath), wf.DetachedDead, wf.RunResult{})
	h.next(run)
	if st := h.status(run); st.Detach != wf.DetachNone || w.appliedCount() != 1 {
		t.Fatalf("detach %q, inline applies %d, want none and the stepped-down child's one", st.Detach, w.appliedCount())
	}
	return h, run, lr, lost
}

// After one command steps the run down to none, a concurrent command's open
// detached intent whose effect_started was lost is resumed through its proc
// record, never re-run inline beside its live runner.
func TestDetachLadderStepDownResumesAConcurrentLaunch(t *testing.T) {
	w := newWorld()
	h, run, lr, lost := fanAtNone(t, true, w)
	lr.setHideProc(lost, false)
	h.next(run)
	var resumed bool
	for _, s := range startedOf(t, h, run) {
		resumed = resumed || s.Intent == lost
	}
	if w.appliedCount() != 1 || lr.startCount() != 3 || !resumed {
		t.Fatalf("inline applies %d, launches %d, resumed %v, want 1, 3 and the lost launch's effect_started", w.appliedCount(), lr.startCount(), resumed)
	}
}

// Past the start grace with no proc record, an open detached intent in a run
// that stepped down to none is recovered, not launched inline unchecked: an
// inline run leaves no proc record, so a call killed mid-run would otherwise
// run a keyed command again with no Check.
func TestDetachLadderOpenIntentAtNoneIsRecovered(t *testing.T) {
	w := newWorld()
	w.status["build/x"] = wf.EffectNotDone
	h, run, _, _ := fanAtNone(t, false, w)
	checks := w.checks
	h.now = h.now.Add(11 * time.Second)
	h.next(run)
	if w.checks != checks+1 || w.appliedCount() != 2 {
		t.Fatalf("checks %d (was %d), inline applies %d, want one more Check before the lost intent runs inline", w.checks, checks, w.appliedCount())
	}
}

// The probe runs a unit, so a run that journaled its mode never probes again:
// next and status read run_started.
func TestJournaledDetachModeIsNotProbedAgain(t *testing.T) {
	lr, w := &ladderRunner{detRunner: newDetRunner()}, newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, lr))
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	h.next(run)
	if st := h.status(run); st.Detach != wf.DetachSystemd || lr.probes != 1 {
		t.Fatalf("detach %q after %d probes, want systemd from the one probe at run start", st.Detach, lr.probes)
	}
}

// Before a timed-out systemd intent steps down it fences its paths and looks
// once more: a late unit that wrote its record first is adopted as that
// intent's launch; one that comes after the fence exits unrun, so the process
// re-run never runs beside it.
func TestDetachLadderStepDownFencesALateUnit(t *testing.T) {
	timeout := fmt.Errorf("%w: systemd-run did not return within 5s", wf.ErrUnavailable)
	for _, first := range []bool{false, true} {
		t.Run(fmt.Sprint("record before the fence=", first), func(t *testing.T) {
			lr, w := &ladderRunner{detRunner: newDetRunner(), lateAtFence: first, fail: map[string]error{wf.DetachSystemd: timeout}}, newWorld()
			d := detDef(idempotentDetached(time.Minute))
			h := newHarnessOpts(t, d, w, detOpts(t, lr))
			run := h.run(d, map[string]string{"topic": "build"})
			h.next(run)
			intent := lastIntent(t, h, run)
			h.now = h.now.Add(11 * time.Second)
			h.next(run)
			fence := fmt.Sprint("fence ", intent)
			if first {
				if want := []string{"start systemd", fence}; !slices.Equal(lr.events, want) || len(degradedOf(t, h, run)) != 0 {
					t.Fatalf("events %v, degraded %+v, want %v and the late unit adopted", lr.events, degradedOf(t, h, run), want)
				}
				if st := startedOf(t, h, run); len(st) != 1 || st[0].Intent != intent {
					t.Fatalf("effect_started = %+v, want the late unit adopted for %d", st, intent)
				}
				return
			}
			if want := []string{"start systemd", fence, "start process"}; !slices.Equal(lr.events, want) {
				t.Fatalf("events %v, want %v: the systemd paths are fenced before the process launch", lr.events, want)
			}
		})
	}
}

// A re-run of a launch that reported nothing launched still looks for the
// failed launch's proc record before it starts: a record that appeared
// anyway is a group to end first.
func TestDetachLadderStepDownEndsALateRecordFirst(t *testing.T) {
	lr, w := &ladderRunner{detRunner: newDetRunner(), late: true, fail: map[string]error{wf.DetachSystemd: wf.ErrNotLaunched}}, newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, lr))
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	seqs, _ := intentsOf(t, h, run)
	if got := lr.terminations(); len(seqs) != 2 || len(got) != 1 || got[0] != seqs[0] || !slices.Equal(lr.modes, []string{wf.DetachSystemd}) {
		t.Fatalf("intents %v, terminated %v, Start modes %v, want the late record's group of %d ended and no process launch yet", seqs, got, lr.modes, seqs[0])
	}
	h.now = h.now.Add(11 * time.Second)
	h.next(run)
	if want := []string{wf.DetachSystemd, wf.DetachProcess}; !slices.Equal(lr.modes, want) {
		t.Fatalf("Start modes %v, want %v once the group is empty", lr.modes, want)
	}
}

// A probe that picks a mode below systemd is one degraded record of kind
// detach at run start, under the root definition's name: never journaled again
// by a later call, and the run's verdict is the command's own.
func TestDetachPreflightBelowSystemdIsDegraded(t *testing.T) {
	for _, c := range []struct {
		fact  wf.DetachFact
		cause string
	}{
		{wf.DetachFact{Mode: wf.DetachNone, NSpidFields: 1, Pid1: "codex", Systemd: "not probed: per-command sandbox"}, "per-command sandbox (pid1 codex)"},
		{wf.DetachFact{Mode: wf.DetachProcess, Pid1: "systemd", Systemd: "no user bus: exit 1: Failed to connect to bus"}, "no user bus: exit 1: Failed to connect to bus"},
		{wf.DetachFact{Mode: wf.DetachNone, Systemd: wf.DetachSetBy + "WF_DETACH_MODE"}, ""}, // the user's choice
	} {
		t.Run(c.fact.Mode+" "+c.fact.Systemd, func(t *testing.T) {
			lr, w := &ladderRunner{detRunner: newDetRunner(), fact: &c.fact}, newWorld()
			d := detDef(idempotentDetached(time.Minute))
			h := newHarnessOpts(t, d, w, detOpts(t, lr))
			run := h.run(d, map[string]string{"topic": "build"})
			h.next(run)
			if c.fact.Mode == wf.DetachProcess {
				lr.set(lastIntent(t, h, run), wf.DetachedPresent, wf.RunResult{})
			}
			h.next(run)
			h.next(run)
			var want []wf.DegradedData
			if c.cause != "" {
				want = []wf.DegradedData{{Unit: "detached", Kind: wf.DegradedDetach, Cause: c.cause, Action: "detach " + c.fact.Mode + " at preflight"}}
			}
			st := h.status(run)
			if !slices.Equal(st.Degraded, want) {
				t.Fatalf("status degraded = %+v, want %+v once", st.Degraded, want)
			}
			statusOf(t, st, wf.RunEnded, "dOK")
		})
	}
}

// Once the run steps down to none mid-run, the instructions next returns
// carry the bootstrap's detach-none line, from the step-down call on: long
// commands now run inline inside next. Before it no instruction carries it.
func TestDetachStepDownToNoneAdvisesALongTimeout(t *testing.T) {
	lr, w := &ladderRunner{detRunner: newDetRunner()}, newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, lr))
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	lr.set(lastIntent(t, h, run), wf.DetachedDead, wf.RunResult{})
	for _, ins := range h.next(run) {
		if ins.Advice != "" {
			t.Fatalf("instruction %+v at process carries advice %q, want none", ins, ins.Advice)
		}
	}
	// The process launch dies with a live group: the inline re-run waits for it.
	intent := lastIntent(t, h, run)
	lr.set(intent, wf.DetachedDeadLive, wf.RunResult{})
	lr.setStubborn(intent, true)
	ins := h.next(run)
	boot := wf.Bootstrap(wf.RunStatus{Run: run, Detach: wf.DetachNone}, "./super", "/state")
	if len(ins) != 1 || ins[0].Advice != wf.DetachNoneAdvice || !strings.Contains(boot, wf.DetachNoneAdvice+"\n") {
		t.Fatalf("instructions %+v on the step-down to none, want one carrying the bootstrap's detach-none line", ins)
	}
	// next <n>, the coordinator's form, carries it too.
	handed, err := h.e.HandOut(run, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(handed) != 1 || handed[0].Kind != wf.InstructionWait || handed[0].Advice != wf.DetachNoneAdvice {
		t.Fatalf("hand-out %+v, want the wait instruction carrying the detach-none line", handed)
	}
	// Once the group ends the inline re-run settles the run: ended carries none.
	lr.setStubborn(intent, false)
	lr.set(intent, wf.DetachedDead, wf.RunResult{})
	ended, err := h.e.HandOut(run, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ended) != 1 || ended[0].Kind != wf.InstructionEnded || ended[0].Advice != "" {
		t.Fatalf("instructions %+v at the end, want one ended instruction with no advice", ended)
	}
}
