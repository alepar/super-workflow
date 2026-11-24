package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// C3 and B2 (loud-failures plan): an agent state's spent retry budget, stall
// or lost cap is offered to the coordinator once per state visit (restart or
// give up), then takes its declared exhausted (or stalled) edge. The engine
// never halts for a human on exhaustion and reads no Autonomous option.

type (
	exPrep   struct{}
	exWork   struct{}
	exDone   struct{}
	exGaveUp struct{}
	exStuck  struct{}
	exOut    struct{ OK, Off bool }
	exOpts   struct{ Autonomous bool }
)

// exDef is Prep (an agent) then Work (an agent with Retry Const(2) to GaveUp,
// "gave up"). stall adds NoProgressAfter(2) with OnStalled to Stuck ("stuck")
// and a budget of 5. Work's route returns an edge outside its candidates when
// the output says Off, which halts it Blocked.
func exDef(name string, stall bool) *wf.Def {
	d := wf.New(name)
	wf.Options[exOpts](d)
	prep := wf.NewAgent[exPrep, exOut](d)
	work := wf.NewAgent[exWork, exOut](d)
	done := wf.NewTerminal[exDone](d, wf.Success)
	gaveUp := wf.NewTerminal[exGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	prep.Spec(wf.PromptText("prep"), wf.Retry[exPrep](wf.Const(1), prep.To(gaveUp, "prep gave up")))
	toWork := prep.To(work, "prepared")
	prep.Route(func(exOut, wf.Ctx) wf.Edge[exPrep] { return toWork }, toWork)
	p := wf.Retry[exWork](wf.Const(2), work.To(gaveUp, "gave up"))
	if stall {
		stuck := wf.NewTerminal[exStuck](d, wf.Stalled)
		p = wf.Retry[exWork](wf.Const(5), work.To(gaveUp, "gave up")).NoProgressAfter(2).OnStalled(work.To(stuck, "stuck"))
	}
	work.Spec(wf.PromptText("work"), p)
	ok, off := work.To(done, "ok"), work.To(done, "off")
	work.Route(func(o exOut, _ wf.Ctx) wf.Edge[exWork] {
		if o.Off {
			return off
		}
		return ok
	}, ok)
	return d
}

// exRun starts d (autonomous or not) and passes Prep.
func exRun(t *testing.T, h *harness, d *wf.Def, autonomous bool) wf.RunID {
	t.Helper()
	run, err := h.e.Run(d, nil, exOpts{Autonomous: autonomous})
	if err != nil {
		t.Fatal(err)
	}
	if res := h.submit(run, h.next(run)[0], `{"OK":true}`); res.Class != wf.Accepted {
		t.Fatalf("Prep = %+v, want accepted", res)
	}
	return run
}

// failWorkOnce abandons the open Work attempt with reason and returns it.
func failWorkOnce(t *testing.T, h *harness, run wf.RunID, reason string) wf.Instruction {
	t.Helper()
	ins := h.next(run)
	if len(ins) != 1 || ins[0].State != "exWork" {
		t.Fatalf("next = %+v, want one Work instruction", ins)
	}
	if _, err := h.e.Fail(run, ins[0].Token, reason); err != nil {
		t.Fatal(err)
	}
	return ins[0]
}

func lastRefused(t *testing.T, h *harness, run wf.RunID) wf.RefusedData {
	t.Helper()
	rd := refusedRecords(t, h, run)
	if len(rd) == 0 {
		t.Fatal("no refused record")
	}
	return rd[len(rd)-1]
}

func retriedRecords(t *testing.T, h *harness, run wf.RunID) []wf.RetriedData {
	t.Helper()
	var out []wf.RetriedData
	for _, r := range h.kind(run, wf.JournalRetried) {
		var d wf.RetriedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func issuedAt(h *harness, run wf.RunID, state string) int {
	n := 0
	for _, r := range h.kind(run, wf.JournalInstructionIssued) {
		if r.State == state {
			n++
		}
	}
	return n
}

// recoveryOffers is the run's open recovery offers, from status.
func recoveryOffers(t *testing.T, h *harness, run wf.RunID) []wf.RecoveryView {
	t.Helper()
	st, err := h.e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	return st.Recoveries
}

// B2: a spent budget is offered to the coordinator before the declared edge:
// the halt carries Recovery, is not a human's (no Fix lines, not blocked),
// and next hands out a recovery instruction with restart and give-up. Give
// up takes the exhausted edge with its label as the cause.
func TestExhaustionOffersRecoveryThenGivesUp(t *testing.T) {
	for _, autonomous := range []bool{false, true} {
		d := exDef("ex-route", false)
		h := newHarness(t, d, nil)
		h.manualRecovery = true
		run := exRun(t, h, d, autonomous)
		failWorkOnce(t, h, run, "a")
		failWorkOnce(t, h, run, "b")
		rd := lastRefused(t, h, run)
		if !rd.Halt || !rd.Recovery || rd.Plan != nil || rd.Exhausted != wf.ExhaustedBudget || !slices.Equal(rd.Reasons, []string{"abandoned: b"}) {
			t.Fatalf("autonomous=%v refused = %+v, want the recovery offer", autonomous, rd)
		}
		st := h.status(run)
		if st.Phase == wf.RunBlocked || len(st.Fix) != 0 {
			t.Fatalf("status = %s fix %q, want the offer, not a halt for a human", st.Phase, st.Fix)
		}
		ins := h.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionRecovery || ins[0].Recovery == nil || ins[0].Recovery.Restarts != wf.RecoveryRestarts ||
			ins[0].Recovery.Commands[wf.RecoveryRestart] == "" || ins[0].Recovery.Commands[wf.RecoveryGiveUp] == "" || ins[0].Recovery.Commands[wf.RecoveryResume] != "" {
			t.Fatalf("next = %+v, want one recovery instruction with restart and give-up", ins)
		}
		if err := h.e.Recover(run, ins[0].Name, wf.RecoveryGiveUp); err != nil {
			t.Fatal(err)
		}
		statusOf(t, h.status(run), wf.RunEnded, "exGaveUp")
		ds := degradedOf(t, h, run)
		if len(ds) != 1 || ds[0].Kind != wf.DegradedRecovery || !strings.HasPrefix(ds[0].Action, "give up") {
			t.Fatalf("degraded = %+v, want the give-up recorded", ds)
		}
		if n := issuedAt(h, run, "exWork"); n != 2 {
			t.Fatalf("Work issued %d times, want 2", n)
		}
	}
}

// Restart grants one more attempt; that attempt's exhaustion on the same
// visit takes the edge with no second offer.
func TestRecoveryRestartIsOneAttemptPerVisit(t *testing.T) {
	d := exDef("ex-restart", false)
	h := newHarness(t, d, nil)
	h.manualRecovery = true
	run := exRun(t, h, d, true)
	failWorkOnce(t, h, run, "a")
	failWorkOnce(t, h, run, "b")
	if err := h.e.Recover(run, recoveryOffers(t, h, run)[0].Unit, wf.RecoveryRestart); err != nil {
		t.Fatal(err)
	}
	failWorkOnce(t, h, run, "c")
	if offers := recoveryOffers(t, h, run); len(offers) != 0 {
		t.Fatalf("offers = %+v, want none: the visit's offer is spent", offers)
	}
	statusOf(t, h.status(run), wf.RunEnded, "exGaveUp")
	if n := issuedAt(h, run, "exWork"); n != 3 {
		t.Fatalf("Work issued %d times, want 3 (the budget and the restart)", n)
	}
	if err := h.e.Recover(run, "exWork#3", wf.RecoveryRestart); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Recover on an ended run = %v, want ErrRejected", err)
	}
}

// routeBackDef loops Prep -> Work, Work's exhausted edge going back to Prep, at most
// four Work visits: a route-back cycle a coordinator could answer restart on
// forever, but for RecoveryRestarts.
func routeBackDef() *wf.Def {
	d := wf.New("ex-routeback")
	prep := wf.NewAgent[exPrep, exOut](d)
	work := wf.NewAgent[exWork, exOut](d)
	done := wf.NewTerminal[exDone](d, wf.Success)
	gaveUp := wf.NewTerminal[exGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	prep.Spec(wf.PromptText("prep"), wf.Retry[exPrep](wf.Const(1), prep.To(gaveUp, "prep gave up")).NoRecovery())
	toWork := prep.To(work, "prepared")
	prep.Route(func(exOut, wf.Ctx) wf.Edge[exPrep] { return toWork }, toWork)
	work.Spec(wf.PromptText("work"), wf.Retry[exWork](wf.Const(1), work.To(prep, "back to prep")))
	ok := work.To(done, "ok")
	work.Route(func(exOut, wf.Ctx) wf.Edge[exWork] { return ok }, ok)
	wf.MaxVisits(wf.Const(4), work.To(gaveUp, "too many visits"))
	return d
}

// The restarts of one unit are capped across visits: after RecoveryRestarts
// restarts, the next exhaustion takes the edge with no offer, and each offer
// prints the restarts left.
func TestRecoveryRestartsCappedAcrossVisits(t *testing.T) {
	d := routeBackDef()
	h := newHarness(t, d, nil)
	h.manualRecovery = true
	run := h.run(d, nil)
	var left []int
	for i := 0; i < 60; i++ {
		ins := h.next(run)
		if len(ins) == 0 {
			break
		}
		in := ins[0]
		switch {
		case in.Kind == wf.InstructionRecovery:
			left = append(left, in.Recovery.Restarts)
			if err := h.e.Recover(run, in.Name, wf.RecoveryRestart); err != nil {
				t.Fatal(err)
			}
		case in.State == "exPrep":
			h.submit(run, in, `{"OK":true}`)
		default:
			if _, err := h.e.Fail(run, in.Token, "no"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Equal(left, []int{2, 1}) {
		t.Fatalf("offers printed restarts left %v, want [2 1]: two restarts, then no offer", left)
	}
	statusOf(t, h.status(run), wf.RunEnded, "exGaveUp")
}

// NoRecovery opts out: the exhausted edge at once, and the flag is in the
// view and the hash.
func TestNoRecoveryTakesTheEdge(t *testing.T) {
	d := routeBackDef()
	var prep *wf.RetryView
	for _, sv := range wf.Inspect(d).States {
		if sv.Name == "exPrep" {
			prep = sv.Retry
		}
		if sv.Name == "exWork" && (sv.Retry == nil || !sv.Retry.Recovery) {
			t.Fatalf("exWork retry view = %+v, want Recovery", sv.Retry)
		}
	}
	if prep == nil || prep.Recovery {
		t.Fatalf("exPrep retry view = %+v, want no Recovery (NoRecovery)", prep)
	}
	h := newHarness(t, d, nil)
	h.manualRecovery = true
	run := h.run(d, nil)
	if _, err := h.e.Fail(run, h.next(run)[0].Token, "no"); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "exGaveUp")
}

// A stall is offered too; give up takes the stalled edge.
func TestExhaustionStallRoutes(t *testing.T) {
	d := exDef("ex-stall", true)
	h := newHarness(t, d, nil)
	run := exRun(t, h, d, false)
	failWorkOnce(t, h, run, "same")
	failWorkOnce(t, h, run, "same")
	if rd := lastRefused(t, h, run); !rd.Recovery || rd.Exhausted != wf.ExhaustedStalled {
		t.Fatalf("refused = %+v, want a stall recovery offer", rd)
	}
	statusOf(t, h.status(run), wf.RunEnded, "exStuck")
}

// The lost cap is offered with no retry spent.
func TestExhaustionLostCapRoutes(t *testing.T) {
	d := lsDef("ex-lost", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run := h.run(d, nil)
	loseClaimedUnits(t, h, run, 3)
	if rd := lastRefused(t, h, run); !rd.Halt || !rd.Recovery || rd.Spent {
		t.Fatalf("lost cap = %+v, want the recovery offer with no retry spent", rd)
	}
	// B4: each loss is a degraded record: offered again, then the cap.
	var lost []string
	for _, d := range degradedOf(t, h, run) {
		if d.Kind == wf.DegradedLost {
			lost = append(lost, d.Action)
		}
	}
	if !slices.Equal(lost, []string{"offered again", "offered again", "lost cap: offered for recovery"}) {
		t.Fatalf("lost records = %q, want two re-offers then the cap offered for recovery", lost)
	}
	if st := h.status(run); st.Phase != wf.RunEnded || st.Outcome != wf.Stopped {
		t.Fatalf("status = %s, want ended stopped once the offer is given up", st.Phase)
	}
}

// wf retry on an ended run is rejected (exit 4), retry or stop.
func TestRetryOnEndedRunRejected(t *testing.T) {
	d := exDef("ex-ended", false)
	h := newHarness(t, d, nil)
	run := exRun(t, h, d, false)
	failWorkOnce(t, h, run, "a")
	failWorkOnce(t, h, run, "b")
	statusOf(t, h.status(run), wf.RunEnded, "exGaveUp")
	for _, stop := range []bool{false, true} {
		if err := h.e.Retry(run, "", stop); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "already ended") {
			t.Fatalf("Retry(stop=%v) on an ended run = %v, want ErrRejected", stop, err)
		}
	}
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: exDef("ex-ended", false)}}}
	state := filepath.Join(t.TempDir(), "state")
	cli := func(args ...string) (int, string) {
		var o, er bytes.Buffer
		code := wf.CLI(cfg, append(args, "--state-dir", state), &o, &er)
		return code, er.String()
	}
	if code, er := cli("run", "ex-ended", "--workspace", t.TempDir(), "--run-id", "x1"); code != 0 {
		t.Fatalf("run = %d %s", code, er)
	}
	for i := 0; i < 3; i++ {
		var ins []wf.Instruction
		var o bytes.Buffer
		wf.CLI(cfg, []string{"next", "x1", "1", "--state-dir", state, "--json"}, &o, &bytes.Buffer{})
		if err := json.Unmarshal(o.Bytes(), &ins); err != nil || len(ins) != 1 {
			t.Fatalf("next = %q", o.String())
		}
		if code, er := cli("prompt", "x1", "--claim", claimToken(t, ins[0])); code != 0 {
			t.Fatalf("prompt = %d %s", code, er)
		}
		if i == 0 {
			done := []string{"done", "x1", ins[0].Name}
			if ins[0].Output != "" {
				writeAllocated(t, ins[0].Output, `{"OK":true}`)
			} else {
				done = append(done, "--output", writeAllocated(t, filepath.Join(t.TempDir(), "o.json"), `{"OK":true}`))
			}
			cli(done...)
			continue
		}
		if code, er := cli("failed", "x1", ins[0].Name, "--reason", "no"); code != 0 {
			t.Fatalf("failed = %d %s", code, er)
		}
	}
	if code, er := cli("recover", "x1", "exWork#2", "give-up"); code != 0 {
		t.Fatalf("recover give-up = %d %s", code, er)
	}
	if code, er := cli("retry", "x1", "--stop"); code != 4 {
		t.Fatalf("retry --stop on the ended run = %d %s, want 4", code, er)
	}
	if code, er := cli("recover", "x1", "exWork#2", "give-up"); code != 4 {
		t.Fatalf("recover on the ended run = %d %s, want 4", code, er)
	}
}

// --stop answers only an exhaustion halt, which no run reaches any more: a
// plain halt is rejected (exit 4) and shows no fix lines.
func TestStopRejectedOnPlainHalt(t *testing.T) {
	d := lsDef("ex-plain", false) // no retry policy: a spent refusal halts plainly
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `"x"`)
	if err := h.e.Retry(run, "", true); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "not halted by retry exhaustion") {
		t.Fatalf("Retry(stop) on a plain halt = %v, want ErrRejected", err)
	}
	if st := h.status(run); len(st.Fix) != 0 {
		t.Fatalf("Fix = %q on a plain halt, want none", st.Fix)
	}
}

// A Map child whose budget is spent is offered; the spawner waits on the
// offer, and give up takes the child's own exhausted edge and settles it.
func TestExhaustedMapChildRoutes(t *testing.T) {
	d := retryMapDef(new(bool))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("next = %+v, want both items at work", ins)
	}
	for _, in := range ins {
		if in.Child == "RrFan[1]" {
			if _, err := h.e.Fail(run, in.Token, "no"); err != nil {
				t.Fatal(err)
			}
		} else {
			h.submit(run, in, `{"OK":true}`)
		}
	}
	if n := len(h.kind(run, wf.JournalChildSettled)); n != 1 {
		t.Fatalf("child_settled = %d, want only the item that succeeded settled while the other is offered", n)
	}
	statusOf(t, h.status(run), wf.RunEnded, "RrPart")
	if n := len(h.kind(run, wf.JournalChildSettled)); n != 2 {
		t.Fatalf("child_settled = %d, want both items settled after give-up", n)
	}
}

// B4: an exhaustion that takes its edge with no offer (here the restart's
// attempt failing too) is a degraded record of kind exhausted.
func TestExhaustionAfterRestartIsRecorded(t *testing.T) {
	d := exDef("ex-after-restart", false)
	h := newHarness(t, d, nil)
	h.manualRecovery = true
	run := exRun(t, h, d, true)
	failWorkOnce(t, h, run, "a")
	failWorkOnce(t, h, run, "b")
	if err := h.e.Recover(run, recoveryOffers(t, h, run)[0].Unit, wf.RecoveryRestart); err != nil {
		t.Fatal(err)
	}
	failWorkOnce(t, h, run, "c")
	statusOf(t, h.status(run), wf.RunEnded, "exGaveUp")
	var kinds []wf.DegradedKind
	for _, d := range degradedOf(t, h, run) {
		kinds = append(kinds, d.Kind)
	}
	if !slices.Equal(kinds, []wf.DegradedKind{wf.DegradedRecovery, wf.DegradedExhausted}) {
		t.Fatalf("degraded kinds = %v, want the restart, then exhausted", kinds)
	}
}

// Codex cross-review, crash window 2: an exhaustion answer is journaled and
// replayed. The process dies before or after the degraded record (before
// retried); a fresh engine finishes the restart (one more attempt, no second
// offer) with one degraded record, and a give-up takes its edge.
func TestExhaustionAnswerSurvivesACrashAtEveryRecord(t *testing.T) {
	for _, choice := range []string{wf.RecoveryRestart, wf.RecoveryGiveUp} {
		for _, at := range []wf.JournalKind{wf.JournalDegraded, wf.JournalRetried} {
			t.Run(choice+"/"+at.String(), func(t *testing.T) {
				d := exDef("ex-crash", false)
				h := newHarness(t, d, nil)
				h.manualRecovery = true
				run := exRun(t, h, d, true)
				failWorkOnce(t, h, run, "a")
				failWorkOnce(t, h, run, "b")
				unit := recoveryOffers(t, h, run)[0].Unit
				cj := &crashJournal{Journal: h.j, kind: at, armed: true}
				crashed := wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
				if err := crashed.Recover(run, unit, choice); !errors.Is(err, errCrash) {
					t.Fatalf("Recover = %v, want the crash at %s", err, at)
				}
				h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
				ins := h.next(run)
				if choice == wf.RecoveryRestart {
					if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent || ins[0].State != "exWork" {
						t.Fatalf("after the crash next = %+v, want the restarted Work attempt, not the offer again", ins)
					}
				} else {
					statusOf(t, h.status(run), wf.RunEnded, "exGaveUp")
				}
				if ds := degradedOf(t, h, run); len(ds) != 1 || ds[0].Kind != wf.DegradedRecovery {
					t.Fatalf("degraded = %+v, want the answer recorded once", ds)
				}
			})
		}
	}
}

// D3 with B2: a recovery give-up's end names the deciding check, read from
// the recovery halt's refusal (the edge rides the retried record).
func TestGiveUpCauseNamesTheRefusal(t *testing.T) {
	d := exDef("ex-cause", false)
	h := newHarness(t, d, nil)
	run := exRun(t, h, d, false)
	failWorkOnce(t, h, run, "a")
	failWorkOnce(t, h, run, "the deciding reason")
	st := h.status(run) // the harness gives the offer up
	if st.Phase != wf.RunEnded || !strings.Contains(st.Cause, "gave up") || !strings.Contains(st.Cause, "refused: abandoned: the deciding reason") {
		t.Fatalf("cause = %q, want the edge and the refused reason", st.Cause)
	}
}
