package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func handOutAs(t *testing.T, h *harness, run wf.RunID, n int, session string) []wf.Instruction {
	t.Helper()
	ins, err := h.e.(*wf.Engine).HandOutAs(run, n, wf.Coordinator{Harness: "codex", Session: session})
	if err != nil {
		t.Fatal(err)
	}
	var out []wf.Instruction
	for _, in := range ins {
		if in.Kind == wf.InstructionAgent || in.Kind == wf.InstructionRecovery {
			out = append(out, in)
		}
	}
	return out
}

// handOutAsH is handOutAs for a coordinator of harness: its children are of
// that harness too (a child's verbs come from its hand-out's, an earlier revision).
func handOutAsH(t *testing.T, h *harness, run wf.RunID, n int, session, harness string) []wf.Instruction {
	t.Helper()
	ins, err := h.e.(*wf.Engine).HandOutAs(run, n, wf.Coordinator{Harness: harness, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	var out []wf.Instruction
	for _, in := range ins {
		if in.Kind == wf.InstructionAgent || in.Kind == wf.InstructionRecovery {
			out = append(out, in)
		}
	}
	return out
}

func fetchAs(t *testing.T, h *harness, run wf.RunID, in wf.Instruction, who hn.Caller) {
	t.Helper()
	if _, err := h.e.(*wf.Engine).PromptFetchAs(run, in.Name, claimToken(t, in), who); err != nil {
		t.Fatalf("prompt %s: %v", in.Name, err)
	}
}

func codexChild(session, agent string) hn.Caller {
	return hn.Caller{Harness: "codex", Session: session, Agent: agent, Is: hn.IsChild, FromEnv: true}
}

// B1/B2: a 40-unit next whose spawns are spread over 20 minutes loses nothing:
// a hand-out has no clock before its child's prompt fetch claims it. The
// coordinator's polls take no new units (n 0), so nothing is offered again;
// every late child claims with the token of the one offer.
func TestFortyUnitHandOutSpreadOverTwentyMinutesLosesNothing(t *testing.T) {
	items := make([]int, 40)
	for i := range items {
		items[i] = i + 1
	}
	d := fanDef("proto-forty", chItems(items...), 40, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAs(t, h, run, 40, "boss")
	if len(ins) != 40 {
		t.Fatalf("handed %d units, want 40", len(ins))
	}
	for i, in := range ins {
		h.now = h.now.Add(30 * time.Second)
		fetchAs(t, h, run, in, codexChild("boss", "kid-"+in.Name))
		if again := handOutAs(t, h, run, 0, "boss"); len(again) != 0 {
			t.Fatalf("after %d spawns the coordinator was offered %d units again", i+1, len(again))
		}
	}
	if lost := h.kind(run, wf.JournalAttemptLost); len(lost) != 0 {
		t.Fatalf("%d units lost", len(lost))
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != 40 {
		t.Fatalf("%d handed_out records, want 40 (no re-offer)", n)
	}
	if st := h.status(run); st.UnitCounts.Running != 40 {
		t.Fatalf("counts %+v, want 40 running", st.UnitCounts)
	}
}

// B4: a run whose Codex coordinator died with three children running,
// resumed by a new session 65 s later (inside the children's stall
// threshold), offers all three again at once with no manual verb, each with a
// degraded record: Codex children are threads of the coordinator's process.
// A unit handed to the old session and never claimed is offered again with a
// degraded record too.
func TestNewSessionReoffersDeadCoordinatorsUnits(t *testing.T) {
	d := fanDef("proto-orphans", chItems(1, 2, 3, 4), 4, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAs(t, h, run, 4, "old")
	for _, in := range ins[:3] {
		fetchAs(t, h, run, in, codexChild("old", "kid-"+in.Name))
	}
	h.now = h.now.Add(65 * time.Second)
	again := handOutAs(t, h, run, 10, "new")
	if len(again) != 4 {
		t.Fatalf("new session was offered %d units, want 4: %+v", len(again), again)
	}
	var degraded []wf.DegradedData
	for _, r := range h.kind(run, wf.JournalDegraded) {
		var dd wf.DegradedData
		if err := json.Unmarshal(r.Data, &dd); err != nil {
			t.Fatal(err)
		}
		degraded = append(degraded, dd)
	}
	if len(degraded) != 4 {
		t.Fatalf("degraded = %+v, want four orphan records", degraded)
	}
	for i, dd := range degraded {
		want := "died with it"
		if i == 3 {
			want = "never claimed"
		}
		if dd.Kind != wf.DegradedOrphan || !strings.Contains(dd.Cause, "old") || !strings.Contains(dd.Cause, want) {
			t.Errorf("degraded[%d] = %+v, want an orphan record naming the old session and %q", i, dd, want)
		}
	}
	if lost := h.kind(run, wf.JournalAttemptLost); len(lost) != 3 {
		t.Fatalf("%d attempt_lost, want 3", len(lost))
	}
	for _, r := range h.kind(run, wf.JournalHandedOut)[4:] {
		var ho wf.HandedOutData
		if err := json.Unmarshal(r.Data, &ho); err != nil || ho.Session != "new" || ho.Harness != "codex" {
			t.Fatalf("re-offer handed_out = %+v, want session new under codex", ho)
		}
	}
}

// B4: on a harness whose children may outlive their coordinator, a child of
// the old session is kept: never swept for going quiet (rule 5). Once its
// lease runs out the new session is asked about it, and its died answer
// hands the unit out again.
func TestNewSessionKeepsLiveChildrenUntilTheyGoQuiet(t *testing.T) {
	d := fanDef("proto-live", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAsH(t, h, run, 1, "old", "remote")
	fetchAs(t, h, run, ins[0], hn.Caller{Harness: "remote", Session: "old", Agent: "kid", Is: hn.IsChild, FromEnv: true})
	h.now = h.now.Add(10 * time.Second)
	if again := handOutAsH(t, h, run, 1, "new", "remote"); len(again) != 0 {
		t.Fatalf("a live child's unit was offered again: %+v", again)
	}
	if n := len(h.kind(run, wf.JournalDegraded)); n != 0 {
		t.Fatalf("%d degraded records for a live child", n)
	}
	h.now = h.now.Add(25 * time.Minute) // quiet past the stall default
	if again := handOutAsH(t, h, run, 1, "new", "remote"); len(again) != 0 || len(h.kind(run, wf.JournalDegraded)) != 0 {
		t.Fatalf("the quiet child of the old session was swept on elapsed time: %+v", again)
	}
	h.now = h.now.Add(10 * time.Minute) // past the 30m lease: asked
	if again := handOutAsH(t, h, run, 1, "new", "remote"); len(again) != 0 || countStatus(heartbeats(t, h, run), "stalled") != 1 {
		t.Fatalf("past the lease: offered %+v, stalled %d; want the question", again, countStatus(heartbeats(t, h, run), "stalled"))
	}
	if err := h.e.(*wf.Engine).HeartbeatAs(run, wf.HeartbeatTarget{Name: ins[0].Name}, false, wf.Coordinator{Harness: "remote", Session: "new"}); err != nil {
		t.Fatal(err)
	}
	again := handOutAsH(t, h, run, 1, "new", "remote")
	if len(again) != 1 {
		t.Fatalf("after the died answer: %+v, want the unit offered again", again)
	}
	if st, err := h.e.Status(run); err != nil || st.Phase != wf.RunRunning {
		t.Fatalf("status %+v, %v", st.Phase, err)
	}
}

func TestNextRejectsSpawnedOrRefusedChildCoordinator(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("prompt-refused=%t", refused), func(t *testing.T) {
			d := fanDef("proto-child-next", chItems(1, 2), 2, wf.AllSettled())
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			first := handOutAsH(t, h, run, 1, "boss", "pi")[0]
			e := h.e.(*wf.Engine)
			child := "wf-" + claimToken(t, first)
			if refused {
				child = "unattributed-child"
				who := hn.Caller{Harness: "pi", Session: child, Is: hn.IsSelf, FromEnv: true}
				if _, err := e.PromptFetchAs(run, first.Name, claimToken(t, first), who); !errors.Is(err, wf.ErrRejected) {
					t.Fatalf("child's prompt fetch = %v, want refused", err)
				}
			}
			if got, err := e.HandOutAs(run, wf.NextAuto, wf.Coordinator{Harness: "pi", Session: child}); !errors.Is(err, wf.ErrRejected) {
				t.Fatalf("child's next = %v, %v, want refused", got, err)
			}
			if n := len(h.kind(run, wf.JournalHandedOut)); n != 1 {
				t.Fatalf("refused next left %d hand-outs, want 1", n)
			}
		})
	}
}

func TestCoordinatorResumeClearsOnlyRefusalEvidence(t *testing.T) {
	d := fanDef("proto-child-resume", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	offered := handOutAsH(t, h, run, 1, "boss", "pi")[0]
	e := h.e.(*wf.Engine)
	token := claimToken(t, offered)

	replacement := wf.Coordinator{Harness: "pi", Session: "replacement"}
	refusePrompt := func(co wf.Coordinator, offer wf.Instruction) {
		t.Helper()
		who := hn.Caller{Harness: co.Harness, Session: co.Session, Is: hn.IsSelf, FromEnv: true}
		if _, err := e.PromptFetchAs(run, offer.Name, claimToken(t, offer), who); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("%s's prompt fetch = %v, want refused", co.Session, err)
		}
	}
	refusePrompt(replacement, offered)
	repeated := wf.Coordinator{Harness: "pi", Session: "repeated"}
	for range 22 {
		refusePrompt(repeated, offered) // exhaust the refusal audit cap in the first segment
	}
	if err := e.Stop(run, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	child := wf.Coordinator{Harness: "pi", Session: "wf-" + token}
	if _, err := e.HandOutAs(run, wf.NextAuto, child); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("spawned child's next in resumed segment = %v, want refused", err)
	}
	got, err := e.HandOutAs(run, wf.NextAuto, replacement)
	again := agentsOf(got)
	if err != nil || len(again) != 2 {
		t.Fatalf("replacement's first next after resume gave %d offers, %v; want both units", len(again), err)
	}
	i := slices.IndexFunc(again, func(a wf.Instruction) bool { return a.Name == offered.Name })
	if i < 0 {
		t.Fatal("replacement did not receive the previously offered unit")
	}
	refusePrompt(repeated, again[i])
	if _, err := e.HandOutAs(run, wf.NextAuto, repeated); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("new refusal after the previous segment's cap = %v, want refused next", err)
	}
}

func TestExpiredHandoutsFollowMostRecentCoordinatorDrive(t *testing.T) {
	d := fanDef("proto-handout-return", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first := handOutAsH(t, h, run, 1, "original", "pi")[0]
	fetchAs(t, h, run, first, hn.Caller{Harness: "pi", Session: "original", Agent: "kid", Is: hn.IsChild, FromEnv: true})
	stranded := handOutAsH(t, h, run, 2, "interloper", "pi")
	if len(stranded) != 2 {
		t.Fatalf("interloper has %d hand-outs, want 2", len(stranded))
	}
	h.now = h.now.Add(time.Minute)
	if early := handOutAsH(t, h, run, 2, "original", "pi"); len(early) != 0 {
		t.Fatalf("re-offered %d still inside their window", len(early))
	}
	h.now = h.now.Add(time.Minute)
	again := handOutAsH(t, h, run, 2, "original", "pi")
	if len(again) != len(stranded) {
		t.Fatalf("re-offered %d stale units, want %d", len(again), len(stranded))
	}
	for i, unit := range again {
		if unit.Name != stranded[i].Name || claimToken(t, unit) == claimToken(t, stranded[i]) {
			t.Fatalf("re-offer %d does not keep its name and mint a new claim", i)
		}
	}
	if n := len(h.kind(run, wf.JournalAttemptLost)); n != 0 {
		t.Fatalf("re-offering unfetched units lost %d claimed units", n)
	}
	var records int
	for _, record := range h.kind(run, wf.JournalDegraded) {
		var event wf.DegradedData
		if err := json.Unmarshal(record.Data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == wf.DegradedUnstarted && event.Action == "handed out again" {
			records++
		}
	}
	if records != len(stranded) {
		t.Fatalf("journal attributed %d stale re-offers, want %d", records, len(stranded))
	}
}

// J4: nothing is lost before a claim. A coordinator with a session gets its
// own unclaimed hand-out back by time only once UnstartedGrace (5 minutes,
// an earlier revision) passed, marked never started, and nothing is lost even after a
// 10-minute gap; at the end of a wait with nothing else its blocking next lists it
// again ("not started yet") under the same name with a fresh token, and the
// first offer's child that does start still claims it while the second is
// told the unit is taken. One the engine cannot name gets it again once
// nothing of the run is claimed for the hand-out window, and never while a
// child runs.
func TestIdleHandOutIsOfferedAgain(t *testing.T) {
	d := fanDef("proto-idle", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first := handOutAs(t, h, run, 2, "boss")
	h.now = h.now.Add(4 * time.Minute)
	if again := handOutAs(t, h, run, 2, "boss"); len(again) != 0 {
		t.Fatalf("offered again inside the grace: %+v", again)
	}
	h.now = h.now.Add(6 * time.Minute)
	if again := handOutAs(t, h, run, 2, "boss"); len(again) != 2 || !again[0].NotStarted || again[0].Name != first[0].Name {
		t.Fatalf("after 10 minutes = %+v, want both offered again as never started", again)
	}
	if lost := h.kind(run, wf.JournalAttemptLost); len(lost) != 0 {
		t.Fatalf("a slow spawn lost %d units", len(lost))
	}
	res, err := h.e.(*wf.Engine).NextWait(run, wf.NextAuto, wf.Coordinator{Harness: "codex", Session: "boss"}, 50*time.Millisecond)
	again := agentsOf(res.Instructions)
	if err != nil || !res.Relisted || len(again) != 2 || again[0].Name != first[0].Name || claimToken(t, again[0]) == claimToken(t, first[0]) {
		t.Fatalf("the end of a wait = %+v, %v; want both listed again under their names with fresh tokens", res, err)
	}
	if out := wf.RenderNext(res, "codex", func(v string, a ...string) string { return v }); !strings.HasPrefix(out, wf.NextRelisted+"\n") {
		t.Errorf("a re-list does not say not started yet:\n%.200s", out)
	}
	fetchAs(t, h, run, first[0], codexChild("boss", "k1"))
	if _, err := h.e.(*wf.Engine).PromptFetchAs(run, again[0].Name, claimToken(t, again[0]), codexChild("boss", "k2")); err == nil || !strings.Contains(err.Error(), "another child already owns this unit; exit without working") {
		t.Errorf("the second child of a re-listed unit = %v, want told the unit is taken", err)
	}

	h2 := newHarness(t, d, nil)
	run2 := h2.run(d, nil)
	ins := handOutAs(t, h2, run2, 2, "")
	h2.now = h2.now.Add(time.Minute)
	if again := handOutAs(t, h2, run2, 2, ""); len(again) != 0 {
		t.Fatalf("offered again inside the window: %+v", again)
	}
	h2.now = h2.now.Add(2 * time.Minute)
	again = handOutAs(t, h2, run2, 2, "")
	if len(again) != 2 || again[0].Name != ins[0].Name {
		t.Fatalf("idle hand-outs not offered again under their names: %+v", again)
	}
	// The first offer's child still claims with its own token (B3 keeps
	// the second child out).
	fetchAs(t, h2, run2, ins[0], codexChild("", "k1"))
}

// an earlier revision (e2e-flash code run): a unit the coordinator was offered and never
// started sits handed while later nexts keep returning other offers, so no
// wait ever ends empty (J4). Once a child of a unit handed to the same
// session by a later next fetched its prompt, the coordinator has moved past
// the earlier offer: that unit is offered again with a fresh token and stops
// counting as one of its children starting. A sibling from the same next
// fetching is no such evidence (a serial spawner may still be starting it),
// and no clock is involved.
//
// Calls are ordered by the journal, not the clock: the same holds when the
// engine's clock stands still or runs backward between the calls.
func TestUnstartedHandOutComesBackOnceALaterOffersChildFetched(t *testing.T) {
	for _, step := range []time.Duration{4 * time.Minute, 0, -5 * time.Minute} {
		t.Run(fmt.Sprint(step), func(t *testing.T) { unstartedComesBack(t, step) })
	}
}

func unstartedComesBack(t *testing.T, step time.Duration) {
	d := fanDef("proto-unstarted", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss"}
	first := handOutAs(t, h, run, 2, "boss")
	fetchAs(t, h, run, first[0], codexChild("boss", "k1")) // first[1] is never started
	h.now = h.now.Add(step)                                // 4 minutes: past the old 2-minute spawn grace, inside UnstartedGrace
	second := handOutAs(t, h, run, 1, "boss")
	if len(second) != 1 || second[0].Name == first[1].Name {
		t.Fatalf("a sibling's fetch from the same next re-offered the unstarted unit: %+v", second)
	}
	// The coordinator blocks in next; the later offer's child fetching is
	// what it returns on.
	type result struct {
		res wf.NextResult
		err error
	}
	got := make(chan result, 1)
	go func() {
		res, err := e.NextWait(run, 1, co, 10*time.Second)
		got <- result{res, err}
	}()
	time.Sleep(100 * time.Millisecond)
	fetchAs(t, h, run, second[0], codexChild("boss", "k3"))
	var r result
	select {
	case r = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("a blocking next did not return when a later offer's child fetched")
	}
	again := agentsOf(r.res.Instructions)
	if r.err != nil || r.res.Timeout || len(again) != 1 || again[0].Name != first[1].Name || claimToken(t, again[0]) == claimToken(t, first[1]) {
		t.Fatalf("next after a later offer's fetch = %+v, %v; want %s again with a fresh token", r.res, r.err, first[1].Name)
	}
	if out := wf.RenderNext(r.res, "codex", func(v string, a ...string) string { return v }); !strings.Contains(out, "not started yet: "+first[1].Name) {
		t.Errorf("the re-offer does not say the unit was never started:\n%.400s", out)
	}
	var noted bool
	for _, rec := range h.kind(run, wf.JournalDegraded) {
		var dd wf.DegradedData
		if err := json.Unmarshal(rec.Data, &dd); err == nil && dd.Unit == first[1].Name && dd.Kind == wf.DegradedUnstarted {
			noted = true
		}
	}
	if !noted {
		t.Errorf("the re-offer of %s is not journaled as degraded", first[1].Name)
	}
	// Re-offered once: it is not offered again until a still later offer's
	// child fetches.
	if more := handOutAs(t, h, run, 1, "boss"); len(more) != 0 {
		t.Fatalf("offered again with no new evidence: %+v", more)
	}
	// The first offer's child, late, still claims it; the re-offer's is told
	// the unit is taken.
	fetchAs(t, h, run, first[1], codexChild("boss", "k2"))
	if _, err := e.PromptFetchAs(run, again[0].Name, claimToken(t, again[0]), codexChild("boss", "k2b")); err == nil || !strings.Contains(err.Error(), "another child already owns this unit; exit without working") {
		t.Errorf("the re-offer's child = %v, want told the unit is taken", err)
	}
}

// an earlier revision review: a fetch credits the offer whose token it presented. Call 1
// offers A and B, call 2 offers C and C's child fetches; calls 3 and 4 offer
// A and B again, one each. B's child from call 1 then fetches with its call-1
// token: that is no evidence against A's call-3 offer, so A is not offered
// again.
func TestAFetchCreditsTheOfferWhoseTokenItPresented(t *testing.T) {
	d := fanDef("proto-credit", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first := handOutAs(t, h, run, 2, "boss")
	c := handOutAs(t, h, run, 1, "boss")
	fetchAs(t, h, run, c[0], codexChild("boss", "kc"))
	third := handOutAs(t, h, run, 1, "boss")
	fourth := handOutAs(t, h, run, 1, "boss")
	if len(third) != 1 || len(fourth) != 1 || !third[0].NotStarted || !fourth[0].NotStarted || third[0].Name == fourth[0].Name {
		t.Fatalf("calls 3 and 4 = %+v, %+v; want A and B offered again, one each", third, fourth)
	}
	var late wf.Instruction // the call-1 offer of the unit call 4 offered again
	for _, in := range first {
		if in.Name == fourth[0].Name {
			late = in
		}
	}
	fetchAs(t, h, run, late, codexChild("boss", "kb"))
	if again := handOutAs(t, h, run, 1, "boss"); len(again) != 0 {
		t.Fatalf("a call-1 token's fetch re-offered %s, offered again by call 3: %+v", third[0].Name, again)
	}
}

// an earlier revision review: an offer of units that were never started is sized with
// the line that heads each of them, so it stays within the harness's output
// limit.
func TestNotStartedOffersFitTheOutputLimit(t *testing.T) {
	const units = 30
	items := make([]int, units+1)
	for i := range items {
		items[i] = i + 1
	}
	for _, limit := range []wf.Coordinator{{OutLines: 60}, {OutLines: 120}, {OutLines: 200}, {OutBytes: 6000}, {OutBytes: 12000}, {OutBytes: 20000}} {
		d := fanDef("proto-fit", chItems(items...), units+1, wf.AllSettled())
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		handOutAs(t, h, run, units, "boss")
		later := handOutAs(t, h, run, 1, "boss")
		fetchAs(t, h, run, later[0], codexChild("boss", "k"))
		co := limit
		co.Harness, co.Session = "codex", "boss"
		res, err := h.e.(*wf.Engine).NextWait(run, units, co, time.Second)
		if err != nil || len(agentsOf(res.Instructions)) == 0 || !res.Instructions[0].NotStarted {
			t.Fatalf("limit %+v: %+v, %v; want units offered again as never started", limit, res, err)
		}
		out := wf.RenderNext(res, "codex", func(v string, a ...string) string { return v })
		if lines := strings.Count(out, "\n"); limit.OutLines > 0 && lines > limit.OutLines {
			t.Errorf("limit %d lines: next printed %d lines for %d units", limit.OutLines, lines, len(res.Instructions))
		}
		if limit.OutBytes > 0 && len(out) > limit.OutBytes {
			t.Errorf("limit %d bytes: next printed %d bytes for %d units", limit.OutBytes, len(out), len(res.Instructions))
		}
	}
}

// A4: a no-adapter child's fetch names no harness on the claim, so a manual
// started may still name one; a started before the fetch takes the child's
// identity from its environment at the fetch.
func TestStartedAfterGenericFetchAndBefore(t *testing.T) {
	d := fanDef("proto-started", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAsH(t, h, run, 2, "", "claude")
	e := h.e.(*wf.Engine)
	if _, err := e.PromptFetchAs(run, ins[0].Name, claimToken(t, ins[0]), hn.For("nonesuch").Caller(func(string) string { return "" })); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Started(run, ins[0].Name, wf.Claim{Harness: "codex", Session: "s", Agent: "a"}); err != nil {
		t.Fatalf("started after a generic fetch: %v", err)
	}
	if err := h.e.Started(run, ins[1].Name, wf.Claim{Harness: "claude", Session: "s"}); err != nil {
		t.Fatal(err)
	}
	fetchAs(t, h, run, ins[1], hn.Caller{Harness: "claude", Session: "s", Root: "/root", FromEnv: true})
	var m []wf.MetadataAttachedData
	for _, r := range h.kind(run, wf.JournalMetadataAttached) {
		var d wf.MetadataAttachedData
		_ = json.Unmarshal(r.Data, &d)
		if d.Name == ins[1].Name {
			m = append(m, d)
		}
	}
	if len(m) != 1 || m[0].Root != "/root" || !m[0].Env {
		t.Fatalf("metadata_attached for a started-first unit = %+v, want the child's root", m)
	}
	// The coordinator's own session fetching a unit a started claimed is
	// still refused.
	h2 := newHarness(t, d, nil)
	run2 := h2.run(d, nil)
	got := handOutAs(t, h2, run2, 1, "boss")
	if err := h2.e.Started(run2, got[0].Name, wf.Claim{Harness: "codex", Session: "boss", Agent: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.e.(*wf.Engine).PromptFetchAs(run2, got[0].Name, claimToken(t, got[0]), hn.Caller{Harness: "codex", Session: "boss", Is: hn.IsSelf, FromEnv: true}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("self fetch after started = %v, want a refusal", err)
	}
}

// next --wait with no room left returns when a child finishes and ready
// units wait beyond its n.
func TestNextWaitReturnsWhenASlotFrees(t *testing.T) {
	d := fanDef("proto-freed", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAs(t, h, run, 1, "boss")
	fetchAs(t, h, run, ins[0], codexChild("boss", "k1"))
	e := h.e.(*wf.Engine)
	done := make(chan wf.NextResult, 1)
	go func() {
		// n left out with the harness's limit full: no room until a child ends.
		r, err := e.NextWait(run, wf.NextAuto, wf.Coordinator{Harness: "codex", Session: "boss", Limit: 1}, 5*time.Second)
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := h.e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		// With n left out the freed slot's unit is offered at once.
		if r.Timeout || (!r.Freed && len(agentsOf(r.Instructions)) != 1) {
			t.Fatalf("next = %+v, want the freed slot offered", r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("next --wait did not return when a slot freed")
	}
}

// A3b: a prompt fetched from the coordinator's own session is refused with
// the fix; nothing is claimed.
func TestCoordinatorsOwnClaimIsRefused(t *testing.T) {
	d := clAgentDef("proto-self")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAs(t, h, run, 1, "boss")
	_, err := h.e.(*wf.Engine).PromptFetchAs(run, ins[0].Name, claimToken(t, ins[0]), hn.Caller{Harness: "codex", Session: "boss", Is: hn.IsSelf})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "this is a child's command: start a child agent with the text between the lines") {
		t.Fatalf("self claim = %v", err)
	}
	if n := len(h.kind(run, wf.JournalAttemptStarted)); n != 0 {
		t.Fatalf("a refused self claim journaled %d attempt_started", n)
	}
}

// A1/A2: a Claude child is found by its claim token, lazily: its transcript is
// flushed only after its prompt fetch, and the engine attaches agent and
// transcript at done.
func TestLateFlushedClaudeTranscriptResolvesAtDone(t *testing.T) {
	root := t.TempDir()
	d := clAgentDef("proto-claude")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAsH(t, h, run, 1, "sess", "claude")
	tok := claimToken(t, ins[0])
	fetchAs(t, h, run, ins[0], hn.Caller{Harness: "claude", Session: "sess", Root: root, FromEnv: true})
	if got := attemptStarted(t, h, run); len(got) != 1 || got[0].Session != "sess" || got[0].Agent != "" || got[0].Root != root {
		t.Fatalf("claim = %+v, want the session and root, no agent yet", got)
	}
	sub := filepath.Join(root, "projects", "-repo", "sess", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "agent-abc.jsonl"), []byte("wf prompt --claim "+tok), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var attached []wf.MetadataAttachedData
	for _, r := range h.kind(run, wf.JournalMetadataAttached) {
		var m wf.MetadataAttachedData
		if err := json.Unmarshal(r.Data, &m); err != nil {
			t.Fatal(err)
		}
		attached = append(attached, m)
	}
	if len(attached) != 1 || attached[0].Agent != "abc" || !strings.HasSuffix(attached[0].Transcript, "agent-abc.jsonl") || !attached[0].Resolved {
		t.Fatalf("metadata_attached = %+v", attached)
	}
	if n := len(h.kind(run, wf.JournalDegraded)); n != 0 {
		t.Fatalf("a resolved child is journaled degraded (%d records)", n)
	}
}

// A3b/A3c: an unknown harness runs clean with no identity, and journals no
// degraded record for it.
func TestUnknownHarnessRunsWithNoIdentity(t *testing.T) {
	d := clAgentDef("proto-generic")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: "generic"})
	if err != nil || len(ins) != 1 {
		t.Fatalf("hand-out = %+v, %v", ins, err)
	}
	fetchAs(t, h, run, ins[0], hn.For("generic").Caller(func(string) string { return "" }))
	if _, err := h.e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if ds := h.kind(run, wf.JournalDegraded); len(ds) != 0 {
		t.Fatalf("degraded = %d records, want none", len(ds))
	}
}

// C2d: a stop is the engine's: next offers nothing until resume, and a child
// already running still submits.
func TestStopOffersNothingUntilResume(t *testing.T) {
	d := fanDef("proto-stop", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAs(t, h, run, 1, "boss")
	fetchAs(t, h, run, ins[0], codexChild("boss", "k1"))
	e := h.e.(*wf.Engine)
	if err := e.Stop(run, "budget"); err != nil {
		t.Fatal(err)
	}
	got, err := e.HandOutAs(run, 5, wf.Coordinator{Harness: "codex", Session: "boss"})
	if err != nil || len(got) != 1 || got[0].Kind != wf.InstructionStopped {
		t.Fatalf("next while stopped = %+v, %v", got, err)
	}
	if _, err := h.e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatalf("a running child's done while stopped: %v", err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	if again := handOutAs(t, h, run, 5, "boss"); len(again) != 1 {
		t.Fatalf("after resume %d units, want the one left", len(again))
	}
}

// A3a: the harness is the coordinator's, read on each next: a run created
// under Claude and driven from Codex hands out Codex dispatch.
func TestDispatchFollowsTheCallersHarness(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "claude"})
	run := h.run(d, nil)
	got, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "x"})
	if err != nil || len(got) != 1 {
		t.Fatalf("hand-out = %+v, %v", got, err)
	}
	if dp := got[0].Dispatch; dp.Model != "gpt-6.2-astra" || dp.AgentType != "" {
		t.Fatalf("codex caller got dispatch %+v, want gpt-6.2-astra and no agent type", dp)
	}
	h2 := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "claude"})
	run2 := h2.run(d, nil)
	got, _ = h2.e.(*wf.Engine).HandOutAs(run2, 1, wf.Coordinator{Harness: "hermes"})
	if dp := got[0].Dispatch; dp.Model != "" || dp.Tier != wf.TierStrong {
		t.Fatalf("unknown caller got dispatch %+v, want the tier and no model", dp)
	}
}

// Rule 5 step 2: the run's tokens are what children report with --usage. A
// child that reports nothing counts zero, whatever its transcript records.
func TestUsageCountsOnlyWhatTheChildReports(t *testing.T) {
	for _, c := range []struct {
		name   string
		usage  int
		failed bool
		want   int
	}{
		{name: "done", want: 0},
		{name: "failed", failed: true, want: 0},
		{name: "done --usage", usage: 9, want: 9},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, run, tok, root, unit := usageRun(t, "proto-usage", wf.RunBudget{})
			writeClaudeChild(t, root, tok, `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":5000,"output_tokens":40}}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":3,"cache_read_input_tokens":6000,"output_tokens":7}}}
`)
			var err error
			if c.failed {
				_, err = h.e.FailedAs(run, unit, "gave up", wf.FailTransient)
			} else {
				_, err = h.e.Done(run, unit, wf.Submission{Output: json.RawMessage(`{}`), Tokens: c.usage})
			}
			if err != nil {
				t.Fatal(err)
			}
			rs, err := h.e.Status(run)
			if err != nil {
				t.Fatal(err)
			}
			if rs.Tokens != c.want {
				t.Fatalf("run tokens = %d, want %d", rs.Tokens, c.want)
			}
		})
	}
}

// Rule 5 step 2: a failed attempt's transcript spends none of the budget, so
// its retry goes out even when the transcript records more than the budget.
func TestFailedAttemptTranscriptSpendsNoBudget(t *testing.T) {
	h, run, tok, root, unit := usageRun(t, "proto-usage-budget", wf.RunBudget{Tokens: 100})
	writeClaudeChild(t, root, tok, `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":60}}}
`)
	issued := len(h.kind(run, wf.JournalInstructionIssued))
	if _, err := h.e.FailedAs(run, unit, "gave up", wf.FailTransient); err != nil {
		t.Fatal(err)
	}
	if n := len(h.kind(run, wf.JournalInstructionIssued)); n <= issued {
		t.Fatalf("instructions issued %d -> %d: the transcript's usage held back the retry", issued, n)
	}
}

// usageRun hands out and fetches the one unit of a fresh claude run; it
// returns the unit's claim token, the transcript root and the unit name.
func usageRun(t *testing.T, name string, b wf.RunBudget) (*harness, wf.RunID, string, string, string) {
	t.Helper()
	root := t.TempDir()
	d := clAgentDef(name)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: b})
	run := h.run(d, nil)
	ins := handOutAsH(t, h, run, 1, "sess", "claude")
	tok := claimToken(t, ins[0])
	fetchAs(t, h, run, ins[0], hn.Caller{Harness: "claude", Session: "sess", Root: root, FromEnv: true})
	return h, run, tok, root, ins[0].Name
}

// writeClaudeChild writes a claude subagent transcript under root whose
// prompt names the claim tok, followed by lines.
func writeClaudeChild(t *testing.T, root, tok, lines string) {
	t.Helper()
	sub := filepath.Join(root, "projects", "-repo", "sess", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","message":{"content":"wf prompt --claim ` + tok + `"}}
` + lines
	if err := os.WriteFile(filepath.Join(sub, "agent-abc.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
}

// B4: only a newer session clears an older one's units. An older session
// calling again clears nothing of the newer one's, even one that took no
// units before the newer one appeared; a live coordinator whose Codex
// children claim under their own thread ids (no CODEX_SESSION_ID) keeps them;
// and a coordinator's own died report counts toward the lost cap.
func TestOnlyANewerSessionClearsUnits(t *testing.T) {
	d := fanDef("proto-order", chItems(1, 2), 2, wf.AllSettled())
	newRun := func(t *testing.T) (*harness, wf.RunID) {
		h := newHarness(t, d, nil)
		return h, h.run(d, nil)
	}
	lost := func(h *harness, run wf.RunID) []string {
		var causes []string
		for _, r := range h.kind(run, wf.JournalAttemptLost) {
			var d wf.AttemptLostData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				t.Fatal(err)
			}
			causes = append(causes, d.Cause)
		}
		return causes
	}
	t.Run("older session back", func(t *testing.T) {
		h, run := newRun(t)
		handOutAs(t, h, run, 0, "s1") // s1 first, takes nothing
		for _, in := range handOutAs(t, h, run, 2, "s2") {
			fetchAs(t, h, run, in, codexChild("s2", "kid-"+in.Name))
		}
		h.now = h.now.Add(time.Minute)
		handOutAs(t, h, run, 2, "s1")
		if c := lost(h, run); len(c) != 0 {
			t.Fatalf("an older session cleared a newer one's units: %v", c)
		}
	})
	t.Run("children under their own thread ids", func(t *testing.T) {
		h, run := newRun(t)
		for _, in := range handOutAs(t, h, run, 2, "co") {
			fetchAs(t, h, run, in, codexChild("thread-"+in.Name, "thread-"+in.Name))
		}
		h.now = h.now.Add(time.Minute)
		handOutAs(t, h, run, 2, "co")
		if c := lost(h, run); len(c) != 0 {
			t.Fatalf("a live coordinator lost its own children: %v", c)
		}
	})
	t.Run("own died report", func(t *testing.T) {
		h, run := newRun(t)
		ins := handOutAs(t, h, run, 1, "co")
		fetchAs(t, h, run, ins[0], codexChild("co", "kid"))
		if err := h.e.(*wf.Engine).HeartbeatAs(run, wf.HeartbeatTarget{Name: ins[0].Name}, false, wf.Coordinator{Harness: "codex", Session: "co"}); err != nil {
			t.Fatal(err)
		}
		if c := lost(h, run); len(c) != 1 || c[0] != wf.LostCauseDied {
			t.Fatalf("lost causes = %v, want one died", c)
		}
	})
}

// endedChild hands the one unit of a proto-ended run to session sess, claims
// it as a Claude child whose transcript is the claim line then fixture, and
// returns the run, the unit's instruction and what the next hand-out
// returned, inside the stall threshold.
func endedChild(t *testing.T, h *harness, d *wf.Def, fixture string) (wf.RunID, wf.Instruction, []wf.Instruction) {
	t.Helper()
	root := t.TempDir()
	run := h.run(d, nil)
	ins := handOutAsH(t, h, run, 1, "sess", "claude")
	tok := claimToken(t, ins[0])
	fetchAs(t, h, run, ins[0], hn.Caller{Harness: "claude", Session: "sess", Root: root, FromEnv: true})
	end, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "ended", fixture))
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "projects", "-repo", "sess", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	first := `{"type":"user","message":{"role":"user","content":"wf prompt --claim ` + tok + `"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sub, "agent-abc.jsonl"), append([]byte(first), end...), 0o644); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Second) // well inside the stall threshold
	return run, ins[0], handOutAsH(t, h, run, 1, "sess", "claude")
}

// F1 (replacing E2's re-offer): a claimed child whose transcript ended on its
// end marker with no done raises a recovery offer at once, with the
// transcript, the engine's reading of where it stopped and the commands that
// apply; the attempt stays open. One cut mid-turn is not offered.
func TestEndedChildIsOfferedForRecovery(t *testing.T) {
	d := clAgentDef("proto-ended")
	h := newHarness(t, d, nil)
	h.manualRecovery = true
	run, unit, again := endedChild(t, h, d, "claude-handback.jsonl")
	if len(again) != 1 || again[0].Kind != wf.InstructionRecovery || again[0].Name != unit.Name {
		t.Fatalf("next = %+v, want the recovery offer of %s", again, unit.Name)
	}
	v := again[0].Recovery
	if v == nil || v.Transcript == "" || v.Resume == "" || !strings.Contains(v.Resume, "SendMessage(") || !strings.Contains(v.Resume, "--claim") ||
		v.Commands[wf.RecoveryResume] == "" || v.Commands[wf.RecoveryRestart] == "" || v.Commands[wf.RecoveryGiveUp] == "" {
		t.Fatalf("recovery = %+v, want the transcript, the fixer resume line and three commands", v)
	}
	if n := len(h.kind(run, wf.JournalAttemptLost)); n != 0 {
		t.Fatalf("%d attempt_lost: the attempt must stay open until the answer", n)
	}
	if st := h.status(run); len(st.Recoveries) != 1 {
		t.Fatalf("status recoveries = %+v, want the offer", st.Recoveries)
	}

	t.Run("mid-turn is not offered", func(t *testing.T) {
		h := newHarness(t, d, nil)
		h.manualRecovery = true
		run, _, again := endedChild(t, h, d, "claude-mid-tool-use.jsonl")
		if len(again) != 0 || len(h.kind(run, wf.JournalRecoveryOffered)) != 0 {
			t.Fatalf("a child mid-turn was offered: %+v", again)
		}
	})
}

// F1's three answers: resume keeps the claim and the child's done is taken
// on the same attempt; restart loses it as died and offers attempt 2; give
// up takes the Blocked route. Each is a degraded record of kind recovery.
func TestEndedChildRecoveryChoices(t *testing.T) {
	d := clAgentDef("proto-ended")
	for _, choice := range []string{wf.RecoveryResume, wf.RecoveryRestart, wf.RecoveryGiveUp} {
		t.Run(choice, func(t *testing.T) {
			h := newHarness(t, d, nil)
			h.manualRecovery = true
			run, unit, _ := endedChild(t, h, d, "claude-handback.jsonl")
			if err := h.e.Recover(run, unit.Name, choice); err != nil {
				t.Fatal(err)
			}
			ds := degradedOf(t, h, run)
			if len(ds) != 1 || ds[0].Kind != wf.DegradedRecovery || !strings.HasPrefix(ds[0].Action, choice) && !(choice == wf.RecoveryGiveUp && strings.HasPrefix(ds[0].Action, "give up")) {
				t.Fatalf("degraded = %+v, want the %s recorded", ds, choice)
			}
			switch choice {
			case wf.RecoveryResume:
				if n := len(h.kind(run, wf.JournalAttemptLost)); n != 0 {
					t.Fatalf("resume lost the attempt (%d attempt_lost)", n)
				}
				if res, err := h.e.Done(run, unit.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil || res.Class != wf.Accepted {
					t.Fatalf("the resumed child's done = %+v %v, want accepted on the same attempt", res, err)
				}
				statusOf(t, h.status(run), wf.RunEnded, "clDone")
			case wf.RecoveryRestart:
				again := handOutAsH(t, h, run, 1, "sess", "claude")
				if len(again) != 1 || !strings.HasSuffix(again[0].Name, "#2") {
					t.Fatalf("re-offer = %+v, want attempt 2", again)
				}
				var lost wf.AttemptLostData
				if rs := h.kind(run, wf.JournalAttemptLost); len(rs) != 1 || json.Unmarshal(rs[0].Data, &lost) != nil || lost.Cause != wf.LostCauseDied {
					t.Fatalf("attempt_lost = %+v, want one died", lost)
				}
			case wf.RecoveryGiveUp:
				statusOf(t, h.status(run), wf.RunEnded, "clGaveUp")
			}
		})
	}
}

// A harness that cannot resume a child offers restart and give up only, and
// a resume answer is refused.
func TestEndedChildWithoutResume(t *testing.T) {
	if line := hn.For("generic").ResumeLine("t1", "go on"); line != "" {
		t.Fatalf("generic resume line = %q, want none", line)
	}
	if line := hn.For("claude").ResumeLine("abc", "go on"); !strings.Contains(line, `to="abc"`) {
		t.Fatalf("claude resume line = %q, want SendMessage to the agent", line)
	}
}

// E3: 48 units against a harness limit of 20, every other spawn refused by
// the harness: next --wait with n left out never offers past the limit less
// the coordinator's busy children, a refused spawn comes back at the end of a
// wait with nothing else (J4: "not started yet", no clock on the unit), and
// every unit completes with no list kept by the coordinator.
func TestHarnessLimitOffersAndRefusedSpawnsComeBack(t *testing.T) {
	items := make([]int, 48)
	for i := range items {
		items[i] = i + 1
	}
	d := fanDef("proto-limit", chItems(items...), 48, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss", Limit: 20, LimitSource: "max_threads in test"}
	var running []wf.Instruction
	spawns := 0
	for round := 0; round < 200; round++ {
		res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Instructions) == 1 && res.Instructions[0].Kind == wf.InstructionEnded {
			if done := len(h.kind(run, wf.JournalAccepted)); done < 48 {
				t.Fatalf("the run ended with %d units accepted, want 48", done)
			}
			return
		}
		for _, in := range res.Instructions {
			if in.Kind == wf.InstructionStopped {
				t.Fatalf("round %d told the coordinator to stop: %+v", round, in)
			}
		}
		offered := 0
		for _, in := range res.Instructions {
			if in.Kind != wf.InstructionAgent {
				continue
			}
			offered++
			if spawns++; spawns%2 == 0 {
				continue // the harness refused this spawn: the coordinator just goes on
			}
			fetchAs(t, h, run, in, codexChild(co.Session, "kid-"+in.Name))
			running = append(running, in)
		}
		// A re-list offers the coordinator's own unstarted units again: they
		// are already among its busy ones.
		if offered > 0 && !res.Relisted && (res.Offer == nil || res.Offer.Limit != 20 || offered+res.Offer.Busy > 20) {
			t.Fatalf("round %d offered %d with %+v: past the limit", round, offered, res.Offer)
		}
		// Half the running children finish; then time passes.
		for len(running) > 0 && len(running)%2 == 0 {
			in := running[0]
			running = running[1:]
			if _, err := e.Done(run, in.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
				t.Fatal(err)
			}
		}
		if len(running) > 0 && round%3 == 2 {
			for _, in := range running {
				if _, err := e.Done(run, in.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
					t.Fatal(err)
				}
			}
			running = nil
		}
		h.now = h.now.Add(30 * time.Second)
	}
	t.Fatalf("the run did not end in 200 rounds (%d spawns)", spawns)
}

// an earlier revision: a newer session's next while the previous coordinator's next
// --wait is running (it holds its session's lock) is refused with the fix,
// sweeping nothing; once that coordinator is stopped (wf stop), or its call
// is gone (a crash restart), the newer session's next re-offers its
// children at once.
func TestNewSessionRefusedWhileTheOldCoordinatorIsLive(t *testing.T) {
	for _, c := range []string{"live", "stopped", "crashed"} {
		t.Run(c, func(t *testing.T) {
			runs := t.TempDir()
			d := fanDef("proto-live-coord", chItems(1, 2), 2, wf.AllSettled())
			h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(r wf.RunID) string { return filepath.Join(runs, string(r)) }})
			run := h.run(d, nil)
			e := h.e.(*wf.Engine)
			ins, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "claude", Session: "old"})
			if err != nil {
				t.Fatal(err)
			}
			for _, in := range agentsOf(ins) {
				fetchAs(t, h, run, in, hn.Caller{Harness: "claude", Session: "old", Root: t.TempDir(), FromEnv: true})
			}
			if c != "crashed" {
				// The old coordinator blocks in next --wait: its lock is held.
				lock := filepath.Join(runs, string(run), "coordinators", "old.lock")
				if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
					t.Fatal(err)
				}
				f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString("4242\n"); err != nil {
					t.Fatal(err)
				}
			}
			if c == "stopped" {
				if err := e.Stop(run, "operator"); err != nil {
					t.Fatal(err)
				}
				if err := e.Resume(run); err != nil {
					t.Fatal(err)
				}
			}
			again, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "claude", Session: "new"})
			if c == "live" {
				if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "another coordinator session of this run is live") || strings.Contains(err.Error(), "4242") || strings.Contains(err.Error(), "resume") || strings.Contains(err.Error(), "stop") {
					t.Fatalf("next from a parallel session = %v, want the live-coordinator refusal with its stop and resume path", err)
				}
				if n := len(h.kind(run, wf.JournalAttemptLost)); n != 0 {
					t.Fatalf("a refused next lost %d units", n)
				}
				return
			}
			if c == "stopped" {
				// A stop and resume is a harness-segment boundary: nothing
				// says the old coordinator's children exited, so they are
				// kept, not re-offered, until they go quiet.
				if err != nil || len(agentsOf(again)) != 0 || len(h.kind(run, wf.JournalAttemptLost)) != 0 {
					t.Fatalf("stopped: next = %d units, %v, %d lost; want the old segment's children kept", len(agentsOf(again)), err, len(h.kind(run, wf.JournalAttemptLost)))
				}
				return
			}
			if err != nil || len(agentsOf(again)) != 2 {
				t.Fatalf("%s: next = %d units, %v; want both re-offered at once", c, len(agentsOf(again)), err)
			}
		})
	}
}

// an earlier revision: next --wait holds its session's lock while it blocks and lets go
// when it returns.
func TestNextWaitHoldsTheCoordinatorLock(t *testing.T) {
	runs := t.TempDir()
	d := fanDef("proto-lock", chItems(1), 1, wf.AllSettled())
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(r wf.RunID) string { return filepath.Join(runs, string(r)) }})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "claude", Session: "s"}
	if _, err := e.NextWait(run, 1, co, time.Second); err != nil { // hands out the unit
		t.Fatal(err)
	}
	lock := filepath.Join(runs, string(run), "coordinators", "s.lock")
	held := make(chan bool)
	go func() {
		_, _ = e.NextWait(run, 1, co, 300*time.Millisecond) // nothing for it: blocks
		close(held)
	}()
	time.Sleep(100 * time.Millisecond)
	tryLock := func() bool {
		f, err := os.Open(lock)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil
	}
	if tryLock() {
		t.Fatal("the lock is free while next --wait blocks")
	}
	<-held
	if !tryLock() {
		t.Fatal("the lock is held after next --wait returned")
	}
}

func agentsOf(ins []wf.Instruction) []wf.Instruction {
	var out []wf.Instruction
	for _, in := range ins {
		if in.Kind == wf.InstructionAgent {
			out = append(out, in)
		}
	}
	return out
}

// Done trusts the hand-back: a child whose resolved transcript shows no read
// of its instruction body (it read some other file) has its validate and its
// done accepted. The engine tells the child to read the body and does not
// look in the harness's transcript to check that it did.
func TestDoneAcceptedWithNoTranscriptReadOfTheBody(t *testing.T) {
	for _, h := range []string{"claude", "codex"} {
		t.Run(h, func(t *testing.T) {
			runs, root := t.TempDir(), t.TempDir()
			d := clAgentDef("proto-read-" + h)
			hs := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(r wf.RunID) string { return filepath.Join(runs, string(r)) }})
			run := hs.run(d, nil)
			e := hs.e.(*wf.Engine)
			ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: h, Session: "sess"})
			if err != nil || len(agentsOf(ins)) != 1 {
				t.Fatalf("hand-out %+v, %v", ins, err)
			}
			in := agentsOf(ins)[0]
			tok := claimToken(t, in)
			who := hn.Caller{Harness: "claude", Session: "sess", Root: root, FromEnv: true}
			if h == "codex" {
				who = hn.Caller{Harness: "codex", Session: "sess", Agent: "thr1", Root: root, Is: hn.IsChild, FromEnv: true}
			}
			f, err := e.PromptFetchAs(run, in.Name, tok, who)
			if err != nil {
				t.Fatal(err)
			}
			m := regexp.MustCompile(`(?m)^  (/.+/prompt\.md)$`).FindStringSubmatch(f.Delivered)
			if m == nil {
				t.Fatalf("no body path on the page:\n%s", f.Delivered)
			}
			body := m[1]
			transcript := filepath.Join(root, "projects", "-repo", "sess", "subagents", "agent-abc.jsonl")
			other := "claude-read-other.jsonl"
			if h == "codex" {
				transcript = filepath.Join(root, "sessions", "2026", "11", "15", "rollout-2026-11-15T00-00-00-thr1.jsonl")
				other = "codex-exec-other.jsonl"
			}
			record := func(name string) string {
				raw, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "read", name))
				if err != nil {
					t.Fatal(err)
				}
				return strings.ReplaceAll(string(raw), "/state/runs/r1/prompt/0123456789abcdef/prompt.md", body)
			}
			if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
				t.Fatal(err)
			}
			first := `{"type":"user","message":{"role":"user","content":"wf prompt --claim ` + tok + `"}}` + "\n"
			if err := os.WriteFile(transcript, []byte(first+record(other)), 0o644); err != nil {
				t.Fatal(err)
			}
			if res, err := e.Validate(run, in.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil || res.Class != wf.Accepted {
				t.Fatalf("validate with no read of the body in the transcript = %+v, %v; want accepted", res, err)
			}
			if res, err := e.Done(run, in.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil || res.Class != wf.Accepted {
				t.Fatalf("done with no read of the body in the transcript = %+v, %v; want accepted", res, err)
			}
			for _, r := range hs.records(run) {
				if r.Kind == wf.JournalRefused {
					t.Fatalf("a done with no transcript read journaled a refusal: %s", r.Data)
				}
			}
		})
	}
}

// An answered offer survives a crash: the answer is journaled first, and the
// next drive records and applies it; an offer dies with its unit, so a child
// that fails in the resume window leaves no stale offer.
func TestEndedChildRecoveryAnswerAndUnitEnd(t *testing.T) {
	d := clAgentDef("proto-ended")
	t.Run("answer applied on the next drive", func(t *testing.T) {
		h := newHarness(t, d, nil)
		h.manualRecovery = true
		run, unit, _ := endedChild(t, h, d, "claude-handback.jsonl")
		raw, _ := json.Marshal(wf.RecoveryAnsweredData{Unit: unit.Name, Choice: wf.RecoveryRestart})
		unlock := lock(t, h.j, run)
		recs, _ := h.j.Records(run)
		if err := h.j.Append(wf.JournalRecord{Run: run, Kind: wf.JournalRecoveryAnswered, DefHash: recs[0].DefHash, At: h.now, Data: raw}); err != nil {
			t.Fatal(err)
		}
		unlock()
		again := handOutAsH(t, h, run, 1, "sess", "claude")
		if len(again) != 1 || !strings.HasSuffix(again[0].Name, "#2") {
			t.Fatalf("next after a crashed answer = %+v, want attempt 2", again)
		}
		if ds := degradedOf(t, h, run); len(ds) != 1 || ds[0].Kind != wf.DegradedRecovery {
			t.Fatalf("degraded = %+v, want the restart recorded once", ds)
		}
	})
	t.Run("an offer ends with its unit", func(t *testing.T) {
		h := newHarness(t, d, nil)
		h.manualRecovery = true
		run, unit, _ := endedChild(t, h, d, "claude-handback.jsonl")
		if _, err := h.e.Failed(run, unit.Name, "gave up"); err != nil {
			t.Fatal(err)
		}
		if st := h.status(run); len(st.Recoveries) != 0 {
			t.Fatalf("recoveries = %+v, want none once the unit failed", st.Recoveries)
		}
		if err := h.e.Recover(run, unit.Name, wf.RecoveryGiveUp); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("Recover after the unit ended = %v, want ErrRejected", err)
		}
	})
}

// F1: a resumed child keeps its unit, its claim and its transcript: its done
// is accepted on the same attempt, whether or not its transcript shows a read
// of its body.
func TestResumedChildDoneIsAccepted(t *testing.T) {
	for _, read := range []bool{true, false} {
		t.Run(fmt.Sprintf("read=%v", read), func(t *testing.T) {
			runs, root := t.TempDir(), t.TempDir()
			d := clAgentDef("proto-resume-read")
			h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(r wf.RunID) string { return filepath.Join(runs, string(r)) }})
			h.manualRecovery = true
			run := h.run(d, nil)
			e := h.e.(*wf.Engine)
			ins := handOutAsH(t, h, run, 1, "sess", "claude")
			tok := claimToken(t, ins[0])
			f, err := e.PromptFetchAs(run, ins[0].Name, tok, hn.Caller{Harness: "claude", Session: "sess", Root: root, FromEnv: true})
			if err != nil {
				t.Fatal(err)
			}
			m := regexp.MustCompile(`(?m)^  (/.+/prompt\.md)$`).FindStringSubmatch(f.Delivered)
			if m == nil {
				t.Fatalf("no body path on the page:\n%s", f.Delivered)
			}
			body := m[1]
			fixture := func(dir, name string) string {
				raw, err := os.ReadFile(filepath.Join("..", "harness", "testdata", dir, name))
				if err != nil {
					t.Fatal(err)
				}
				return strings.ReplaceAll(string(raw), "/state/runs/r1/prompt/0123456789abcdef/prompt.md", body)
			}
			life := fixture("read", "claude-read-other.jsonl")
			if read {
				life += fixture("read", "claude-read-tool.jsonl")
			}
			transcript := filepath.Join(root, "projects", "-repo", "sess", "subagents", "agent-abc.jsonl")
			if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
				t.Fatal(err)
			}
			first := `{"type":"user","message":{"role":"user","content":"wf prompt --claim ` + tok + `"}}` + "\n"
			if err := os.WriteFile(transcript, []byte(first+life+fixture("ended", "claude-handback.jsonl")), 0o644); err != nil {
				t.Fatal(err)
			}
			h.now = h.now.Add(10 * time.Second)
			offer := handOutAsH(t, h, run, 1, "sess", "claude")
			if len(offer) != 1 || offer[0].Kind != wf.InstructionRecovery || offer[0].Recovery.Resume == "" {
				t.Fatalf("next = %+v, want the recovery offer with resume", offer)
			}
			if err := h.e.Recover(run, ins[0].Name, wf.RecoveryResume); err != nil {
				t.Fatal(err)
			}
			res, err := e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
			if err != nil || res.Class != wf.Accepted {
				t.Fatalf("resumed done = %+v, %v; want accepted", res, err)
			}
		})
	}
}

// R6: a Hermes child is the engine's to find: its delegation log's goal line
// names its claim token, so done attaches its agent and transcript. What the
// log shows of the child's reads (the body's full path, or read_file's
// base-name preview) changes nothing: the done is accepted and nothing is
// journaled about the read. A log that ends on its final line with
// no done is offered for recovery (F1): restart or give up, no resume, and a
// restart offers attempt 2.
func TestHermesChildIsResolvedFromItsDelegationLog(t *testing.T) {
	for _, c := range []string{"done", "base-name read", "ended"} {
		t.Run(c, func(t *testing.T) {
			runs, root := t.TempDir(), t.TempDir()
			d := clAgentDef("proto-hermes-" + c)
			h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(r wf.RunID) string { return filepath.Join(runs, string(r)) }})
			run := h.run(d, nil)
			e := h.e.(*wf.Engine)
			ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "coord"})
			if err != nil || len(agentsOf(ins)) != 1 {
				t.Fatalf("hand-out %+v, %v", ins, err)
			}
			in := agentsOf(ins)[0]
			tok := claimToken(t, in)
			who := hn.For("hermes").Caller(func(k string) string {
				// A delegate_task child's subprocesses carry Hermes's marker.
				return map[string]string{"HERMES_SESSION_ID": "child-sess", "HERMES_HOME": root, "HERMES_DELEGATED_CHILD_CONTEXT": root}[k]
			})
			if _, err := e.PromptFetchAs(run, in.Name, tok, who); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(root, "cache", "delegation", "live", "deleg_abc", "task-0.log")
			if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
				t.Fatal(err)
			}
			body := "=== Hermes subagent live transcript ===\ngoal: Run: super wf prompt --claim " + tok + " then do what it prints\n" +
				"01:00:28 tool     | -> terminal(super wf prompt --claim " + tok + ")\n" +
				"01:00:30 tool     | -> read_file(prompt.md)\n"
			if c == "done" {
				pf := h.kind(run, wf.JournalPromptFetched)
				var fd wf.PromptFetchedData
				if len(pf) != 1 || json.Unmarshal(pf[0].Data, &fd) != nil || fd.PromptDir == "" {
					t.Fatalf("prompt_fetched %+v", fd)
				}
				body += "01:00:31 tool     | -> terminal(cat " + filepath.Join(fd.PromptDir, "prompt.md") + ")\n"
			}
			if c == "ended" {
				body += "01:01:33 final    | end status=completed exit_reason=completed\n"
			}
			if err := os.WriteFile(log, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if c == "ended" {
				h.now = h.now.Add(10 * time.Second)
				offer, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "coord"})
				if err != nil || len(offer) != 1 || offer[0].Kind != wf.InstructionRecovery || offer[0].Recovery.Commands[wf.RecoveryResume] != "" {
					t.Fatalf("after the log ended without done: %+v, %v; want a recovery offer with no resume", offer, err)
				}
				if err := e.Recover(run, in.Name, wf.RecoveryRestart); err != nil {
					t.Fatal(err)
				}
				again, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "coord"})
				if err != nil || len(agentsOf(again)) != 1 || !strings.HasSuffix(agentsOf(again)[0].Name, "#2") {
					t.Fatalf("after the restart: %+v, %v; want attempt 2", again, err)
				}
				return
			}
			if res, err := e.Done(run, in.Name, wf.Submission{Output: json.RawMessage(`{}`)}); err != nil || res.Class != wf.Accepted {
				t.Fatalf("done = %+v, %v", res, err)
			}
			var m wf.MetadataAttachedData
			if rs := h.kind(run, wf.JournalMetadataAttached); len(rs) != 1 || json.Unmarshal(rs[0].Data, &m) != nil || m.Agent != "deleg_abc/task-0" || m.Transcript != log {
				t.Fatalf("metadata_attached = %+v", m)
			}
			if n := len(h.kind(run, wf.JournalDegraded)); n != 0 {
				t.Fatalf("%s: %d degraded records, want 0", c, n)
			}
		})
	}
}

// Codex cross-review, crash window 1: an ended child's answer stays pending
// until its action is durable. The process dies right after the degraded
// record (before attempt_lost) or right after the answer (before the degraded
// record); a fresh engine finishes the restart with one degraded record and
// offers attempt 2.
func TestEndedChildAnswerSurvivesACrashAtEveryRecord(t *testing.T) {
	d := clAgentDef("proto-ended")
	for _, at := range []wf.JournalKind{wf.JournalDegraded, wf.JournalAttemptLost} {
		t.Run(at.String(), func(t *testing.T) {
			h := newHarness(t, d, nil)
			h.manualRecovery = true
			run, unit, _ := endedChild(t, h, d, "claude-handback.jsonl")
			cj := &crashJournal{Journal: h.j, kind: at, armed: true}
			crashed := wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
			if err := crashed.Recover(run, unit.Name, wf.RecoveryRestart); !errors.Is(err, errCrash) {
				t.Fatalf("Recover = %v, want the crash at %s", err, at)
			}
			h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
			again := handOutAsH(t, h, run, 1, "sess", "claude")
			if len(again) != 1 || again[0].Kind != wf.InstructionAgent || !strings.HasSuffix(again[0].Name, "#2") {
				t.Fatalf("after the crash next = %+v, want attempt 2 of the unit", again)
			}
			if ds := degradedOf(t, h, run); len(ds) != 1 || ds[0].Kind != wf.DegradedRecovery {
				t.Fatalf("degraded = %+v, want the restart recorded once", ds)
			}
		})
	}
}

// The crash-point sweep of every recovery path: for each answer, the process
// dies on the append of each journal kind in turn (the record not written,
// so after the one before it); a fresh engine must carry the run on. The run
// then has something to do (an agent unit, a recovery offer) or has ended,
// and a resumed child's done is accepted.
func TestRecoveryCrashPointSweep(t *testing.T) {
	type path struct {
		name    string
		lostCap int
		ended   bool
		choice  string
	}
	paths := []path{
		{"ended/restart", 0, true, wf.RecoveryRestart},
		{"ended/give-up", 0, true, wf.RecoveryGiveUp},
		{"ended/resume", 0, true, wf.RecoveryResume},
		{"ended/restart at the lost cap", 1, true, wf.RecoveryRestart},
		{"exhausted/restart", 0, false, wf.RecoveryRestart},
		{"exhausted/give-up", 0, false, wf.RecoveryGiveUp},
	}
	var kinds []wf.JournalKind
	for k := range wf.JournalPayloads {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	for _, p := range paths {
		for _, k := range kinds {
			t.Run(p.name+"/"+k.String(), func(t *testing.T) {
				opts := wf.EngineOptions{LostCap: p.lostCap}
				var h *harness
				var run wf.RunID
				var unit string
				var d *wf.Def
				if p.ended {
					d = clAgentDef("proto-ended")
					h = newHarnessOpts(t, d, nil, opts)
					h.manualRecovery = true
					var in wf.Instruction
					run, in, _ = endedChild(t, h, d, "claude-handback.jsonl")
					unit = in.Name
				} else {
					d = exDef("ex-sweep", false)
					h = newHarnessOpts(t, d, nil, opts)
					h.manualRecovery = true
					run = exRun(t, h, d, true)
					failWorkOnce(t, h, run, "a")
					failWorkOnce(t, h, run, "b")
					unit = recoveryOffers(t, h, run)[0].Unit
				}
				fresh := func(j wf.Journal) wf.Executor {
					o := opts
					o.Defs, o.Clock = []*wf.Def{d}, func() time.Time { return h.now }
					return wf.NewEngine(j, h.reg, o)
				}
				cj := &crashJournal{Journal: h.j, kind: k, armed: true}
				if err := fresh(cj).Recover(run, unit, p.choice); err != nil && !errors.Is(err, errCrash) {
					t.Fatalf("Recover: %v", err)
				}
				h.e = fresh(h.j)
				if p.choice == wf.RecoveryResume {
					if res, err := h.e.Done(run, unit, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil || res.Class != wf.Accepted {
						t.Fatalf("the resumed child's done after a crash at %s = %+v, %v; want accepted", k, res, err)
					}
					return
				}
				ins, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sess"})
				if err != nil {
					t.Fatal(err)
				}
				st, err := h.e.Status(run)
				if err != nil {
					t.Fatal(err)
				}
				if st.Phase != wf.RunEnded && len(ins) == 0 {
					t.Fatalf("after a crash at %s the run is stuck: nothing to hand out, phase %s, instances %+v", k, st.Phase, st.Instances)
				}
				n := 0
				for _, dd := range degradedOf(t, h, run) {
					if dd.Kind == wf.DegradedRecovery {
						n++
					}
				}
				if n > 1 {
					t.Fatalf("after a crash at %s: %d recovery records for one answer, want at most 1", k, n)
				}
			})
		}
	}
}

// A claim needs its token and nothing from the harness's records: a Claude
// Code fetch with no subagent transcript naming the token is served at once
// (no look loop), and a never-offered token is refused. A Hermes
// coordinator's own session (no delegated-child marker) is refused as B1
// refuses any coordinator's own first fetch (I2).
func TestAClaimNeedsItsTokenNotARecord(t *testing.T) {
	runs, root := t.TempDir(), t.TempDir()
	d := clAgentDef("claim-needs-child")
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(r wf.RunID) string { return filepath.Join(runs, string(r)) }})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sess", Root: root})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	in := agentsOf(ins)[0]
	tok := claimToken(t, in)
	project := filepath.Join(root, "projects", "-repo")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	who := hn.For("claude").Caller(func(k string) string {
		return map[string]string{"CLAUDE_CODE_SESSION_ID": "sess", "CLAUDE_CONFIG_DIR": root}[k]
	})
	// No record of the session at all: nothing to see, so nothing refused.
	if !hn.For("claude").Caller(func(string) string { return "" }).FromEnv {
		t.Fatal("caller not from env")
	}
	// A never-offered token is refused for that.
	if err := os.WriteFile(filepath.Join(project, "sess.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PromptFetchAs(run, in.Name, "bogus", who); err == nil || !strings.Contains(err.Error(), "use the command in your task text") {
		t.Errorf("a bogus token = %v", err)
	}
	// The session's records show no subagent (its own transcript, no
	// subagents/ yet): the fetch is served on its token, with no wait for a
	// record to appear.
	start := time.Now()
	if _, err := e.PromptFetchAs(run, in.Name, tok, who); err != nil || time.Since(start) > time.Second {
		t.Fatalf("a fetch with no child record = %v after %s, want served at once", err, time.Since(start))
	}
	if got := h.kind(run, wf.JournalDegraded); len(got) != 0 {
		t.Fatalf("the fetch journaled %+v", got)
	}
	d2 := clAgentDef("hermes-own-claim")
	h2 := newHarnessOpts(t, d2, nil, wf.EngineOptions{RunDir: func(r wf.RunID) string { return filepath.Join(runs, string(r)) }})
	run2 := h2.run(d2, nil)
	e2 := h2.e.(*wf.Engine)
	ins2, err := e2.HandOutAs(run2, 1, wf.Coordinator{Harness: "hermes", Session: "local-coord"})
	if err != nil || len(agentsOf(ins2)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins2, err)
	}
	coord := hn.For("hermes").Caller(func(k string) string { return map[string]string{"HERMES_SESSION_ID": "local-coord"}[k] })
	if _, err := e2.PromptFetchAs(run2, agentsOf(ins2)[0].Name, claimToken(t, agentsOf(ins2)[0]), coord); err == nil || !strings.Contains(err.Error(), "this is a child's command") {
		t.Errorf("a Hermes coordinator's own fetch = %v, want refused", err)
	}
}

// Rung 4b G7: a child of the run (here Hermes's delegated-child marker)
// running a coordinator verb is refused, never recorded as a coordinator;
// its own verbs still work.
func TestAChildNeverDrivesTheRun(t *testing.T) {
	def := clAgentDef("child-verbs")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1", "--harness", "claude")
	mustSingle(t, def, "wf", "next", "1", "--state-dir", state)
	t.Setenv("CLAUDECODE", "")
	t.Setenv("HERMES_SESSION_ID", "20261115_110611_df5a85")
	t.Setenv("HERMES_DELEGATED_CHILD_CONTEXT", "/example/u/.hermes")
	ws := t.TempDir()
	for _, args := range [][]string{{"wf", "next", "1"}, {"wf", "recover", "x#1", "restart"}, {"wf", "stop"}, {"next"},
		{"run", "--run-id", "r1", "--workspace", ws}, {"run", "--run-id", "fresh", "--workspace", ws}} {
		code, _, er := singleCLI(def, append(args, "--state-dir", state)...)
		if code != 4 || !strings.Contains(er, "you are a child of this run: finish your unit with validate and done; the coordinator handles the rest") {
			t.Errorf("%v from a child: exit %d, %q", args, code, er)
		}
	}
	if out := mustSingle(t, def, "wf", "journal", "--state-dir", state); strings.Contains(out, "20261115_110611_df5a85") {
		t.Errorf("a child's calls were journaled:\n%s", out)
	}
	if code, _, er := singleCLI(def, "wf", "status", "--state-dir", state); code != 0 {
		t.Errorf("status from a child: exit %d, %s", code, er)
	}
	// A Codex child nested in a Hermes terminal, and a Hermes delegate that
	// inherited a Codex coordinator's variables, are children too.
	for _, env := range []map[string]string{
		{"HERMES_SESSION_ID": "outer", "CODEX_SESSION_ID": "coord", "CODEX_THREAD_ID": "kid"},
		{"HERMES_SESSION_ID": "kid", "HERMES_DELEGATED_CHILD_CONTEXT": "/p", "CODEX_SESSION_ID": "outer", "CODEX_THREAD_ID": "outer"},
	} {
		for _, k := range []string{"HERMES_SESSION_ID", "HERMES_DELEGATED_CHILD_CONTEXT", "CODEX_SESSION_ID", "CODEX_THREAD_ID"} {
			t.Setenv(k, env[k])
		}
		if code, _, er := singleCLI(def, "wf", "next", "1", "--state-dir", state); code != 4 || !strings.Contains(er, "you are a child of this run") {
			t.Errorf("next from a mixed-environment child %v: exit %d, %q", env, code, er)
		}
	}
	// A Codex child (its thread is not the session) is refused the same.
	t.Setenv("HERMES_SESSION_ID", "")
	t.Setenv("HERMES_DELEGATED_CHILD_CONTEXT", "")
	t.Setenv("CODEX_SESSION_ID", "coord-thread")
	t.Setenv("CODEX_THREAD_ID", "child-thread")
	for _, args := range [][]string{{"wf", "heartbeat", "active", "x"}, {"wf", "next", "1"}} {
		if code, _, er := singleCLI(def, append(args, "--state-dir", state)...); code != 4 || !strings.Contains(er, "you are a child of this run") {
			t.Errorf("%v from a Codex child: exit %d, %q", args, code, er)
		}
	}
}

// Plan section I's test: the offer text has no line that begins with a
// command, on every harness, and a claim from the coordinator's own session
// is refused on Codex and Hermes with the fix (I2). Claude Code's environment
// does not tell a coordinator from its subagent, so there the claim token
// alone decides.
func TestTheOfferIsNotACommandForTheCoordinator(t *testing.T) {
	bin := "/example/u/project/.superpowers/bin/super"
	l := func(verb string, args ...string) string { return bin + " wf " + verb + " " + strings.Join(args, " ") }
	unit := wf.Instruction{Kind: wf.InstructionAgent, Name: "Design#1",
		Brief:    "You are a child agent for one unit of work. Run: " + bin + " wf prompt --claim t --state-dir /s\nThen do what that prints.",
		Dispatch: wf.DispatchHints{Model: "opus", Effort: "high", AgentType: "general-purpose"}}
	for _, h := range []string{"claude", "codex", "hermes", "generic"} {
		out := wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{unit, unit}}, h, l)
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) > 0 && (strings.HasPrefix(f[0], "/") || f[0] == "wf" || f[0] == "workflow" || f[0] == "super" || strings.HasPrefix(f[0], "run:")) {
				t.Errorf("%s: a line begins with a command: %q", h, line)
			}
		}
		if strings.Count(out, "the unit is a child's to run") != 2 || strings.Count(out, "\n-----\n") < 2 || strings.Count(out, "<the text between the lines>") != 2 {
			t.Errorf("%s offer:\n%s", h, out)
		}
		// With no hints at all the call is still well formed.
		bare := unit
		bare.Dispatch = wf.DispatchHints{}
		if got := wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{bare}}, h, l); strings.Contains(got, "(,") || strings.Contains(got, "()") {
			t.Errorf("%s offer without hints is malformed:\n%s", h, got)
		}
	}

	runs := t.TempDir()
	for _, c := range []struct {
		harness string
		env     map[string]string
	}{
		{"codex", map[string]string{"CODEX_THREAD_ID": "boss", "CODEX_SESSION_ID": "boss"}},
		{"hermes", map[string]string{"HERMES_SESSION_ID": "boss"}},
	} {
		d := clAgentDef("own-claim-" + c.harness)
		h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(r wf.RunID) string { return filepath.Join(runs, string(r)) }})
		run := h.run(d, nil)
		e := h.e.(*wf.Engine)
		root := t.TempDir()
		ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: c.harness, Session: "boss", Root: root})
		if err != nil || len(agentsOf(ins)) != 1 {
			t.Fatalf("hand-out %+v, %v", ins, err)
		}
		_ = os.MkdirAll(filepath.Join(root, "projects", "-p"), 0o755)
		_ = os.WriteFile(filepath.Join(root, "projects", "-p", "boss.jsonl"), []byte("{}\n"), 0o644)
		env := c.env
		env["CLAUDE_CONFIG_DIR"], env["CODEX_HOME"], env["HERMES_HOME"] = root, root, root
		who := hn.For(c.harness).Caller(func(k string) string { return env[k] })
		in := agentsOf(ins)[0]
		if _, err := e.PromptFetchAs(run, in.Name, claimToken(t, in), who); err == nil || !strings.Contains(err.Error(), "this is a child's command: start a child agent with the text between the lines") {
			t.Errorf("%s: the coordinator's own claim = %v, want refused with the fix", c.harness, err)
		}
	}
}

// next 0 asks for no units, so it returns at once with nothing to wait for
// (main's ask, after H made every next block).
func TestNextZeroReturnsAtOnce(t *testing.T) {
	d := fanDef("next-zero", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	start := time.Now()
	r, err := h.e.(*wf.Engine).NextWait(run, 0, wf.Coordinator{Harness: "claude", Session: "s"}, 5*time.Second)
	if err != nil || r.Timeout || len(agentsOf(r.Instructions)) != 0 || time.Since(start) > time.Second {
		t.Errorf("next 0 = %+v, %v after %s; want nothing, at once", r, err, time.Since(start))
	}
}

// G9 (rung 4b run 4): from the coordinator's own session the engine accepts
// only coordinator verbs: a first prompt fetch is refused with the I2 text,
// and validate, done and failed by name need the unit's claim token. After
// wf stop a child already running still finishes, and nothing new is claimed.
func TestUnitVerbsAreTheChildsOnly(t *testing.T) {
	strictCallers(t)
	def := fanDef("unit-verbs", chItems(1, 2), 2, wf.AllSettled())
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss") // the coordinator's own shell
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "2", "--json", "--state-dir", state)), &ins); err != nil || len(agentsOf(ins)) != 2 {
		t.Fatalf("next: %v %v", ins, err)
	}
	a, b := agentsOf(ins)[0], agentsOf(ins)[1]
	for _, args := range [][]string{
		{"wf", "prompt", "--claim", claimToken(t, a)},
		{"wf", "validate", a.Name, "--output", "/dev/null"},
		{"wf", "done", a.Name, "--output", "/dev/null"},
		{"wf", "failed", a.Name, "--reason", "x"},
	} {
		// I2 and the missing claim token: the refusal says what to do and
		// never what was checked.
		code, _, er := singleCLI(def, append(args, "--state-dir", state)...)
		if code != 4 || !strings.Contains(er, "Unless it asks for a person, you may run next again.") || strings.Contains(er, "stop and report") ||
			strings.Contains(er, "end only your child task") || strings.Contains(er, "session") || strings.Contains(er, "CODEX") {
			t.Errorf("%v from the coordinator: exit %d, %q", args, code, er)
		}
	}
	t.Setenv("CODEX_SESSION_ID", "boss")
	// A real child of unit a, spawned by the coordinator, claims it; then
	// the run is stopped.
	writeCodexChild(t, os.Getenv("CODEX_HOME"), "boss", "kid-a", claimToken(t, a))
	writeCodexChild(t, os.Getenv("CODEX_HOME"), "boss", "kid-b", claimToken(t, b))
	t.Setenv("CODEX_THREAD_ID", "kid-a")
	mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, a), "--state-dir", state)
	t.Setenv("CODEX_THREAD_ID", "kid-b")
	// Another live unit's token names no claim on this one: refused, as a
	// child-local refusal the coordinator goes on from.
	if code, _, er := singleCLI(def, "wf", "prompt", a.Name, "--claim", claimToken(t, b), "--state-dir", state); code != 4 ||
		!strings.Contains(er, "the claim belongs to another live unit") || !strings.Contains(er, "continues with next") || strings.Contains(er, "Human stop required") {
		t.Errorf("foreign live claim: exit %d, %q", code, er)
	}
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	t.Setenv("CLAUDECODE", "") // a person's shell stops it
	mustSingle(t, def, "wf", "stop", "--reason", "operator", "--state-dir", state)
	t.Setenv("CODEX_SESSION_ID", "boss")
	// Nothing new is claimed: b's child, spawned too late, is refused.
	t.Setenv("CODEX_THREAD_ID", "kid-b")
	if code, _, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, b), "--state-dir", state); code != 4 || !strings.Contains(er, "is stopped: no new unit starts") {
		t.Errorf("a new claim after stop: exit %d, %q", code, er)
	}
	// started on a handed unit is a claim too: refused after stop.
	if code, _, er := singleCLI(def, "wf", "started", b.Name, "--session", "boss", "--agent", "kid-b", "--state-dir", state); code != 4 || !strings.Contains(er, "start a child agent") {
		t.Errorf("started after stop: exit %d, %q", code, er)
	}
	// a's running child still fetches again with its claim and finishes.
	t.Setenv("CODEX_THREAD_ID", "kid-a")
	if code, _, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, a), "--state-dir", state); code != 0 {
		t.Errorf("the running child's refetch by claim after stop: exit %d, %s", code, er)
	}
	out := writeAllocated(t, a.Output, `{"OK":true}`)
	if code, _, er := singleCLI(def, "wf", "done", "--claim", claimToken(t, a), "--output", out, "--state-dir", state); code != 0 {
		t.Errorf("the running child's done after stop: exit %d, %s", code, er)
	}
	// A person resumes the run: b's offer from before the stop expired at
	// that boundary, the coordinator's next offers b again, and a
	// child of that offer claims it.
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	mustSingle(t, def, "wf", "resume", "--state-dir", state, "--operator", "op", "--reason", "r")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "kid-b")
	if code, _, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, b), "--state-dir", state); code != 4 || !strings.Contains(er, "this offer expired") {
		t.Errorf("the pre-stop offer's claim after resume: exit %d, %q", code, er)
	}
	t.Setenv("CODEX_THREAD_ID", "boss")
	var again []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state)), &again); err != nil || len(agentsOf(again)) != 1 || agentsOf(again)[0].Name != b.Name {
		t.Fatalf("the coordinator's next after resume = %v, %v; want %s again", again, err, b.Name)
	}
	b = agentsOf(again)[0]
	writeCodexChild(t, os.Getenv("CODEX_HOME"), "boss", "kid-b2", claimToken(t, b))
	t.Setenv("CODEX_THREAD_ID", "kid-b2")
	if code, _, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, b), "--state-dir", state); code != 0 {
		t.Errorf("a claim of the re-offer after resume: exit %d, %s", code, er)
	}
	// A Hermes coordinator's own session (no delegated-child marker) is
	// refused a unit's verbs too.
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	t.Setenv("HERMES_SESSION_ID", "local-coord")
	// J1: the run is driven from Codex; a call that resolves to another
	// harness is refused.
	// A child's verbs are the engine's to refuse: without the
	// unit's claim, and from another harness than its hand-out's, it is
	// refused, naming no harness.
	if code, _, er := singleCLI(def, "wf", "failed", b.Name, "--reason", "x", "--state-dir", state); code != 4 || strings.Contains(strings.ToLower(er), "codex") || strings.Contains(strings.ToLower(er), "hermes") {
		t.Errorf("failed from a Hermes shell: exit %d, %q", code, er)
	}
}

// writeCodexChild writes a Codex child rollout under home: spawned by the
// coordinator thread parent, its spawn message naming token (J2).
func writeCodexChild(t *testing.T, home, parent, thread, token string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "11", "15")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session_meta","payload":{"id":"` + thread + `","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `"}}}}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"You are a child agent for one unit of work. Run: wf prompt --claim ` + token + `"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-15T00-00-00-"+thread+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A caller holding a unit's claim token from another harness than the unit's
// hand-out, or as the child of another coordinator session, is served: the
// token binds the call. The journal notes each mismatch once and holds no
// refusal, and no call ends on a stop for a person.
func TestACallerFromAnotherHarnessOrCoordinatorIsNotedNotRefused(t *testing.T) {
	strictCallers(t)
	def := fanDef("caller-notes", chItems(1, 2), 2, wf.AllSettled())
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "2", "--json", "--state-dir", state)), &ins); err != nil || len(agentsOf(ins)) != 2 {
		t.Fatalf("next: %v %v", ins, err)
	}
	a, b := agentsOf(ins)[0], agentsOf(ins)[1]
	served := func(what string, args ...string) {
		t.Helper()
		code, _, er := singleCLI(def, append(args, "--state-dir", state)...)
		if code != 0 || strings.Contains(er, "Human stop required") {
			t.Fatalf("%s: exit %d, %q; want served", what, code, er)
		}
	}
	// Unit a from a Claude Code shell, unit b from a child of another Codex
	// coordinator session.
	t.Setenv("CODEX_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "1")
	served("prompt from another harness", "wf", "prompt", "--claim", claimToken(t, a))
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "wrong-parent")
	t.Setenv("CODEX_THREAD_ID", "foreign-child")
	served("prompt from another coordinator's child", "wf", "prompt", "--claim", claimToken(t, b))
	out := writeAllocated(t, b.Output, `{"OK":true}`)
	served("validate from another coordinator's child", "wf", "validate", "--claim", claimToken(t, b), "--output", out)
	served("done from another coordinator's child", "wf", "done", "--claim", claimToken(t, b), "--output", out)
	journal := mustSingle(t, def, "wf", "journal", "--state-dir", state)
	if strings.Contains(journal, `"child refused"`) {
		t.Errorf("the journal holds a refusal:\n%s", journal)
	}
	for _, want := range []string{"is not the one its unit was handed out in", "the caller is a child of coordinator session wrong-parent"} {
		if strings.Count(journal, want) != 1 {
			t.Errorf("the journal notes %q %d times, want once:\n%s", want, strings.Count(journal, want), journal)
		}
	}
}

// Plan section J's test: a coordinator script that strips its environment
// and calls a child verb by name, with no claim token, is refused on Claude,
// Codex and Hermes, from its own shell and stripped alike, and the refusal
// never names what was checked (J3). With the token the stripped call is
// served: the token is what admits a child verb.
func TestChildVerbsByNameNeedTheClaimToken(t *testing.T) {
	strictCallers(t)
	for _, c := range []struct {
		harness string
		env     map[string]string
	}{
		{"claude", map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "boss"}},
		{"codex", map[string]string{"CODEX_THREAD_ID": "boss", "CODEX_SESSION_ID": "boss"}},
		{"hermes", map[string]string{"HERMES_SESSION_ID": "boss"}},
	} {
		t.Run(c.harness, func(t *testing.T) {
			def := fanDef("stripped-"+c.harness, chItems(1), 1, wf.AllSettled())
			set := func(env map[string]string) {
				for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID", "HERMES_DELEGATED_CHILD_CONTEXT"} {
					t.Setenv(k, env[k])
				}
			}
			set(c.env)
			home := t.TempDir()
			t.Setenv("HERMES_HOME", home)
			_ = os.MkdirAll(filepath.Join(home, "cache", "delegation", "live"), 0o755)
			state := filepath.Join(t.TempDir(), "state")
			mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
			var ins []wf.Instruction
			if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil || len(agentsOf(ins)) != 1 {
				t.Fatalf("next: %v %v", ins, err)
			}
			u := agentsOf(ins)[0]
			out := writeAllocated(t, u.Output, `{"OK":true}`)
			// The unit is claimed with its token, from a stripped shell.
			set(nil)
			mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, u), "--state-dir", state)
			before := mustSingle(t, def, "wf", "journal", "--state-dir", state)
			for _, env := range []map[string]string{c.env, {}} { // its own shell, then stripped
				set(env)
				for _, args := range [][]string{
					{"wf", "validate", u.Name, "--output", out},
					{"wf", "done", u.Name, "--output", out},
					{"wf", "failed", u.Name, "--reason", "x"},
				} {
					code, _, er := singleCLI(def, append(args, "--state-dir", state)...)
					if code != 4 || !strings.Contains(er, "--claim") {
						t.Errorf("%v with %v: exit %d, %q; want refused for its missing claim token", args, env, code, er)
					}
					for _, named := range []string{"session", "CODEX_", "HERMES_", "CLAUDE"} {
						if strings.Contains(er, named) {
							t.Errorf("%v: the refusal names what was checked (%s): %q", args, named, er)
						}
					}
				}
			}
			set(nil)
			if after := mustSingle(t, def, "wf", "journal", "--state-dir", state); after != before {
				t.Errorf("the refused verbs appended to the journal:\n%s", strings.TrimPrefix(after, before))
			}
			mustSingle(t, def, "wf", "done", "--claim", claimToken(t, u), "--output", out, "--state-dir", state)
		})
	}
}

// J1 (review): a run started from a known harness is driven from it even
// before a coordinator names a session, so a first next with the
// environment stripped is refused. The records root of the first
// coordinator call is not pinned: a later next of the same harness under
// another root (a session moved to another profile) is served, and the
// journal notes the change once.
func TestTheRunsHarnessIsPinnedAndAChangedRootIsNoted(t *testing.T) {
	strictCallers(t)
	def := fanDef("pinned", chItems(1, 2, 3), 3, wf.AllSettled())
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	t.Setenv("CODEX_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	if code, _, er := singleCLI(def, "wf", "next", "1", "--json", "--state-dir", state); code != 4 || !refusedAsMismatch(er) {
		t.Errorf("a stripped first next: exit %d, %q", code, er)
	}
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss")
	mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state)
	t.Setenv("CODEX_HOME", t.TempDir()) // the session moved to another records root
	for i := 0; i < 2; i++ {
		if code, _, er := singleCLI(def, "wf", "next", "1", "--json", "--state-dir", state); code != 0 {
			t.Fatalf("next %d under another records root: exit %d, %q", i+1, code, er)
		}
	}
	if n := strings.Count(mustSingle(t, def, "wf", "journal", "--state-dir", state), `"Kind":"`+string(wf.DegradedRootChanged)+`"`); n != 1 {
		t.Errorf("the journal notes the changed root %d times, want once", n)
	}
}

// strictCallers turns off the suite's TrustSessionlessCallers for one test.
func strictCallers(t *testing.T) {
	t.Helper()
	wf.TrustSessionlessCallers = false
	t.Cleanup(func() { wf.TrustSessionlessCallers = true })
}

// Cross-review on J: a session-less first coordinator (CLAUDECODE alone)
// does not drive the run (J1). A run started with --harness from a plain
// shell hands out under no session; its units are claimed by their token.
func TestASessionlessCoordinatorIsRefused(t *testing.T) {
	strictCallers(t)
	def := fanDef("sessionless", chItems(1), 1, wf.AllSettled())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	if code, _, er := singleCLI(def, "wf", "next", "1", "--json", "--state-dir", state); code != 4 || !refusedAsMismatch(er) {
		t.Fatalf("a session-less next: exit %d, %q", code, er)
	}
	// Started with --harness from a plain shell, the first coordinator still
	// hands out under no session: the unit's token claims it.
	state2 := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state2, "--workspace", t.TempDir(), "--run-id", "r2", "--harness", "claude")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state2)), &ins); err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("next: %v %v", ins, err)
	}
	if code, _, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, agentsOf(ins)[0]), "--state-dir", state2); code != 0 {
		t.Errorf("a claim with no child record: exit %d, %q; want served", code, er)
	}
}

// wf-checker's protocol probe on J: a refused child verb appends nothing to
// the journal, whatever its refusal.
func TestARefusedChildVerbWritesNothing(t *testing.T) {
	strictCallers(t)
	def := fanDef("refused-writes", chItems(1), 1, wf.AllSettled())
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("next: %v %v", ins, err)
	}
	u := agentsOf(ins)[0]
	writeCodexChild(t, os.Getenv("CODEX_HOME"), "boss", "kid-w", claimToken(t, u))
	t.Setenv("CODEX_THREAD_ID", "kid-w")
	out := writeAllocated(t, u.Output, `{"OK":true}`)
	before := mustSingle(t, def, "wf", "journal", "--state-dir", state)
	for _, args := range [][]string{
		{"wf", "done", u.Name, "--claim", claimToken(t, u), "--output", out},   // handed, not claimed
		{"wf", "failed", u.Name, "--claim", claimToken(t, u), "--reason", "x"}, // handed, not claimed
		{"wf", "prompt", "--claim", "0000000000000000"},                        // a wrong token
	} {
		if code, _, _ := singleCLI(def, append(args, "--state-dir", state)...); code == 0 {
			t.Fatalf("%v was accepted", args)
		}
		if after := mustSingle(t, def, "wf", "journal", "--state-dir", state); after != before {
			t.Errorf("refused %v appended to the journal:\n%s", args, strings.TrimPrefix(after, before))
		}
	}
	// The unit is claimed by its child, then the child's record is gone: its
	// validate still writes nothing and its done is taken, on the token alone.
	mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, u), "--state-dir", state)
	if err := os.Remove(filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "11", "15", "rollout-2026-11-15T00-00-00-kid-w.jsonl")); err != nil {
		t.Fatal(err)
	}
	before = mustSingle(t, def, "wf", "journal", "--state-dir", state)
	mustSingle(t, def, "wf", "validate", u.Name, "--claim", claimToken(t, u), "--output", out, "--state-dir", state)
	if after := mustSingle(t, def, "wf", "journal", "--state-dir", state); after != before {
		t.Errorf("validate appended to the journal:\n%s", strings.TrimPrefix(after, before))
	}
	mustSingle(t, def, "wf", "done", u.Name, "--claim", claimToken(t, u), "--output", out, "--state-dir", state)
	if after := mustSingle(t, def, "wf", "journal", "--state-dir", state); strings.Contains(after, `"child refused"`) || !strings.Contains(after, `"submitted"`) {
		t.Errorf("the done of a child whose record is gone was not taken:\n%s", strings.TrimPrefix(after, before))
	}
}

// G16: real V2 kickoff records survive the CLI caller/root plumbing.
func TestCodexV2RecordedChildThroughCLI(t *testing.T) {
	for _, tc := range []struct{ name, file, parent, child, claim string }{
		{"sol-first", "codex-v2-child-1.jsonl", "00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000002", "0123456789abcdef"},
		{"sol-retry", "codex-v2-child-2.jsonl", "10000000-0000-4000-8000-000000000004", "10000000-0000-4000-8000-000000000005", "1010101010101010"},
		{"terra-first", "codex-terra-child-1.jsonl", "10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002", "2020202020202020"},
		{"terra-retry", "codex-terra-child-2.jsonl", "10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000003", "3030303030303030"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			strictCallers(t)
			def := fanDef("v2-child", chItems(1), 1, wf.AllSettled())
			parent := tc.parent
			child := tc.child
			t.Setenv("CLAUDECODE", "")
			t.Setenv("CODEX_SESSION_ID", parent)
			t.Setenv("CODEX_THREAD_ID", parent)
			workspace := t.TempDir()
			root := filepath.Join(workspace, "harness")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", root)
			state := filepath.Join(t.TempDir(), "state")
			mustSingle(t, def, "run", "--state-dir", state, "--workspace", workspace, "--run-id", "r1")
			var ins []wf.Instruction
			if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil {
				t.Fatal(err)
			}
			unit := agentsOf(ins)[0]
			journal, err := os.ReadFile(filepath.Join(state, "runs", "r1", "journal.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			pinned := 0
			for _, line := range strings.Split(strings.TrimSpace(string(journal)), "\n") {
				var rec wf.JournalRecord
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatal(err)
				}
				if !rec.RelativePaths {
					t.Fatal("current CLI wrote a non-relative journal record")
				}
				var data map[string]any
				if err := json.Unmarshal(rec.Data, &data); err != nil {
					t.Fatal(err)
				}
				if recorded, ok := data["Root"]; ok {
					pinned++
					if recorded != root {
						t.Fatalf("pinned root encoded or changed: %v, want %s", recorded, root)
					}
				}
			}
			if pinned < 2 {
				t.Fatalf("missing coordinator/handout roots: %d", pinned)
			}

			raw, err := os.ReadFile(filepath.Join("../harness/testdata/started", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			body := strings.ReplaceAll(string(raw), tc.claim, claimToken(t, unit))
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			body = strings.ReplaceAll(body, "/test/bin/workflow", executable)
			// Place this fixture child after this test's offer.
			var lines []string
			for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
				var r map[string]any
				if err := json.Unmarshal([]byte(line), &r); err != nil {
					t.Fatal(err)
				}
				if r["type"] == "session_meta" {
					r["payload"].(map[string]any)["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
				}
				b, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				lines = append(lines, string(b))
			}
			sessions := filepath.Join(t.TempDir(), "sessions")
			if err := os.MkdirAll(filepath.Join(sessions, "2026", "11", "15"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sessions, filepath.Join(root, "sessions")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sessions, "2026", "11", "15", "rollout-real-"+child+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(tc.name, "terra-") {
				parentRaw, err := os.ReadFile("../harness/testdata/started/codex-terra-parent.jsonl")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sessions, "2026", "11", "15", "rollout-parent.jsonl"), parentRaw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"wf", "prompt", "--claim", claimToken(t, unit), "--state-dir", state}
			if code, _, _ := singleCLI(def, args...); code != 4 {
				t.Fatalf("coordinator accepted with child's record: %d", code)
			}
			t.Setenv("CODEX_THREAD_ID", child)
			if code, _, er := singleCLI(def, args...); code != 0 {
				t.Fatalf("real child rejected: %d %s", code, er)
			}
			// The settling precheck also receives the CLI identity; a stopped child
			// can report failed without losing the caller at the second guard.
			if code, _, er := singleCLI(def, "wf", "failed", "--claim", claimToken(t, unit), "--reason", "test stop", "--state-dir", state); code != 0 {
				t.Fatalf("child failed verb: %d %s", code, er)
			}

		})
	}
}

// A child-verb check that passed is not run again in a one-verb engine, but
// a revocation between it and the verb still refuses the verb: the stop of
// the run.
func TestPassedChildCheckDoesNotOutliveARevocation(t *testing.T) {
	d := fanDef("proto-revoke", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	wf.SetOneVerb(e)
	ins := handOutAs(t, h, run, 2, "boss")
	b := ins[1]
	k1 := codexChild("boss", "k1")
	if err := e.ChildVerbCheck(run, "prompt", b.Name, claimToken(t, b), k1); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PromptFetchAs(run, b.Name, claimToken(t, b), k1); err == nil || !strings.Contains(err.Error(), "is stopped") {
		t.Errorf("a first fetch after the stop = %v, want refused", err)
	}
}

// an earlier revision, the e2e-flash code run's shape: a coordinator whose harness runs
// three subagents takes offers in batches and leaves three units out of its
// spawns, so their children never fetch. Its nexts never reach the end of a
// wait (the harness cut each call, so J4's re-list never ran): each is one
// pass. The run still finishes: a later offer's child fetching, or
// UnstartedGrace when the left-out units are all that remain, brings them
// back. Its ended instruction carries the totals status prints.
func TestLeftOutSpawnsStillFinishTheRun(t *testing.T) {
	for _, drop := range [][]int{{3, 4, 9}, {12, 13, 14}} {
		t.Run(fmt.Sprint(drop), func(t *testing.T) {
			items := make([]int, 14)
			for i := range items {
				items[i] = i + 1
			}
			d := fanDef("proto-leftout", chItems(items...), 14, wf.AllSettled())
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			e := h.e.(*wf.Engine)
			co := wf.Coordinator{Harness: "codex", Session: "boss", Limit: 3, LimitSource: "three subagents in test"}
			left := map[string]bool{}
			for _, n := range drop {
				left[fmt.Sprintf("chFan/%d/chWork#1", n)] = true
			}
			for round := 0; round < 100; round++ {
				ins, err := e.HandOutAs(run, wf.NextAuto, co)
				if err != nil {
					t.Fatal(err)
				}
				if len(ins) == 1 && ins[0].Kind == wf.InstructionEnded {
					again := 0
					for _, rec := range h.kind(run, wf.JournalDegraded) {
						var dd wf.DegradedData
						if json.Unmarshal(rec.Data, &dd) == nil && dd.Kind == wf.DegradedUnstarted && left[dd.Unit] {
							again++
						}
					}
					if again < len(drop) {
						t.Errorf("%d never-started re-offers, want one per left-out unit (%d)", again, len(drop))
					}
					st, err := e.Status(run)
					if err != nil {
						t.Fatal(err)
					}
					if want, _, _ := strings.Cut(wf.StatusTotals(st), " · "); ins[0].Counts != want || !strings.HasPrefix(want, "subagents 14: 14 ") {
						t.Errorf("ended counts = %q, want status's %q for all 14", ins[0].Counts, want)
					}
					return
				}
				for _, in := range ins {
					if in.Kind != wf.InstructionAgent {
						continue
					}
					if left[in.Name] && !in.NotStarted {
						continue // left out of the spawn batch: no child ever fetches this offer
					}
					fetchAs(t, h, run, in, codexChild("boss", "kid-"+in.Name+claimToken(t, in)))
					if _, err := e.Done(run, in.Name, wf.Submission{Output: json.RawMessage(`{"N":1}`)}); err != nil {
						t.Fatal(err)
					}
				}
				h.now = h.now.Add(time.Minute)
			}
			t.Fatal("the run did not end in 100 nexts: left-out units sat handed")
		})
	}
}

// an earlier revision: a stopped run's instruction carries the run's totals exactly as
// status prints them, handed units included, so a final report quotes them.
func TestStoppedInstructionCarriesStatusCounts(t *testing.T) {
	d := fanDef("proto-counts", chItems(1, 2, 3, 4), 4, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins := handOutAs(t, h, run, 3, "boss")
	fetchAs(t, h, run, ins[0], codexChild("boss", "k1"))
	if _, err := e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"N":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "operator"); err != nil {
		t.Fatal(err)
	}
	got, err := e.HandOutAs(run, wf.NextAuto, wf.Coordinator{Harness: "codex", Session: "boss"})
	if err != nil || len(got) != 1 || got[0].Kind != wf.InstructionStopped {
		t.Fatalf("next of a stopped run = %+v, %v", got, err)
	}
	st, err := e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	want, _, _ := strings.Cut(wf.StatusTotals(st), " · ")
	if got[0].Counts != want || !strings.Contains(want, "2 handed") || !strings.Contains(want, "1 done") {
		t.Fatalf("stopped counts = %q, want status's %q (1 done, 2 handed, 1 pending)", got[0].Counts, want)
	}
	out := wf.RenderNext(wf.NextResult{Instructions: got}, "codex", func(v string, a ...string) string { return v })
	if !strings.Contains(out, wf.NextCountsLead+want) {
		t.Errorf("next does not print the totals to quote:\n%s", out)
	}
}

// an earlier revision re-review: a pass judges grace eligibility at one time, and a grace
// deadline the clock crosses between that judgment and the wait's timer is
// scheduled at once, not dropped. The clock here crosses the deadline at the
// k-th reading after next starts, for every k across a pass: next returns
// the never-started re-offer at once, never its whole wait's J4 re-list.
func TestGraceCrossedDuringAPassWakesNextAtOnce(t *testing.T) {
	for k := 1; k <= 60; k++ {
		d := fanDef("proto-cross", chItems(1), 1, wf.AllSettled())
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		first := handOutAs(t, h, run, 1, "boss")
		due := h.now.Add(5 * time.Minute)
		var mu sync.Mutex
		reads := 0
		armed := false
		clock := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			if !armed {
				return h.now
			}
			if reads++; reads < k {
				return due.Add(-time.Millisecond)
			}
			return due.Add(time.Millisecond)
		}
		e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Clock: clock, Defs: []*wf.Def{d}}).(*wf.Engine)
		mu.Lock()
		armed = true
		mu.Unlock()
		res, err := e.NextWait(run, 1, wf.Coordinator{Harness: "codex", Session: "boss"}, 2*time.Second)
		if again := agentsOf(res.Instructions); err != nil || res.Relisted || len(again) != 1 || !again[0].NotStarted || again[0].Name != first[0].Name {
			t.Fatalf("clock crossing at reading %d: %+v, %v; want %s offered again at once as never started", k, res, err, first[0].Name)
		}
	}
}

// A unit offered and never fetched is offered again for as long as the run
// goes on: no count of offers names it stuck or ends the session. Each
// offer waits for its evidence (here the grace), says to start the child
// again, and the latest one's child still runs the unit to the run's end. A
// negative grace is the default one.
func TestNeverStartedUnitIsOfferedAgainWithoutEnd(t *testing.T) {
	d := fanDef("proto-again", chItems(1), 1, wf.AllSettled())
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{UnstartedGrace: -time.Minute})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss"}
	first := handOutAs(t, h, run, 1, "boss")
	if again := handOutAs(t, h, run, 1, "boss"); len(again) != 0 {
		t.Fatalf("a negative grace re-offered at once: %+v", again)
	}
	var last wf.Instruction
	for i := 1; i <= 12; i++ {
		h.now = h.now.Add(4 * time.Minute)
		if res, err := e.NextWait(run, 1, co, 20*time.Millisecond); err != nil || len(agentsOf(res.Instructions)) != 0 && !res.Relisted {
			t.Fatalf("offer %d, inside the grace: next = %+v, %v; want no offer but the whole wait's re-list", i, res, err)
		}
		h.now = h.now.Add(5 * time.Minute)
		res, err := e.NextWait(run, 1, co, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range res.Instructions {
			if in.Kind == wf.InstructionStopped || in.Kind == wf.InstructionEnded {
				t.Fatalf("offer %d: the session was told to end: %+v", i, in)
			}
		}
		again := agentsOf(res.Instructions)
		if len(again) != 1 || !again[0].NotStarted || again[0].Name != first[0].Name {
			t.Fatalf("offer %d = %+v, want %s offered again as never started", i, res.Instructions, first[0].Name)
		}
		out := wf.RenderNext(res, "codex", func(v string, a ...string) string { return v })
		if !strings.Contains(out, "start its child again") || !strings.Contains(out, wf.NextStartAgain) ||
			strings.Contains(out, "end the session") || strings.Contains(out, "do not start") || strings.Contains(out, "Stuck") {
			t.Fatalf("offer %d reads:\n%s", i, out)
		}
		last = again[0]
	}
	fetchAs(t, h, run, last, codexChild("boss", "k"))
	if _, err := e.Done(run, last.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
}

// Whole waits with nothing else re-list a unit (J4) every time, however
// many passed, and the first offer's slow child still claims the unit with
// its token.
func TestWholeWaitsKeepRelisting(t *testing.T) {
	d := fanDef("proto-j4again", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss"}
	first := handOutAs(t, h, run, 1, "boss")
	for i := 1; i <= 8; i++ {
		h.now = h.now.Add(20 * time.Second)
		res, err := e.NextWait(run, 1, co, 20*time.Millisecond)
		if err != nil || !res.Relisted || len(agentsOf(res.Instructions)) != 1 {
			t.Fatalf("wait %d = %+v, %v; want a J4 re-list", i, res, err)
		}
		if out := wf.RenderNext(res, "codex", func(v string, a ...string) string { return v }); !strings.Contains(out, wf.NextStartAgain) {
			t.Fatalf("re-list %d does not say what to do about a child that keeps failing to start:\n%s", i, out)
		}
	}
	fetchAs(t, h, run, first[0], codexChild("boss", "slow"))
}

// A program polling next --json (HandOutAs, the path it uses) is offered a
// never-fetched unit again on every poll past the grace: an agent
// instruction each time, never a stopped one.
func TestNeverStartedUnitStaysInTheJSONHandOut(t *testing.T) {
	d := fanDef("proto-jsonagain", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss"}
	first := handOutAs(t, h, run, 1, "boss")
	for poll := 1; poll <= 10; poll++ {
		h.now = h.now.Add(5 * time.Minute)
		ins, err := e.HandOutAs(run, wf.NextAuto, co)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(ins)
		if err != nil {
			t.Fatal(err)
		}
		var got []map[string]any
		if err := json.Unmarshal(data, &got); err != nil || len(got) != 1 || got[0]["kind"] != "agent" || got[0]["name"] != first[0].Name || got[0]["notStarted"] != true || got[0]["stuck"] != nil {
			t.Fatalf("poll %d: next --json = %s, %v; want %s offered again as never started", poll, data, err, first[0].Name)
		}
	}
}

// A never-fetched unit is offered again beside the run's other work: with a
// gate waiting for an answer, next returns the gate and the unit, and no
// stopped instruction.
func TestNeverStartedUnitComesWithOtherWork(t *testing.T) {
	gd := wf.New("g-open")
	w := wf.NewAgent[hoWork, chOut](gd)
	ans := wf.NewEnum(gd, "ho-open-ans", hoAns("yes"), hoAns("no"))
	g := wf.NewGate[hoCheck, hoAns](gd, "Keep?", ans, wf.PolicyAnswerable)
	ok := wf.NewTerminal[hoDone](gd, wf.Success)
	no := wf.NewTerminal[hoStop](gd, wf.Stopped)
	gd.OnFailure(wf.Blocked, no)
	w.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[hoWork](wf.Const(1), w.To(no, "gave up")))
	toGate := w.To(g, "ok")
	w.Route(func(chOut, wf.Ctx) wf.Edge[hoWork] { return toGate }, toGate)
	g.Answers(wf.On(hoAns("yes"), g.To(ok, "yes")), wf.On(hoAns("no"), g.To(no, "no")))
	wf.Yield(ok, func(wf.Ctx) chRes { return chRes{} })
	wf.Yield(no, func(wf.Ctx) chRes { return chRes{} })
	d := wf.New("proto-againmixed")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](gd))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))

	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss"}
	ins := handOutAs(t, h, run, 2, "boss")
	fetchAs(t, h, run, ins[0], codexChild("boss", "k1"))
	if _, err := e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err) // its child now waits at its gate; ins[1] is never started
	}
	for poll := 1; poll <= 8; poll++ {
		h.now = h.now.Add(5 * time.Minute)
		got, err := e.HandOutAs(run, wf.NextAuto, co)
		if err != nil {
			t.Fatal(err)
		}
		gates, again := 0, agentsOf(got)
		for _, in := range got {
			if in.Kind == wf.InstructionStopped {
				t.Fatalf("poll %d: a stopped instruction came with an open gate: %+v", poll, got)
			}
			if in.Kind == wf.InstructionHumanGate {
				gates++
			}
		}
		if gates != 1 || len(again) != 1 || again[0].Name != ins[1].Name || !again[0].NotStarted {
			t.Fatalf("poll %d: next = %+v, want the gate and %s offered again", poll, got, ins[1].Name)
		}
	}
}

// Through the CLI, a coordinator whose unit is never started is offered it
// again on every call past the grace, in text and in JSON alike: every call
// exits 0, and none is the loop's end (exit 7) or says to end the session.
func TestNeverStartedUnitNeverEndsTheCLILoop(t *testing.T) {
	defer wf.SetUnstartedGrace(50 * time.Millisecond)()
	t.Setenv(wf.NextWaitCapEnv, "100ms")
	for _, form := range []string{"text", "json"} {
		t.Run(form, func(t *testing.T) {
			def := fanDef("cli-again-"+form, chItems(1), 1, wf.AllSettled())
			state := startPlain(t, def, "codex")
			args := []string{"wf", "next", "--state-dir", state}
			if form == "json" {
				args = []string{"wf", "next", "1", "--json", "--state-dir", state}
			}
			offers := 0
			for call := 0; call < 12; call++ {
				code, out, er := singleCLI(def, args...)
				if code != 0 {
					t.Fatalf("call %d exited %d: %s\n%s", call, code, out, er)
				}
				if strings.Contains(out, "end the session") || strings.Contains(out, "stopped:\n") || strings.Contains(out, `"kind": "stopped"`) || strings.Contains(out, "Stuck") {
					t.Fatalf("call %d told the session to end:\n%s", call, out)
				}
				if strings.Contains(out, "chFan/1/chWork#1") {
					offers++
				}
				time.Sleep(60 * time.Millisecond) // past the grace
			}
			if offers < 8 {
				t.Fatalf("the unit was offered in %d of 12 calls, want it offered again past every grace", offers)
			}
		})
	}
}

// an earlier revision (baseline-1117 seq 1246 and 1561): a real Codex child whose first
// command printed its bound result whole (`const r = await
// tools.exec_command({...}); text(r);`) was refused as the coordinator,
// because J2 read only `text(r.output)`. Whatever form its record holds, and
// when its first command is another one or its record shows no launch at
// all, the child is served on its claim token. The coordinator's own
// environment is still refused (I2) with the child's record present, and
// that refusal is journaled.
func TestCodexChildRecordFormsThroughCLI(t *testing.T) {
	const parent, child, recorded = "00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000002", "0123456789abcdef"
	for _, tc := range []struct {
		name string
		form func(string) string
	}{
		{"text-await", func(s string) string { return s }},
		{"text-result-output", func(s string) string {
			s = strings.Replace(s, `text(await tools.exec_command({`, `const r = await tools.exec_command({`, 1)
			return strings.Replace(s, "}));\n", "});text(r.output);\n", 1)
		}},
		{"text-result", func(s string) string {
			s = strings.Replace(s, `text(await tools.exec_command({`, `const r = await tools.exec_command({`, 1)
			return strings.Replace(s, "}));\n", "});text(r);\n", 1)
		}},
		{"other-first-command", func(s string) string {
			return `text(await tools.exec_command({cmd:"ls"}));` + "\n"
		}},
		{"missing-workspace-prefix", func(s string) string {
			return strings.Replace(s, "/test/bin/workflow", "./workflow", 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			strictCallers(t)
			def := fanDef("codex-forms", chItems(1), 1, wf.AllSettled())
			t.Setenv("CLAUDECODE", "")
			t.Setenv("CODEX_SESSION_ID", parent)
			t.Setenv("CODEX_THREAD_ID", parent)
			workspace := t.TempDir()
			root := filepath.Join(workspace, "harness")
			sessions := filepath.Join(root, "sessions", "2026", "11", "15")
			if err := os.MkdirAll(sessions, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", root)
			state := filepath.Join(t.TempDir(), "state")
			mustSingle(t, def, "run", "--state-dir", state, "--workspace", workspace, "--run-id", "r1")
			var ins []wf.Instruction
			if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil {
				t.Fatal(err)
			}
			unit := agentsOf(ins)[0]
			raw, err := os.ReadFile("../harness/testdata/started/codex-v2-child-1.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(executable, filepath.Join(workspace, "workflow")); err != nil {
				t.Fatal(err)
			}
			var lines []string
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var r map[string]any
				if err := json.Unmarshal([]byte(line), &r); err != nil {
					t.Fatal(err)
				}
				p := r["payload"].(map[string]any)
				if r["type"] == "session_meta" {
					p["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
				}
				if in, ok := p["input"].(string); ok {
					in = strings.ReplaceAll(in, recorded, claimToken(t, unit))
					p["input"] = strings.ReplaceAll(tc.form(in), "/test/bin/workflow", executable)
					if strings.HasPrefix(tc.name, "text-result") && strings.Contains(p["input"].(string), "}))") {
						t.Fatalf("the form did not apply to %q", in)
					}
				}
				b, _ := json.Marshal(r)
				lines = append(lines, string(b))
			}
			if err := os.WriteFile(filepath.Join(sessions, "rollout-2026-11-15T00-00-00-"+child+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"wf", "prompt", "--claim", claimToken(t, unit), "--state-dir", state}
			if code, _, _ := singleCLI(def, args...); code != 4 {
				t.Fatalf("the coordinator's own environment was accepted with its child's record: exit %d", code)
			}
			t.Setenv("CODEX_THREAD_ID", child)
			if code, _, er := singleCLI(def, args...); code != 0 {
				t.Fatalf("the child was refused: exit %d %s", code, er)
			}
			if n := strings.Count(mustSingle(t, def, "wf", "journal", "--state-dir", state), `"child refused"`); n != 1 {
				t.Errorf("child-refused records = %d, want 1 (the coordinator's attempt)", n)
			}
		})
	}
}

// The eval failure shape on Codex: a child of the first offer whose record
// the engine cannot find. Its fetch is served and binds the unit, its done
// settles it and the run ends; the journal holds no refusal.
func TestChildWithNoRecordFetchesAndFinishes(t *testing.T) {
	home := t.TempDir()
	d := fanDef("ax6-norecord", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss", Root: home}
	ins, err := e.HandOutAs(run, 1, co)
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	first := agentsOf(ins)[0]
	kid := hn.Caller{Harness: "codex", Session: "boss", Agent: "kid-1", Root: home, Is: hn.IsChild, FromEnv: true}
	start := time.Now()
	if _, err := e.PromptFetchAs(run, first.Name, claimToken(t, first), kid); err != nil || time.Since(start) > time.Second {
		t.Fatalf("a child with no record = %v after %s, want served at once", err, time.Since(start))
	}
	var fetched wf.PromptFetchedData
	if fs := h.kind(run, wf.JournalPromptFetched); len(fs) != 1 || json.Unmarshal(fs[0].Data, &fetched) != nil || fetched.Claim != claimToken(t, first) {
		t.Fatalf("prompt fetches %+v, want one binding the unit to its token", fs)
	}
	if _, err := e.Done(run, first.Name, wf.Submission{Output: json.RawMessage(`{"N":1}`)}); err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, rec := range h.kind(run, wf.JournalDegraded) {
		var dd wf.DegradedData
		if json.Unmarshal(rec.Data, &dd) != nil || dd.Unit != first.Name {
			continue
		}
		switch dd.Kind {
		case wf.DegradedChildRefused:
			refused++
		}
	}
	if refused != 0 {
		t.Fatalf("child-refused records = %d, want 0", refused)
	}
	end, err := e.HandOutAs(run, 1, co)
	if err != nil || len(end) != 1 || end[0].Kind != wf.InstructionEnded {
		t.Fatalf("after the done = %+v, %v; want the run ended", end, err)
	}
}

// selfFetch is the one identity refusal left (I2): the coordinator's own
// session fetching a unit no child has fetched.
func selfFetch(t *testing.T, e *wf.Engine, run wf.RunID, u wf.Instruction, session, root string) error {
	t.Helper()
	_, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), hn.Caller{Harness: "codex", Session: session, Root: root, Is: hn.IsSelf, FromEnv: true})
	return err
}

// an earlier revision review: a refused verb's audit record is its only write. With a
// declared migration pending (the binary's definition changed), the refusal
// journals the one child-refused record and no migration.
func TestIdentityRefusalWritesNoMigration(t *testing.T) {
	home := t.TempDir()
	v1, v2 := sizedAgentDef("ax6mig", "Old prompt."), sizedAgentDef("ax6mig", "New prompt.")
	v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash()})
	h := newHarness(t, v1, nil)
	run := h.run(v1, nil)
	ins, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "boss", Root: home})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	u := agentsOf(ins)[0]
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Clock: func() time.Time { return h.now }, Defs: []*wf.Def{v2}}).(*wf.Engine)
	before := len(h.records(run))
	if err := selfFetch(t, e2, run, u, "boss", home); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("the coordinator's own fetch = %v, want refused", err)
	}
	added := h.records(run)[before:]
	if len(added) != 1 || added[0].Kind != wf.JournalDegraded || !strings.Contains(string(added[0].Data), `"child refused"`) {
		t.Fatalf("the refusal wrote %+v, want only its child-refused record", added)
	}
}

// an earlier revision review: identity refusals of one unit from one caller session are
// recorded one by one up to 20, then once as suppressed, then not at all;
// another session's are still recorded. A failed audit write leaves the verb
// refused and says the record failed.
func TestIdentityRefusalsAreCapped(t *testing.T) {
	home := t.TempDir()
	d := fanDef("ax6-cap", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "boss", Root: home})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	u := agentsOf(ins)[0]
	count := func() (refused, capped int) {
		for _, rec := range h.kind(run, wf.JournalDegraded) {
			var dd wf.DegradedData
			if json.Unmarshal(rec.Data, &dd) != nil || dd.Unit != u.Name {
				continue
			}
			switch dd.Kind {
			case wf.DegradedChildRefused:
				refused++
			case wf.DegradedChildRefusalsCapped:
				capped++
			}
		}
		return
	}
	for i := 0; i < 23; i++ {
		if err := selfFetch(t, e, run, u, "", home); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("fetch %d = %v, want refused", i, err)
		}
	}
	if refused, capped := count(); refused != 20 || capped != 1 {
		t.Fatalf("after 23 refusals: %d child-refused, %d suppressed; want 20 and 1", refused, capped)
	}
	other := hn.Caller{Harness: "codex", Session: "other", Root: home, Is: hn.IsSelf, FromEnv: true}
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), other); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("another session's prompt = %v, want refused", err)
	}
	if refused, _ := count(); refused != 21 {
		t.Fatalf("another session's refusal was not recorded: %d child-refused", refused)
	}
	// Sessions are compared, not matched in the cause's text: a session whose
	// id ends as the capped (empty) session's record did is still recorded.
	lookalike := hn.Caller{Harness: "codex", Session: "x session ", Root: home, Is: hn.IsSelf, FromEnv: true}
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), lookalike); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("a lookalike session's prompt = %v, want refused", err)
	}
	if refused, _ := count(); refused != 22 {
		t.Fatalf("a lookalike session's refusal was suppressed by another session's cap: %d child-refused", refused)
	}

	failing := wf.NewEngine(&crashJournal{Journal: h.j, kind: wf.JournalDegraded, armed: true}, h.reg, wf.EngineOptions{Clock: func() time.Time { return h.now }, Defs: []*wf.Def{d}}).(*wf.Engine)
	if _, err := failing.PromptFetchAs(run, u.Name, claimToken(t, u), hn.Caller{Harness: "codex", Session: "third", Root: home, Is: hn.IsSelf, FromEnv: true}); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "recording this refusal failed") {
		t.Fatalf("a refusal whose record failed = %v, want refused and the failure named", err)
	}
}

// an earlier revision re-review: a refusal record is attributed where the journal put
// the unit, not where a fold against an unmigrated definition would. With a
// pending rename (mOld -> mNew), a refusal before the migration records the
// hand-out's state mOld under v1's hash; after the migration is committed,
// the next one records mNew.
func TestIdentityRefusalUnderAPendingRename(t *testing.T) {
	home := t.TempDir()
	v1 := migDef(false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "m1"})
	run := h.run(v1, nil)
	ins, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "boss", Root: home})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	u := agentsOf(ins)[0]
	handOut := h.kind(run, wf.JournalHandedOut)[0]
	if handOut.State != "mOld" {
		t.Fatalf("hand-out at %q, want mOld", handOut.State)
	}
	v2 := migDef(true)
	v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: map[string]string{"mOld": "mNew"}})
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Clock: func() time.Time { return h.now }, Defs: []*wf.Def{v2}, RunID: "m1"}).(*wf.Engine)
	refuse := func() wf.JournalRecord {
		t.Helper()
		before := len(h.records(run))
		if err := selfFetch(t, e2, run, u, "boss", home); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("the coordinator's own fetch = %v, want refused", err)
		}
		added := h.records(run)[before:]
		if len(added) != 1 || added[0].Kind != wf.JournalDegraded {
			t.Fatalf("the refusal wrote %+v, want one degraded record", added)
		}
		return added[0]
	}
	rec := refuse()
	if rec.State != "mOld" || rec.Instance != handOut.Instance || rec.Attempt != handOut.Attempt || rec.DefHash != handOut.DefHash {
		t.Fatalf("before the migration the refusal is at %q %q %d %s; want the hand-out's mOld %q %d %s", rec.State, rec.Instance, rec.Attempt, rec.DefHash, handOut.Instance, handOut.Attempt, handOut.DefHash)
	}
	if _, err := e2.Status(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.HandOutAs(run, 0, wf.Coordinator{Harness: "codex", Session: "boss", Root: home}); err != nil || len(h.kind(run, wf.JournalMigrated)) != 1 {
		t.Fatalf("the migration was not committed: %v", err)
	}
	if rec := refuse(); rec.State != "mNew" || rec.DefHash != v2.Hash() {
		t.Fatalf("after the migration the refusal is at %q under %s; want mNew under the migrated hash", rec.State, rec.DefHash)
	}
}

// an earlier revision re-review: a child's hand-out is renamed by a migration's qualified
// key for its own definition (child/mOld -> child/mRenamed), as the fold
// narrows it, and keeps the local state name; a root-level key with the same
// local name (mOld) does not leak into the child.
func TestIdentityRefusalUnderAChildRename(t *testing.T) {
	build := func(v2 bool) *wf.Def {
		d := wf.New("mig3")
		if v2 {
			d.Version(2)
		}
		sub := wf.NewSub[mSub, chItem, chRes](d, wf.Typed[chItem, chRes](migChain("child", v2, true)), func(wf.Ctx) chItem { return chItem{} })
		fin := wf.NewTerminal[mEnd](d, wf.Success)
		stop := wf.NewTerminal[mBad](d, wf.Stopped)
		sub.Exits(wf.OnExit[mEnd](sub.To(fin, "ok")), wf.OnExit[mBad](sub.To(stop, "bad")))
		return d
	}
	for _, states := range []map[string]string{
		{"child/mOld": "child/mRenamed"},
		{"child/mOld": "child/mRenamed", "mOld": "mRootOnly"}, // a root key with the child's local name
	} {
		t.Run(fmt.Sprint(len(states)), func(t *testing.T) {
			home := t.TempDir()
			v1 := build(false)
			h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "m3"})
			run := h.run(v1, nil)
			ins, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "boss", Root: home})
			if err != nil || len(agentsOf(ins)) != 1 || agentsOf(ins)[0].State != "child/mOld" {
				t.Fatalf("hand-out %+v, %v", ins, err)
			}
			u := agentsOf(ins)[0]
			handOut := h.kind(run, wf.JournalHandedOut)[0]
			v2 := build(true)
			v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: states})
			e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Clock: func() time.Time { return h.now }, Defs: []*wf.Def{v2}, RunID: "m3"}).(*wf.Engine)
			refuse := func() wf.JournalRecord {
				t.Helper()
				before := len(h.records(run))
				if err := selfFetch(t, e2, run, u, "boss", home); !errors.Is(err, wf.ErrRejected) {
					t.Fatalf("the coordinator's own fetch = %v, want refused", err)
				}
				added := h.records(run)[before:]
				if len(added) != 1 || added[0].Kind != wf.JournalDegraded {
					t.Fatalf("the refusal wrote %+v, want one degraded record", added)
				}
				return added[0]
			}
			if rec := refuse(); rec.State != "mOld" || rec.Instance != handOut.Instance || rec.DefHash != handOut.DefHash {
				t.Fatalf("before the migration: %q %q under %s; want mOld %q under %s", rec.State, rec.Instance, rec.DefHash, handOut.Instance, handOut.DefHash)
			}
			if _, err := e2.HandOutAs(run, 0, wf.Coordinator{Harness: "codex", Session: "boss", Root: home}); err != nil || len(h.kind(run, wf.JournalMigrated)) != 1 {
				t.Fatalf("the migration was not committed: %v", err)
			}
			if rec := refuse(); rec.State != "mRenamed" || rec.Instance != handOut.Instance || rec.DefHash != v2.Hash() {
				t.Fatalf("after the migration: %q %q under %s; want mRenamed %q under %s", rec.State, rec.Instance, rec.DefHash, handOut.Instance, v2.Hash())
			}
		})
	}
}

// refusedAsMismatch reports whether er is J1's refusal: it says not to start
// another agent CLI, sends the caller nowhere else to run the command
// (fanout sweep bl-codex: "run this from an agent of that harness" made a
// coordinator start that harness's CLI itself), and goes on to say what was
// expected and what the call shows. Naming the harness, a missing session or
// a variable is guidance, and is not held against it.
func refusedAsMismatch(er string) bool {
	_, detail, ok := strings.Cut(er, wf.HarnessMismatch+". ")
	if !ok || !strings.Contains(er, "do not start another agent CLI to do this run's units") {
		return false
	}
	for _, elsewhere := range []string{"an agent of that harness", "run this from", "run it from"} {
		if strings.Contains(strings.ToLower(er), elsewhere) {
			return false
		}
	}
	return strings.TrimSpace(detail) != ""
}

// J1's refusal says what was expected and what is missing, in plain words:
// the harness the run is driven from, the one the call shows, that a session
// is missing and which variables are not set. It prints names only: no
// variable's value and no records root.
func TestHarnessMismatchSaysWhatIsMissing(t *testing.T) {
	strictCallers(t)
	def := fanDef("mismatch-says", chItems(1), 1, wf.AllSettled())
	clear := func() {
		for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID"} {
			t.Setenv(k, "")
		}
	}
	clear()
	home := filepath.Join(t.TempDir(), "claude-home-value")
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-id-value")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	next := func() string {
		t.Helper()
		code, out, er := singleCLI(def, "wf", "next", "1", "--json", "--state-dir", state)
		if code != 4 || !refusedAsMismatch(er) {
			t.Fatalf("next: exit %d, %q %q; want J1's refusal", code, out, er)
		}
		for _, value := range []string{home, "session-id-value", "codex-thread-value"} {
			if strings.Contains(out+er, value) {
				t.Errorf("the refusal prints a value (%s):\n%s", value, er)
			}
		}
		return er
	}

	// The driving harness's environment with no session: the missing
	// variable is named.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if er := next(); !strings.Contains(er, "A coordinator session of claude is expected") || !strings.Contains(er, "names no session") ||
		!strings.Contains(er, "not set: ") || !strings.Contains(er, "CLAUDE_CODE_SESSION_ID") || strings.Contains(er, "not set: CLAUDECODE") {
		t.Errorf("a session-less claude call is told:\n%s", er)
	}
	// Another harness: both are named.
	clear()
	t.Setenv("CODEX_THREAD_ID", "codex-thread-value")
	if er := next(); !strings.Contains(er, "This part of the run is driven from claude; this command's environment is codex's") {
		t.Errorf("a codex call on a claude run is told:\n%s", er)
	}
	// No harness at all: the variables the driving harness is recognised by.
	clear()
	if er := next(); !strings.Contains(er, "driven from claude") || !strings.Contains(er, "shows no agent harness") || !strings.Contains(er, "CLAUDE_CODE_SESSION_ID") || !strings.Contains(er, "none of them is set") {
		t.Errorf("a plain shell on a claude run is told:\n%s", er)
	}
}

// The coordinator giving up on a claimed child fails the attempt, but not as
// the child's own failed: its record is not marked ChildFailed, so a later
// failed by that child is never told an earlier one already ended the unit.
func TestRecoveryGiveUpIsNotAChildFailure(t *testing.T) {
	d := clAgentDef("proto-ended")
	h := newHarness(t, d, nil)
	h.manualRecovery = true
	run, unit, offer := endedChild(t, h, d, "claude-handback.jsonl")
	if len(offer) != 1 || offer[0].Recovery == nil {
		t.Fatalf("no recovery offer: %+v", offer)
	}
	// endedChild's root holds projects/-repo/sess/subagents/agent-abc.jsonl.
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(offer[0].Recovery.Transcript)))))
	if err := h.e.Recover(run, unit.Name, wf.RecoveryGiveUp); err != nil {
		t.Fatal(err)
	}
	refused := h.kind(run, wf.JournalRefused)
	if len(refused) == 0 {
		t.Fatal("give-up journaled no failure")
	}
	for _, r := range append(refused, h.kind(run, wf.JournalBackoffScheduled)...) {
		if strings.Contains(string(r.Data), `"ChildFailed"`) {
			t.Errorf("seq %d (%s) is marked as the child's failed: %s", r.Seq, r.Kind, r.Data)
		}
	}
	// The owner's first failed, blocked, with the give-up's own reason.
	var rd wf.RefusedData
	if err := json.Unmarshal(refused[len(refused)-1].Data, &rd); err != nil || len(rd.Reasons) != 1 {
		t.Fatalf("give-up record %s: %v", refused[len(refused)-1].Data, err)
	}
	owner := hn.Caller{Harness: "claude", Session: "sess", Root: root, FromEnv: true}
	if repeated, err := wf.RepeatedFailed(h.e, run, claimToken(t, unit), rd.Reasons[0], "blocked", owner); err != nil || repeated {
		t.Errorf("the owner's failed after a give-up reads as a repeat: %v %v", repeated, err)
	}
}
