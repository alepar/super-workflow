package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// handOneUnit starts a run of the one-agent definition and hands its unit out.
func handOneUnit(t *testing.T, h *harness, d *wf.Def) (wf.RunID, wf.Instruction) {
	t.Helper()
	run := h.run(d, nil)
	ins := h.handOut(run, 1)
	if len(ins) != 1 || ins[0].Name == "" {
		t.Fatalf("HandOut(1) = %+v, want one unit", ins)
	}
	return run, ins[0]
}

func claim(agent string) wf.Claim {
	return wf.Claim{Harness: "claude", Session: "sess-1", Agent: agent, Transcript: "/t/" + agent + ".jsonl"}
}

func attemptStarted(t *testing.T, h *harness, run wf.RunID) []wf.AttemptStartedData {
	t.Helper()
	var out []wf.AttemptStartedData
	for _, r := range h.kind(run, wf.JournalAttemptStarted) {
		var d wf.AttemptStartedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestClaimRefusedForOtherAgent(t *testing.T) {
	d := clAgentDef("claims-a")
	h := newHarness(t, d, nil)
	run, unit := handOneUnit(t, h, d)

	if err := h.e.Started(run, unit.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	got := attemptStarted(t, h, run)
	want := wf.AttemptStartedData{Name: unit.Name, Harness: "claude", Session: "sess-1", Agent: "a", Transcript: "/t/a.jsonl"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("attempt_started = %+v, want exactly %+v", got, want)
	}

	if err := h.e.Started(run, unit.Name, claim("a")); err != nil {
		t.Fatalf("repeat claim by the same agent: %v", err)
	}
	if n := len(attemptStarted(t, h, run)); n != 1 {
		t.Fatalf("a repeat claim journaled %d attempt_started records, want 1", n)
	}

	err := h.e.Started(run, unit.Name, claim("b"))
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "agent \"a\" is already attached") {
		t.Fatalf("claim by another agent = %v, want ErrRejected naming agent a", err)
	}
	if n := len(attemptStarted(t, h, run)); n != 1 {
		t.Fatalf("a refused claim journaled; %d attempt_started records", n)
	}
}

func TestStartedRefusedUnlessHanded(t *testing.T) {
	// Two ready agents, a take of one: the second is pending, with no name yet.
	d := fanDef("claims-b", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	handed := h.handOut(run, 1)
	if len(handed) != 1 {
		t.Fatalf("HandOut(1) = %d units, want 1", len(handed))
	}
	// The second unit has no name until it is handed out; it would be the other item's.
	pending := strings.Replace(handed[0].Name, "/1/", "/2/", 1)
	if pending == handed[0].Name {
		t.Fatalf("cannot derive the pending unit's name from %q", handed[0].Name)
	}
	for _, name := range []string{"no/such/unit#1", pending} {
		if err := h.e.Started(run, name, claim("a")); !errors.Is(err, wf.ErrRejected) {
			t.Errorf("Started(%q) = %v, want ErrRejected", name, err)
		}
		if _, err := h.e.Prompt(run, name); !errors.Is(err, wf.ErrRejected) {
			t.Errorf("Prompt(%q) = %v, want ErrRejected", name, err)
		}
	}
	if n := len(attemptStarted(t, h, run)); n != 0 {
		t.Fatalf("refused claims journaled %d attempt_started records", n)
	}
	rest := h.handOut(run, 1)
	if len(rest) != 1 {
		t.Fatalf("second HandOut = %+v, want the pending unit", rest)
	}
	if err := h.e.Started(run, rest[0].Name, claim("a")); err != nil {
		t.Fatalf("Started on the now handed unit: %v", err)
	}
}

func TestHandedNeverStartedCostsNoTurn(t *testing.T) {
	d := clAgentDef("claims-c")
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run, unit := handOneUnit(t, h, d)

	// Lose the handed unit by hand: the hand-out window passed.
	recs := h.records(run)
	lost := wf.JournalRecord{
		Kind: wf.JournalAttemptLost, Run: run, State: unit.State, Attempt: unit.Attempt, DefHash: recs[0].DefHash, At: h.now,
		Data: mustJSON(t, wf.AttemptLostData{Name: unit.Name, Cause: wf.LostCauseSpawn}),
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(lost)
	unlock()
	if err != nil {
		t.Fatal(err)
	}

	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("after the loss HandOut = %+v, want the unit reissued at attempt 2", again)
	}
	if st := h.status(run); st.Phase != wf.RunRunning || len(h.kind(run, wf.JournalBudgetStopped)) != 0 {
		t.Fatalf("status %+v: the reissue of a never-started unit was budget-stopped", st)
	}

	// A started unit is a turn spent: its reissue is stopped by the budget.
	if err := h.e.Started(run, again[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Failed(run, again[0].Name, "gave up"); err != nil {
		t.Fatal(err)
	}
	if got := len(h.kind(run, wf.JournalBudgetStopped)); got != 1 {
		t.Fatalf("budget_stopped records = %d, want 1 once a started unit spent the turn", got)
	}
}

func TestPromptFetchJournals(t *testing.T) {
	d := clAgentDef("claims-d")
	h := newHarness(t, d, nil)
	run, unit := handOneUnit(t, h, d)

	if err := h.e.Started(run, unit.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	text, err := h.e.Prompt(run, unit.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Say hello.", "done " + string(run) + " " + unit.Name + " --output <file>"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, text)
		}
	}
	fetched := h.kind(run, wf.JournalPromptFetched)
	if len(fetched) != 1 {
		t.Fatalf("prompt_fetched records = %d, want 1", len(fetched))
	}
	var pf wf.PromptFetchedData
	if err := json.Unmarshal(fetched[0].Data, &pf); err != nil || pf.Name != unit.Name {
		t.Fatalf("prompt_fetched = %+v (%v), want the unit name", pf, err)
	}

	if _, err := h.e.Done(run, unit.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Prompt(run, unit.Name); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Prompt on a retired unit = %v, want ErrRejected", err)
	}
	if n := len(h.kind(run, wf.JournalPromptFetched)); n != 1 {
		t.Fatalf("refused prompts journaled: %d prompt_fetched records", n)
	}
}

func TestPromptClaimsHandedUnit(t *testing.T) {
	d := clAgentDef("prompt-claim")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	if _, err := h.e.Prompt(run, "no/such/unit#1"); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("invalid prompt = %v, want ErrRejected", err)
	}
	text, err := h.e.Prompt(run, ins.Name)
	if err != nil || !strings.Contains(text, "Say hello.") {
		t.Fatalf("handed prompt = %q, %v", text, err)
	}
	got := attemptStarted(t, h, run)
	if len(got) != 1 || got[0] != (wf.AttemptStartedData{Name: ins.Name}) {
		t.Fatalf("prompt claim = %+v, want one empty claim", got)
	}
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
	if n := len(attemptStarted(t, h, run)); n != 1 {
		t.Fatalf("reread created %d claims, want 1", n)
	}
	started, fetched := h.kind(run, wf.JournalAttemptStarted), h.kind(run, wf.JournalPromptFetched)
	if len(fetched) != 2 || started[0].Seq >= fetched[0].Seq {
		t.Fatalf("claim must precede fetch: started=%+v fetched=%+v", started, fetched)
	}
}

func TestStartedAttachesMetadataAfterPrompt(t *testing.T) {
	d := clAgentDef("late-metadata")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
	for _, cl := range []wf.Claim{{Session: "sess-1"}, {Agent: "a", Harness: "claude"}, {Transcript: "/t/a.jsonl"}, claim("a"), {Session: "sess-1"}} {
		if err := h.e.Started(run, ins.Name, cl); err != nil {
			t.Fatalf("compatible attachment %+v: %v", cl, err)
		}
	}
	got := metadataAttached(t, h, run)
	if len(got) != 3 || got[2] != (wf.MetadataAttachedData{Name: ins.Name, Harness: "claude", Session: "sess-1", Agent: "a", Transcript: "/t/a.jsonl", State: "running", Late: true}) {
		t.Fatalf("attachments = %+v, want three cumulative late claims", got)
	}
	for _, cl := range []wf.Claim{{Session: "other"}, {Agent: "other"}, {Transcript: "/other"}, {Harness: "other"}} {
		if err := h.e.Started(run, ins.Name, cl); !errors.Is(err, wf.ErrRejected) {
			t.Errorf("conflicting attachment %+v = %v, want ErrRejected", cl, err)
		}
	}
	if n := len(metadataAttached(t, h, run)); n != 3 {
		t.Fatalf("rejected attachments changed journal: %d attachments", n)
	}
}

func TestStartedAttachmentKeepsSettledElapsed(t *testing.T) {
	d := clAgentDef("late-elapsed")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	if err := h.e.Started(run, ins.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	if _, err := h.e.Done(run, ins.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	for _, u := range h.status(run).Units {
		if u.Name == ins.Name {
			if u.Elapsed != 2*time.Minute || u.Session != "sess-1" {
				t.Fatalf("settled elapsed/session = %s/%s, want 2m/sess-1", u.Elapsed, u.Session)
			}
			return
		}
	}
	t.Fatal("settled unit missing from status")
}

func TestDoneRetiredAndRefused(t *testing.T) {
	d := clAgentDef("claims-e")
	h := newHarness(t, d, nil)
	run, unit := handOneUnit(t, h, d)

	if _, err := h.e.Done(run, unit.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Done on an unclaimed unit = %v, want ErrRejected", err)
	}
	if err := h.e.Started(run, unit.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	res, err := h.e.Done(run, unit.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
	if err != nil || res.Class != wf.Accepted {
		t.Fatalf("Done = %+v, %v, want Accepted", res, err)
	}
	if st := h.status(run); st.Phase != wf.RunEnded {
		t.Fatalf("status = %+v, want the run to move on to its terminal", st)
	}
	if _, err := h.e.Done(run, unit.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Done on the retired name = %v, want ErrRejected", err)
	}

	// A refused output spends the attempt (C5) and reissues while retry
	// remains.
	d2 := clAgentDef("claims-e2")
	h = newHarness(t, d2, nil)
	run2, unit2 := handOneUnit(t, h, d2)
	if err := h.e.Started(run2, unit2.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	res, err = h.e.Done(run2, unit2.Name, wf.Submission{Output: json.RawMessage(`"not an object"`)})
	if err != nil || res.Class != wf.Refused {
		t.Fatalf("Done with a bad output = %+v, %v, want Refused", res, err)
	}
	next := h.handOut(run2, 1)
	if len(next) != 1 || next[0].Attempt != 2 || next[0].Name == unit2.Name {
		t.Fatalf("after the refusal HandOut = %+v, want a new unit at attempt 2", next)
	}
	if _, err := h.e.Done(run2, unit2.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Done on the refused attempt's name = %v, want ErrRejected", err)
	}
}

func TestFailedReissues(t *testing.T) {
	d := clAgentDef("claims-f")
	h := newHarness(t, d, nil)
	run, unit := handOneUnit(t, h, d)
	if _, err := h.e.Failed(run, unit.Name, "x"); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Failed on an unclaimed unit = %v, want ErrRejected", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := h.e.Started(run, unit.Name, claim("a")); err != nil {
			t.Fatal(err)
		}
		res, err := h.e.Failed(run, unit.Name, "could not finish")
		if err != nil || res.Class != wf.Refused || !strings.Contains(strings.Join(res.Refusal, " "), "could not finish") {
			t.Fatalf("attempt %d Failed = %+v, %v, want Refused naming the reason", attempt, res, err)
		}
		if attempt == 1 {
			ins := h.handOut(run, 1)
			if len(ins) != 1 || ins[0].Attempt != 2 {
				t.Fatalf("after Failed HandOut = %+v, want a new unit at attempt 2", ins)
			}
			unit = ins[0]
		}
	}
	// Retry(Const(2)) is spent: the state halts, and stop takes its exhausted edge.
	st := h.status(run)
	if st.Phase != wf.RunEnded || st.Outcome != wf.Stopped {
		t.Fatalf("status = %+v, want the run ended Stopped via the exhausted edge", st)
	}
	if ins := h.handOut(run, 1); len(ins) != 1 || ins[0].Kind != wf.InstructionEnded || ins[0].Terminal != "clGaveUp" {
		t.Fatalf("HandOut after exhaustion = %+v, want the single ended instruction at clGaveUp", ins)
	}
}

func TestMapLeaseFromClaim(t *testing.T) {
	kid := wf.New("kid")
	child := wf.Typed[lItem, lRes](kid)
	work := wf.NewAgent[lWork, lOut](kid)
	kdone := wf.NewTerminal[lKDon](kid, wf.Success)
	work.Spec(wf.PromptText("Work."), wf.Retry(wf.Const(2), work.To(kdone, "gave up")))
	next := work.To(kdone, "done")
	work.Route(func(lOut, wf.Ctx) wf.Edge[lWork] { return next }, next)

	d := wf.New("claims-lease")
	src := wf.FromObservation[lItem](wf.Observe[[]lItem]("x", "items", nil))
	wf.NewMap[lFan, lItem, string, lRes](d, src, func(i lItem) string { return i.ID }, child).Lease(10 * time.Minute)
	wf.NewTerminal[lEnd](d, wf.Success)

	h := newHarness(t, d, newWorld())
	run := h.run(d, nil)
	recs := h.records(run)
	spawn := wf.JournalRecord{
		Kind: wf.JournalChildSpawned, Run: run, DefHash: recs[0].DefHash, At: h.now,
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
	if len(ins) != 1 || ins[0].Name == "" {
		t.Fatalf("HandOut = %+v, want the child's unit", ins)
	}
	if n := len(h.kind(run, wf.JournalLeaseClaimed)); n != 0 {
		t.Fatalf("lease_claimed records at hand-out = %d, want 0: the lease starts at the claim", n)
	}

	h.now = h.now.Add(90 * time.Second)
	started := h.now
	if err := h.e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	got := attemptStarted(t, h, run)
	if len(got) != 1 || !got[0].Expires.Equal(started.Add(10*time.Minute)) {
		t.Fatalf("attempt_started = %+v, want Expires = the claim time + 10m", got)
	}
	if n := len(h.kind(run, wf.JournalLeaseClaimed)); n != 0 {
		t.Fatalf("lease_claimed records after the claim = %d, want 0", n)
	}
}

func TestUnavailableBackoffEndsClaimAndOffersFreshUnit(t *testing.T) {
	d := engBackoffDef()
	h := newHarness(t, d, nil)
	run, first := handOneUnit(t, h, d)
	if err := h.e.Started(run, first.Name, claim("old")); err != nil {
		t.Fatal(err)
	}
	res, err := h.e.Done(run, first.Name, wf.Submission{Output: json.RawMessage(`{"Down":true}`)})
	if err != nil || res.Class != wf.Unavailable {
		t.Fatalf("Done = %+v, %v, want Unavailable", res, err)
	}
	if got := h.kind(run, wf.JournalBackoffScheduled); len(got) != 1 {
		t.Fatalf("backoffs = %d, want 1", len(got))
	}
	h.now = h.now.Add(10 * time.Second)
	offered := h.handOut(run, 1)
	if len(offered) != 1 || offered[0].Kind != wf.InstructionAgent || offered[0].Name == first.Name {
		t.Fatalf("after backoff offered = %+v, want a fresh unit replacing %s", offered, first.Name)
	}
	next := offered[0]
	if next.Attempt != first.Attempt {
		t.Fatalf("attempt = %d, want %d (backoff spends no retry)", next.Attempt, first.Attempt)
	}
	var oldOffer wf.HandedOutData
	if err := json.Unmarshal(h.kind(run, wf.JournalHandedOut)[0].Data, &oldOffer); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.PromptClaim(run, first.Name, oldOffer.Claim); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("old claim = %v, want rejected", err)
	}
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: first.Name}, true); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("old heartbeat = %v, want rejected", err)
	}
	// Late started metadata remains idempotent; it must not revive the unit.
	if note, err := h.e.StartedNote(run, first.Name, claim("old")); err != nil || note != "failed" {
		t.Fatalf("late started = %q, %v, want failed without revival", note, err)
	}
	if _, err := h.e.Done(run, first.Name, wf.Submission{Output: json.RawMessage(`{"Down":false}`)}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("old Done = %v, want rejected", err)
	}
	if err := h.e.Started(run, next.Name, claim("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, next.Name, wf.Submission{Output: json.RawMessage(`{"Down":false}`)}); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Hour)
	if got := h.handOut(run, 1); len(got) != 1 || got[0].Kind != wf.InstructionEnded {
		t.Fatalf("after completion offered = %+v, want ended", got)
	}
	statusOf(t, h.status(run), wf.RunEnded, "bDone")
	for _, r := range h.kind(run, wf.JournalHeartbeat) {
		var beat wf.HeartbeatData
		if err := json.Unmarshal(r.Data, &beat); err != nil {
			t.Fatal(err)
		}
		if beat.Status == "stalled" {
			t.Fatalf("unexpected stalled heartbeat: %+v", beat)
		}
	}
}
