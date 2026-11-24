package wf_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// laDef is lsDef with the workflow's per-step lease length (StallAfter).
func laDef(name string, lease time.Duration) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[lsAsk, clAnswer](d)
	if lease > 0 {
		a.Dispatch(wf.StallAfter(lease))
	}
	done := wf.NewTerminal[lsDone](d, wf.Success)
	gaveUp := wf.NewTerminal[lsGaveUp](d, wf.Stopped)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[lsAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[lsAsk] { return next }, next)
	return d
}

// fetchedUnit hands out the one unit and fetches its prompt with its claim.
func fetchedUnit(t *testing.T, h *harness, d *wf.Def) (wf.RunID, wf.Instruction) {
	t.Helper()
	run, ins := handOneUnit(t, h, d)
	if _, err := h.e.PromptClaim(run, ins.Name, claimToken(t, ins)); err != nil {
		t.Fatal(err)
	}
	return run, ins
}

// The workflow sets the lease per step: a unit whose step says 5m is asked
// about at 5m, not at the engine's 30m default, and is not lost by it.
func TestLeaseLengthIsPerStep(t *testing.T) {
	h := newHarnessOpts(t, laDef("la-step", 5*time.Minute), nil, lsOpts())
	run, ins := fetchedUnit(t, h, laDef("la-step", 5*time.Minute))
	start := h.now
	h.now = start.Add(5*time.Minute - time.Nanosecond)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 0 {
		t.Fatalf("asked %d times inside the step's lease", n)
	}
	h.now = start.Add(5 * time.Minute)
	h.handOut(run, 0)
	if lost := askedThenDied(t, h, run, ins.Name); len(lost) != 1 || lost[0].Cause != wf.LostCauseDied {
		t.Fatalf("losses = %+v, want one died", lost)
	}
}

// A heartbeat active answers the question: the unit runs on, with a fresh
// lease from the answer, and is asked again only when that one runs out.
// Being asked is no loss: nothing is handed out again until a died answer.
func TestLeaseQuestionExtendAnswer(t *testing.T) {
	d := laDef("la-extend", 5*time.Minute)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, ins := fetchedUnit(t, h, d)
	start := h.now
	h.now = start.Add(5 * time.Minute)
	if again := h.handOut(run, 1); len(again) != 0 {
		t.Fatalf("handed out %+v while the coordinator is asked", again)
	}
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled = %d, want the one question", n)
	}
	h.now = start.Add(time.Hour) // unanswered, it stays asked: no second question, no loss
	h.handOut(run, 0)
	if n, lost := countStatus(heartbeats(t, h, run), "stalled"), lostRecords(t, h, run); n != 1 || len(lost) != 0 {
		t.Fatalf("an unanswered question: stalled=%d lost=%+v", n, lost)
	}
	answered := h.now
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, true); err != nil {
		t.Fatal(err)
	}
	h.now = answered.Add(5*time.Minute - time.Nanosecond)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("asked again inside the extended lease: %d", n)
	}
	h.now = answered.Add(5 * time.Minute)
	h.handOut(run, 0)
	if n, lost := countStatus(heartbeats(t, h, run), "stalled"), lostRecords(t, h, run); n != 2 || len(lost) != 0 {
		t.Fatalf("after the extension ran out: stalled=%d lost=%+v, want asked again", n, lost)
	}
}

// A validate from the child is a sign of life: one past a quarter of the lease
// renews it (a heartbeat active by child), one inside that quarter writes
// nothing, and one while the unit is asked answers the question.
func TestValidateRenewsTheLease(t *testing.T) {
	d := laDef("la-validate", 8*time.Minute)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, ins := fetchedUnit(t, h, d)
	start := h.now
	validate := func() {
		t.Helper()
		if _, err := h.e.Validate(run, ins.Name, wf.Submission{Output: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("validate: %v", err)
		}
	}
	byChild := func() int {
		n := 0
		for _, hb := range heartbeats(t, h, run) {
			if hb.Status == "active" && hb.By == "child" {
				n++
			}
		}
		return n
	}
	h.now = start.Add(time.Minute)
	validate()
	if n := byChild(); n != 0 {
		t.Fatalf("a validate inside the first quarter renewed: %d", n)
	}
	h.now = start.Add(6 * time.Minute)
	validate()
	if n := byChild(); n != 1 {
		t.Fatalf("a validate past a quarter of the lease renewed %d times, want 1", n)
	}
	renewed := h.now
	h.now = start.Add(8 * time.Minute) // the original lease: renewed, not asked
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 0 {
		t.Fatalf("asked inside the renewed lease: %d", n)
	}
	h.now = renewed.Add(8 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("not asked when the renewed lease ran out: %d", n)
	}
	validate() // the child is alive after all: its call answers the question
	if n := byChild(); n != 2 {
		t.Fatalf("a validate while asked renewed %d times in all, want 2", n)
	}
	if st := h.status(run); len(st.Units) != 1 || st.Units[0].State != "running" {
		t.Fatalf("status after the answering validate: %+v", st.Units)
	}
}

// Review round 1 P2: the validate debounce runs against the lease the unit
// lives by, a Map's when the workflow declared one: with Map.Lease(10m), a
// validate at minute 5 renews it to minute 15, so minute 10 asks nothing.
func TestValidateRenewsAMapLease(t *testing.T) {
	h, _, run, ins := lvLeasedHandedOut(t, 11*time.Hour) // no stall from the class
	claimAt := h.now
	if err := h.e.Started(run, ins.Name, wf.Claim{Harness: "claude", Session: "sess-1", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
	h.now = claimAt.Add(5 * time.Minute)
	if _, err := h.e.Validate(run, ins.Name, wf.Submission{Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	hbs := heartbeats(t, h, run)
	if len(hbs) != 1 || hbs[0].By != "child" || !hbs[0].Expires.Equal(claimAt.Add(15*time.Minute)) {
		t.Fatalf("heartbeats = %+v, want one child renewal to claim+15m", hbs)
	}
	h.now = claimAt.Add(10 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 0 {
		t.Fatalf("asked at the original Map lease despite the renewal: %d", n)
	}
}

// Review round 1 P2: a child that fetches its prompt again under its bound
// token is alive, so the re-fetch answers its open question: the unit runs
// again and next no longer lists it.
func TestRefetchAnswersTheQuestion(t *testing.T) {
	d := laDef("la-refetch", 5*time.Minute)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, ins := fetchedUnit(t, h, d)
	h.now = h.now.Add(5 * time.Minute)
	h.handOut(run, 0)
	if st := h.status(run); len(st.Units) != 1 || st.Units[0].State != "stalled" {
		t.Fatalf("status at the lease = %+v, want stalled", st.Units)
	}
	if _, err := h.e.PromptClaim(run, ins.Name, claimToken(t, ins)); err != nil {
		t.Fatal(err)
	}
	if st := h.status(run); len(st.Units) != 1 || st.Units[0].State != "running" {
		t.Fatalf("status after the re-fetch = %+v, want running", st.Units)
	}
	res, err := h.e.(*wf.Engine).NextWait(run, 0, wf.Coordinator{}, time.Millisecond)
	if err != nil || len(res.Stalled) != 0 {
		t.Fatalf("next after the re-fetch = %+v, %v; want no question", res, err)
	}
}
