package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	lsAsk    struct{}
	lsDone   struct{}
	lsGaveUp struct{}
)

// lsDef is one agent, then a terminal; exhaust adds Retry(Const(2)) with a
// gave-up edge.
func lsDef(name string, retry bool) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[lsAsk, clAnswer](d)
	done := wf.NewTerminal[lsDone](d, wf.Success)
	gaveUp := wf.NewTerminal[lsGaveUp](d, wf.Stopped)
	if retry {
		a.Spec(wf.PromptText("Say hello."), wf.Retry[lsAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	} else {
		a.Spec(wf.PromptText("Say hello."), wf.RetryPolicy[lsAsk]{})
	}
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[lsAsk] { return next }, next)
	return d
}

func lsOpts() wf.EngineOptions {
	return wf.EngineOptions{HandOutWindow: 2 * time.Minute, SpawnWindow: 3 * time.Minute, FetchedLease: 30 * time.Minute, LostCap: 3}
}

func lostRecords(t *testing.T, h *harness, run wf.RunID) []wf.AttemptLostData {
	t.Helper()
	var out []wf.AttemptLostData
	for _, r := range h.kind(run, wf.JournalAttemptLost) {
		var d wf.AttemptLostData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestHandOutWindowExpiry(t *testing.T) {
	d := lsDef("lost-a", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, unit := handOneUnit(t, h, d)

	h.now = h.now.Add(time.Minute)
	if ins := h.handOut(run, 0); len(ins) != 0 || len(lostRecords(t, h, run)) != 0 {
		t.Fatalf("inside the window: %+v, losses %v", ins, lostRecords(t, h, run))
	}
	// Inside the window the unit is not offered again.
	if ins := h.handOut(run, 1); len(ins) != 0 {
		t.Fatalf("HandOut inside the window = %+v, want nothing", ins)
	}
	h.now = h.now.Add(61 * time.Second) // 2m1s after the hand-out
	h.handOut(run, 0)
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("attempt_lost = %+v: an expired hand-out is re-offered, never lost", got)
	}
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Name != unit.Name || again[0].Attempt != unit.Attempt {
		t.Fatalf("HandOut after the window = %+v, want %s offered again", again, unit.Name)
	}
	if again[0].Brief == unit.Brief {
		t.Fatalf("the re-offer carries the old brief (and claim token):\n%s", again[0].Brief)
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != 2 {
		t.Fatalf("handed_out records = %d, want 2 (offer and re-offer)", n)
	}
	// The re-offer opens a fresh window.
	h.now = h.now.Add(time.Minute)
	if ins := h.handOut(run, 1); len(ins) != 0 {
		t.Fatalf("HandOut inside the re-offer window = %+v, want nothing", ins)
	}
	// No retry was spent and the name is still claimable.
	if err := h.e.Started(run, unit.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Failed(run, unit.Name, "x"); err != nil {
		t.Fatal(err)
	}
	if st := h.status(run); st.Phase != wf.RunRunning {
		t.Fatalf("status %+v: the expiry spent a retry, the first Failed exhausted Const(2)", st)
	}
}

// The late child of an expired hand-out claims the same name by its prompt.
func TestExpiredHandOutLateChildPromptClaims(t *testing.T) {
	d := lsDef("lost-a2", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, unit := handOneUnit(t, h, d)
	h.now = h.now.Add(10 * time.Minute) // far past the hand-out window
	h.handOut(run, 0)
	if _, err := h.e.PromptClaim(run, unit.Name, claimToken(t, unit)); err != nil {
		t.Fatalf("late child's prompt: %v", err)
	}
	if got := attemptStarted(t, h, run); len(got) != 1 || got[0].Name != unit.Name {
		t.Fatalf("attempt_started = %+v, want one claim of %s", got, unit.Name)
	}
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("losses = %+v, want none", got)
	}
	if _, err := h.e.Done(run, unit.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatalf("done of the late child: %v", err)
	}
}

// A re-offer plus a second spawn: the second child's claim is refused and the
// first child's result is accepted.
func TestReofferSecondSpawnRefused(t *testing.T) {
	d := lsDef("lost-a3", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, first := handOneUnit(t, h, d)
	h.now = h.now.Add(121 * time.Second)
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Name != first.Name {
		t.Fatalf("re-offer = %+v", again)
	}
	t1, t2 := claimToken(t, first), claimToken(t, again[0])
	if t1 == t2 {
		t.Fatalf("a re-offer reused the claim token %s", t1)
	}
	if _, err := h.e.PromptClaim(run, first.Name, t1); err != nil {
		t.Fatalf("first child: %v", err)
	}
	_, err := h.e.PromptClaim(run, first.Name, t2)
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "another child already owns this unit") || !strings.Contains(err.Error(), "exit without working") {
		t.Fatalf("second child = %v, want ErrRejected another child already owns this unit, telling it to exit without working", err)
	}
	if _, err := h.e.PromptClaim(run, first.Name, t1); err != nil {
		t.Fatalf("same-token re-fetch: %v", err)
	}
	if _, err := h.e.Done(run, first.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatalf("first child's result: %v", err)
	}
}

// Each hand-out mints a fresh token that is journaled and rendered into the
// prompt command of its brief.
func TestHandOutMintsFreshClaimToken(t *testing.T) {
	d := lsDef("lost-a4", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, first := handOneUnit(t, h, d)
	h.now = h.now.Add(121 * time.Second)
	h.handOut(run, 1)
	recs := h.kind(run, wf.JournalHandedOut)
	if len(recs) != 2 {
		t.Fatalf("handed_out records = %d, want 2", len(recs))
	}
	var a, b wf.HandedOutData
	if err := json.Unmarshal(recs[0].Data, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(recs[1].Data, &b); err != nil {
		t.Fatal(err)
	}
	if a.Claim == "" || b.Claim == "" || a.Claim == b.Claim {
		t.Fatalf("journaled claims %q, %q: want two distinct non-empty tokens", a.Claim, b.Claim)
	}
	if !strings.Contains(first.Brief, "prompt "+string(run)+" --claim "+a.Claim) {
		t.Fatalf("brief lacks the --claim %s prompt command:\n%s", a.Claim, first.Brief)
	}
}

// reofferedUnit hands out d's one unit and lets the hand-out expire into a
// re-offer: the unit then has two journaled claim tokens, t1 and t2.
func reofferedUnit(t *testing.T, h *harness, d *wf.Def) (run wf.RunID, name, t1, t2 string) {
	t.Helper()
	run, first := handOneUnit(t, h, d)
	h.now = h.now.Add(121 * time.Second)
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Name != first.Name {
		t.Fatalf("re-offer = %+v", again)
	}
	return run, first.Name, claimToken(t, first), claimToken(t, again[0])
}

// Claim-token semantics: a started claim binds none, the first tokened prompt
// binds, a different token is refused, a tokenless prompt never binds or refuses.
func TestClaimTokenBinding(t *testing.T) {
	d := lsDef("lost-a5", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, name, t1, t2 := reofferedUnit(t, h, d)
	if err := h.e.Started(run, name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.PromptClaim(run, name, t1); err != nil {
		t.Fatalf("child A binds T1 on a started-claimed unit: %v", err)
	}
	_, err := h.e.PromptClaim(run, name, t2)
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "another child already owns this unit") {
		t.Fatalf("child B = %v, want ErrRejected another child already owns this unit", err)
	}
	if _, err := h.e.Prompt(run, name); err != nil {
		t.Fatalf("tokenless re-fetch: %v", err)
	}
	if _, err := h.e.PromptClaim(run, name, t2); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("a tokenless prompt changed the bound token: T2 = %v", err)
	}
	if _, err := h.e.PromptClaim(run, name, t1); err != nil {
		t.Fatalf("same-token re-fetch: %v", err)
	}
	if _, err := h.e.Done(run, name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatalf("A's result: %v", err)
	}
}

// A tokenless prompt claims but binds none: the next tokened prompt binds.
func TestTokenlessPromptBindsNoToken(t *testing.T) {
	d := lsDef("lost-a6", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, name, t1, t2 := reofferedUnit(t, h, d)
	if _, err := h.e.Prompt(run, name); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.PromptClaim(run, name, t2); err != nil {
		t.Fatalf("first tokened prompt after a tokenless claim: %v", err)
	}
	if _, err := h.e.PromptClaim(run, name, t1); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("t1 = %v, want refused once t2 bound", err)
	}
}

// Only a claim token a hand-out of this unit minted is accepted: a forged one,
// or another unit's, is ErrRejected and claims nothing.
func TestPromptClaimRejectsUnjournaledToken(t *testing.T) {
	d := lsDef("lost-a7", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, unit := handOneUnit(t, h, d)
	_, err := h.e.PromptClaim(run, unit.Name, "forged")
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "--claim") {
		t.Fatalf("forged token = %v, want ErrRejected", err)
	}
	if got := attemptStarted(t, h, run); len(got) != 0 {
		t.Fatalf("a forged token claimed the unit: %+v", got)
	}
	if _, err := h.e.PromptClaim(run, unit.Name, claimToken(t, unit)); err != nil {
		t.Fatalf("journaled token: %v", err)
	}
}

// claimToken is the token a hand-out's brief carries in its prompt command.
func claimToken(t *testing.T, ins wf.Instruction) string {
	t.Helper()
	_, rest, ok := strings.Cut(ins.Brief, " --claim ")
	if !ok {
		t.Fatalf("brief has no --claim:\n%s", ins.Brief)
	}
	tok, _, _ := strings.Cut(rest, ",")
	return strings.TrimSpace(strings.Fields(tok)[0])
}

func TestSpawnWindowExpiry(t *testing.T) {
	d := lsDef("lost-b", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, unit := handOneUnit(t, h, d)
	if err := h.e.Started(run, unit.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(2 * time.Minute)
	h.handOut(run, 0)
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("losses inside the spawn window = %d, want 0", n)
	}
	h.now = h.now.Add(61 * time.Second) // 3m1s after the start
	h.handOut(run, 0)
	got := lostRecords(t, h, run)
	if len(got) != 1 || got[0] != (wf.AttemptLostData{Name: unit.Name, Cause: "spawn expired"}) {
		t.Fatalf("attempt_lost = %+v, want one spawn expired", got)
	}
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("HandOut after the loss = %+v, want attempt 2", again)
	}

	// A prompt fetch inside the window keeps the unit.
	if err := h.e.Started(run, again[0].Name, claim("b")); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	if _, err := h.e.Prompt(run, again[0].Name); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Minute)
	h.handOut(run, 0)
	if n := len(lostRecords(t, h, run)); n != 1 {
		t.Fatalf("losses after a prompt fetch = %d, want still 1", n)
	}
}

func TestUnitLeaseExpiry(t *testing.T) {
	kid := wf.New("kid")
	child := wf.Typed[lItem, lRes](kid)
	work := wf.NewAgent[lWork, lOut](kid)
	kdone := wf.NewTerminal[lKDon](kid, wf.Success)
	work.Spec(wf.PromptText("Work."), wf.Retry(wf.Const(2), work.To(kdone, "gave up")))
	next := work.To(kdone, "done")
	work.Route(func(lOut, wf.Ctx) wf.Edge[lWork] { return next }, next)

	d := wf.New("lost-c")
	src := wf.FromObservation[lItem](wf.Observe[[]lItem]("x", "items", nil))
	wf.NewMap[lFan, lItem, string, lRes](d, src, func(i lItem) string { return i.ID }, child).Lease(10 * time.Minute)
	wf.NewTerminal[lEnd](d, wf.Success)

	opts := lsOpts()
	opts.SpawnWindow = time.Hour // Isolate the shorter Map lease from spawn expiry.
	h := newHarnessOpts(t, d, newWorld(), opts)
	run := h.run(d, nil)
	spawn := wf.JournalRecord{
		Kind: wf.JournalChildSpawned, Run: run, DefHash: h.records(run)[0].DefHash, At: h.now,
		Data: mustJSON(t, wf.ChildSpawnedData{ID: "kid-1", Def: "kid", State: "lFan"}),
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(spawn)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	ins := h.handOut(run, 1)
	if len(ins) != 1 {
		t.Fatalf("HandOut = %+v, want the child's unit", ins)
	}
	// No transcript: a transcript-backed unit is stalled at its lease instead
	// (TestLeaseExpiryStallsTranscriptUnit).
	if err := h.e.Started(run, ins[0].Name, wf.Claim{Harness: "claude", Session: "sess-1", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Prompt(run, ins[0].Name); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(9 * time.Minute)
	h.handOut(run, 0)
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("losses before the lease = %d, want 0", n)
	}
	h.now = h.now.Add(61 * time.Second) // 10m1s after the claim
	h.handOut(run, 0)
	got := askedThenDied(t, h, run, ins[0].Name) // a fetched unit's lease asks
	if len(got) != 1 || got[0] != (wf.AttemptLostData{Name: ins[0].Name, Cause: wf.LostCauseDied}) {
		t.Fatalf("attempt_lost = %+v, want one died", got)
	}
	if n := len(h.kind(run, wf.JournalLeaseExpired)); n != 0 {
		t.Fatalf("lease_expired records = %d, want 0 for a unit", n)
	}
}

// loseClaimedUnits hands the unit out, claims it by started and lets its spawn
// window pass, n times.
func loseClaimedUnits(t *testing.T, h *harness, run wf.RunID, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ins := h.handOut(run, 1)
		if len(ins) != 1 {
			t.Fatalf("hand-out %d = %+v, want one unit", i+1, ins)
		}
		if err := h.e.Started(run, ins[0].Name, claim("a")); err != nil {
			t.Fatal(err)
		}
		h.now = h.now.Add(3*time.Minute + time.Second)
		h.handOut(run, 0)
	}
}

func TestLostCapHaltsVisibly(t *testing.T) {
	d := lsDef("lost-d", false)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run := h.run(d, nil)
	loseClaimedUnits(t, h, run, 2)
	if st := h.status(run); st.Phase != wf.RunRunning {
		t.Fatalf("status %+v below the cap, want running", st)
	}
	loseClaimedUnits(t, h, run, 1)

	got := lostRecords(t, h, run)
	if len(got) != 3 || got[0].Cause != "spawn expired" || got[1].Cause != "spawn expired" || got[2].Cause != "lost cap" {
		t.Fatalf("attempt_lost causes = %+v, want two spawn expired then lost cap", got)
	}
	want := "lost cap: " + got[2].Name + " lost 3 times while started (last: spawn expired)"
	st := h.status(run)
	if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, want) {
		t.Fatalf("status = %+v, want blocked with reason containing %q", st, want)
	}
	if ins := h.handOut(run, 1); len(ins) != 0 {
		t.Fatalf("HandOut after the cap = %+v, want nothing", ins)
	}
}

// The lost cap of an agent state takes the exhausted edge (C3), with no retry
// spent.
func TestLostCapTakesFailureRoute(t *testing.T) {
	d := lsDef("lost-e", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run := h.run(d, nil)
	loseClaimedUnits(t, h, run, 3)

	refused := h.kind(run, wf.JournalRefused)
	if len(refused) != 1 {
		t.Fatalf("refused records = %d, want 1", len(refused))
	}
	var rd wf.RefusedData
	if err := json.Unmarshal(refused[0].Data, &rd); err != nil {
		t.Fatal(err)
	}
	if rd.Plan != nil || !rd.Halt || !rd.Recovery || rd.Spent || len(rd.Reasons) != 1 ||
		!strings.Contains(rd.Reasons[0], "lost cap: ") || !strings.Contains(rd.Reasons[0], "lost 3 times while started (last: spawn expired)") {
		t.Fatalf("refused = %+v, want the recovery offer with the lost-cap reason and no retry spent", rd)
	}
	st := h.status(run)
	if st.Phase != wf.RunEnded || st.Outcome != wf.Stopped {
		t.Fatalf("status = %+v, want ended Stopped via the exhausted edge", st)
	}
}
