package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func metadataAttached(t *testing.T, h *harness, run wf.RunID) []wf.MetadataAttachedData {
	t.Helper()
	var out []wf.MetadataAttachedData
	for _, r := range h.kind(run, wf.JournalMetadataAttached) {
		var d wf.MetadataAttachedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func startedNote(t *testing.T, h *harness, run wf.RunID, name string, cl wf.Claim) string {
	t.Helper()
	state, err := h.e.StartedNote(run, name, cl)
	if err != nil {
		t.Fatalf("StartedNote %s: %v", name, err)
	}
	return state
}

func TestStartedAfterDoneAttachesLate(t *testing.T) {
	d := clAgentDef("late-done")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, ins.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if got := startedNote(t, h, run, ins.Name, claim("a")); got != "done" {
		t.Fatalf("note state = %q, want done", got)
	}
	got := metadataAttached(t, h, run)
	want := wf.MetadataAttachedData{Name: ins.Name, Harness: "claude", Session: "sess-1", Agent: "a", Transcript: "/t/a.jsonl", State: "done", Late: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("metadata_attached = %+v, want [%+v]", got, want)
	}
	// A compatible repeat prints the note and journals nothing more.
	if got := startedNote(t, h, run, ins.Name, claim("a")); got != "done" || len(metadataAttached(t, h, run)) != 1 {
		t.Fatalf("repeat: state %q, %d records", got, len(metadataAttached(t, h, run)))
	}
	// Conflicting session or agent is refused and names the conflict.
	err := h.e.Started(run, ins.Name, wf.Claim{Session: "other", Agent: "a"})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), `session "sess-1" is already attached, not "other"`) {
		t.Fatalf("conflicting session = %v, want ErrRejected naming the conflict", err)
	}
	if err := h.e.Started(run, "no/such#1", claim("a")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("unknown name = %v, want ErrRejected", err)
	}
	if n := len(metadataAttached(t, h, run)); n != 1 {
		t.Fatalf("refusals journaled: %d records", n)
	}
}

func TestStartedOnRunningUnitAttachesLate(t *testing.T) {
	d := clAgentDef("late-running")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
	if got := startedNote(t, h, run, ins.Name, claim("a")); got != "running" {
		t.Fatalf("note state = %q, want running", got)
	}
	got := metadataAttached(t, h, run)
	if len(got) != 1 || got[0].State != "running" || !got[0].Late || got[0].Agent != "a" {
		t.Fatalf("metadata_attached = %+v, want one late record for agent a", got)
	}
}

// The first claim of a handed unit is not late and prints no note.
func TestStartedFirstClaimNoNote(t *testing.T) {
	d := clAgentDef("late-first")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	if got := startedNote(t, h, run, ins.Name, claim("a")); got != "" {
		t.Fatalf("first claim note state = %q, want none", got)
	}
	if n := len(metadataAttached(t, h, run)); n != 0 {
		t.Fatalf("first claim journaled %d metadata_attached", n)
	}
}

// A slow Codex-shaped spawner: twenty units are handed out in one batch, the
// hand-out window expires before most children are spawned, a wait reports the
// expired hand-outs and the next hand-out re-offers them under the same names.
// Children are spawned serially, a minute apart; each child's prompt claims its
// unit, and the coordinator's started for it trails that claim by more than
// SpawnWindow (but less than FetchedLease). Drives (hand-out and wait) run
// between every claim and every started. No unit is lost.
func TestSlowSpawnBatchLosesNoUnit(t *testing.T) {
	const n = 20
	ns := make([]int, n)
	for i := range ns {
		ns[i] = i + 1
	}
	d := fanDef("late-batch", chItems(ns...), n, wf.AllSettled())
	opts := lsOpts()
	h := newHarnessOpts(t, d, nil, opts)
	run := h.run(d, nil)
	units := h.handOut(run, -1)
	if len(units) != n {
		t.Fatalf("HandOut = %d units, want %d", len(units), n)
	}
	tokens := map[string]string{}
	for _, u := range units {
		tokens[u.Name] = claimToken(t, u)
	}
	drive := func() {
		t.Helper()
		h.handOut(run, 0)
		if _, err := h.e.Wait(run, -1, 5*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	// The hand-out window closes long before the last child exists.
	h.now = h.now.Add(opts.HandOutWindow + time.Minute)
	res, err := h.e.Wait(run, -1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if evs := eventsOf(res, wf.EventWindow); len(evs) != n {
		t.Fatalf("wait events = %+v, want %d re-offerable hand-outs", res.Events, n)
	}
	reoffer := h.handOut(run, -1)
	if len(reoffer) != n {
		t.Fatalf("re-offer = %d units, want all %d under their own names", len(reoffer), n)
	}
	for _, u := range reoffer {
		if _, ok := tokens[u.Name]; !ok {
			t.Fatalf("re-offer minted new name %s", u.Name)
		}
		tokens[u.Name] = claimToken(t, u)
	}
	// Serial spawns a minute apart; the started for child i arrives lag after
	// its prompt claim, while later children are still being spawned.
	const spawnGap = time.Minute
	lag := opts.SpawnWindow + 2*time.Minute
	if lag >= opts.FetchedLease {
		t.Fatalf("lag %v must stay inside FetchedLease %v", lag, opts.FetchedLease)
	}
	behind := int(lag / spawnGap)
	fetchedAt := make([]time.Time, n)
	for step := 0; step < n+behind; step++ {
		h.now = h.now.Add(spawnGap)
		if i := step; i < n {
			u := reoffer[i]
			if _, err := h.e.PromptClaim(run, u.Name, tokens[u.Name]); err != nil {
				t.Fatalf("prompt %s: %v", u.Name, err)
			}
			fetchedAt[i] = h.now
			drive()
		}
		if i := step - behind; i >= 0 {
			u := reoffer[i]
			if gap := h.now.Sub(fetchedAt[i]); gap <= opts.SpawnWindow || gap >= opts.FetchedLease {
				t.Fatalf("started %s %v after its prompt, want past SpawnWindow and inside FetchedLease", u.Name, gap)
			}
			cl := wf.Claim{Harness: "codex", Session: "coord", Agent: "child-" + u.Name, Transcript: "/t/" + u.Name}
			if state := startedNote(t, h, run, u.Name, cl); state != "running" {
				t.Fatalf("started %d (%s) state = %q, want running", i, u.Name, state)
			}
			drive()
		}
	}
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("losses = %+v, want none", got)
	}
	if got := len(metadataAttached(t, h, run)); got != n {
		t.Fatalf("metadata_attached = %d, want %d", got, n)
	}
	for _, u := range reoffer {
		if _, err := h.e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
			t.Fatalf("done %s: %v", u.Name, err)
		}
	}
}

// A late started on a live fetched unit renews its implicit lease: the lease
// runs FetchedLease from the started (RestartedAt), not from the prompt claim,
// and the renewal survives a fresh fold of the journal.
func TestLateAttachRenewsFetchedLease(t *testing.T) {
	d := lsDef("late-renew", true)
	opts := lsOpts()
	h := newHarnessOpts(t, d, nil, opts)
	run, ins := handOneUnit(t, h, d)
	fetched := h.now
	if _, err := h.e.PromptClaim(run, ins.Name, claimToken(t, ins)); err != nil {
		t.Fatal(err)
	}
	// A drive past SpawnWindow, before any started: the fetched unit stays.
	h.now = fetched.Add(opts.FetchedLease / 2)
	h.handOut(run, 0)
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("lost before the started: %+v", got)
	}
	late := h.now
	if state := startedNote(t, h, run, ins.Name, wf.Claim{Agent: "a"}); state != "running" {
		t.Fatalf("late started state = %q, want running", state)
	}
	if got := metadataAttached(t, h, run); len(got) != 1 || !got[0].Late {
		t.Fatalf("metadata_attached = %+v, want one late record", got)
	}
	h = resumeFrom(t, h, d, run, len(h.records(run)))
	h.now = fetched.Add(opts.FetchedLease + time.Minute) // past the lease from the claim
	h.handOut(run, 0)
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("lost to the claim's lease after a renewing started: %+v", got)
	}
	h.now = late.Add(opts.FetchedLease - time.Nanosecond)
	h.handOut(run, 0)
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("lost inside the renewed lease: %+v", got)
	}
	h.now = late.Add(opts.FetchedLease)
	h.handOut(run, 0)
	lost := askedThenDied(t, h, run, ins.Name) // the renewed lease ran out: asked
	if len(lost) != 1 || lost[0] != (wf.AttemptLostData{Name: ins.Name, Cause: wf.LostCauseDied}) {
		t.Fatalf("losses = %+v, want %s lost on the died answer", lost, ins.Name)
	}
}
