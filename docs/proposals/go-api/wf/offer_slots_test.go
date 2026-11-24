package wf_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// pi v41 (the workflow run): a 22-child panel driven by a coordinator that runs at
// most two children at once, each for about three minutes. Every offer the
// coordinator cannot start yet sat unclaimed past UnstartedGrace, each re-offer
// counted toward the cap, and the run stopped with ten units named stuck. With
// its two child slots declared, the coordinator is offered no more than it can
// start, nothing it is waiting on a slot for is a strike, and the panel ends.
func TestPacedCoordinatorWithTwoSlotsFinishesTwentyTwoChildren(t *testing.T) {
	items := make([]int, 22)
	for i := range items {
		items[i] = i + 1
	}
	d := fanDef("proto-slots", chItems(items...), 22, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss", Children: 2}
	var running []wf.Instruction
	overOffered := false
	for round := 0; round < 400; round++ {
		res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Instructions) == 1 && res.Instructions[0].Kind == wf.InstructionEnded {
			if done := len(h.kind(run, wf.JournalAccepted)); done < 22 {
				t.Fatalf("the run ended with %d units accepted, want 22", done)
			}
			for _, rec := range h.kind(run, wf.JournalDegraded) {
				var dd wf.DegradedData
				if json.Unmarshal(rec.Data, &dd) == nil && dd.Kind == wf.DegradedUnstarted {
					t.Errorf("a unit waiting on a slot was struck as never started: %+v", dd)
				}
			}
			return
		}
		for _, in := range res.Instructions {
			if in.Kind == wf.InstructionStopped {
				t.Fatalf("round %d: next stopped the paced coordinator: %s", round, in.Prompt)
			}
		}
		offered := agentsOf(res.Instructions)
		if len(running)+len(offered) > 2 && !overOffered {
			overOffered = true
			t.Errorf("round %d: offered %d with %d running, past the two declared slots", round, len(offered), len(running))
		}
		// The coordinator starts what its two slots hold; it cannot start
		// the rest.
		for _, in := range offered {
			if len(running) == 2 {
				break
			}
			fetchAs(t, h, run, in, codexChild(co.Session, "kid-"+in.Name))
			running = append(running, in)
		}
		// A child runs about three minutes; the oldest one finishes.
		h.now = h.now.Add(3 * time.Minute)
		if len(running) > 0 {
			if _, err := e.Done(run, running[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
				t.Fatal(err)
			}
			running = running[1:]
		}
	}
	t.Fatal("the run did not end in 400 rounds")
}

// A coordinator whose declared slots are full is offered nothing: its next
// blocks (here, to its short timeout) with no unit, no re-list of an earlier
// offer and no stuck verdict, however long its children run.
func TestFullSlotsOfferNothingAndStrikeNothing(t *testing.T) {
	d := fanDef("proto-full", chItems(1, 2, 3, 4), 4, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss", Children: 2}
	res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	first := agentsOf(res.Instructions)
	if len(first) != 2 {
		t.Fatalf("first next offered %d, want the two declared slots", len(first))
	}
	for _, in := range first {
		fetchAs(t, h, run, in, codexChild("boss", "kid-"+in.Name))
	}
	for i := 0; i < 6; i++ {
		h.now = h.now.Add(2 * time.Minute) // past UnstartedGrace in all, inside the fetched lease
		res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Timeout || len(res.Instructions) != 0 {
			t.Fatalf("wait %d with both slots running = %+v; want nothing until a slot frees", i, res)
		}
	}
	// An explicit count is held to the free slots too.
	if more, err := e.HandOutAs(run, 5, co); err != nil || len(agentsOf(more)) != 0 {
		t.Fatalf("next 5 with both slots running offered %d, %v", len(agentsOf(more)), err)
	}
	if _, err := e.Done(run, first[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	res, err = e.NextWait(run, wf.NextAuto, co, time.Second)
	if err != nil || len(agentsOf(res.Instructions)) != 1 {
		t.Fatalf("next after a slot freed = %+v, %v; want one unit", res, err)
	}
}

// Offers a coordinator holds while its slots are full (here, taken before it
// declared them) wait on a slot: no re-list, no re-offer and no stuck verdict
// while its children run, and once one finishes, no more than the freed slot
// is offered.
func TestOffersWaitingOnAFullSlotAreNoStrike(t *testing.T) {
	d := fanDef("proto-held", chItems(1, 2, 3, 4, 5, 6), 6, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	all := handOutAs(t, h, run, -1, "boss") // no slots declared: every ready unit
	if len(all) != 6 {
		t.Fatalf("undeclared next offered %d, want 6", len(all))
	}
	for _, in := range all[:2] {
		fetchAs(t, h, run, in, codexChild("boss", "kid-"+in.Name))
	}
	handed := len(h.kind(run, wf.JournalHandedOut))
	co := wf.Coordinator{Harness: "codex", Session: "boss", Children: 2}
	for i := 0; i < 5; i++ {
		h.now = h.now.Add(3 * time.Minute) // 15m in all: past UnstartedGrace, inside the stall threshold
		res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Timeout || len(res.Instructions) != 0 {
			t.Fatalf("wait %d with both slots running = %+v; want nothing until a slot frees", i, res.Instructions)
		}
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != handed {
		t.Fatalf("offers waiting on a full slot were handed out again (%d hand-outs, was %d)", n, handed)
	}
	if _, err := e.Done(run, all[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	res, err := e.NextWait(run, wf.NextAuto, co, time.Second)
	if err != nil || len(agentsOf(res.Instructions)) != 1 {
		t.Fatalf("next after a slot freed = %+v, %v; want one unit offered", res.Instructions, err)
	}
}

// Review round 1: the end-of-wait re-list (J4) is an offer like any other,
// held to the child cap less the children running: five offers taken before
// the cap was declared, one child running and a cap of two re-list one.
func TestReListIsHeldToTheChildCap(t *testing.T) {
	d := fanDef("proto-relist", chItems(1, 2, 3, 4, 5, 6), 6, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	all := handOutAs(t, h, run, -1, "boss")
	if len(all) != 6 {
		t.Fatalf("undeclared next offered %d, want 6", len(all))
	}
	fetchAs(t, h, run, all[0], codexChild("boss", "kid-"+all[0].Name))
	co := wf.Coordinator{Harness: "codex", Session: "boss", Children: 2}
	res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := agentsOf(res.Instructions); !res.Relisted || len(got) != 1 {
		t.Fatalf("re-list with one child running and a cap of two = %d units (relisted %v), want 1", len(got), res.Relisted)
	}
}

// Review round 1: a session-less caller (a library's) counts its unclaimed
// offers as starting: a second call before any fetch offers nothing more.
func TestSessionlessOffersCountTowardTheChildCap(t *testing.T) {
	d := fanDef("proto-sessionless", chItems(1, 2, 3, 4, 5, 6), 6, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Children: 2}
	first, err := e.HandOutAs(run, -1, co)
	if err != nil || len(agentsOf(first)) != 2 {
		t.Fatalf("first session-less call = %d units, %v; want 2", len(agentsOf(first)), err)
	}
	second, err := e.HandOutAs(run, -1, co)
	if err != nil || len(agentsOf(second)) != 0 {
		t.Fatalf("second session-less call before any fetch = %d units, %v; want 0", len(agentsOf(second)), err)
	}
}

// Review round 2, as it stands after E4 (no re-offer cap): every offer the
// coordinator holds unclaimed is in the re-list, a long-unstarted one too,
// and the re-list is held to the child cap: A offered four times, B and C
// offered once, a cap of two and no fetch re-lists two.
func TestReListOfManyStartingOffersIsHeldToTheCap(t *testing.T) {
	d := fanDef("proto-relist-final", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	a := handOutAs(t, h, run, 1, "boss")
	for i := 1; i <= 3; i++ {
		h.now = h.now.Add(5 * time.Minute)
		if again := handOutAs(t, h, run, 1, "boss"); len(again) != 1 || again[0].Name != a[0].Name {
			t.Fatalf("re-offer %d = %+v, want %s again", i, again, a[0].Name)
		}
	}
	bc := handOutAs(t, h, run, 2, "boss")
	if len(bc) != 2 {
		t.Fatalf("B and C = %d units, want 2", len(bc))
	}
	co := wf.Coordinator{Harness: "codex", Session: "boss", Children: 2}
	res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := agentsOf(res.Instructions); len(got) != 2 {
		t.Fatalf("re-list of %s, B and C with a cap of two = %d units, want 2", a[0].Name, len(got))
	}
}

// Two children claimed both places and died without done or
// failed; one unit is ready. While their leases run, a blocking next with
// the cap full waits; once they run out it asks about each, with the
// command to declare it dead, and the declaration frees a place for the
// ready unit.
func TestFullCapAsksAboutExpiredChildren(t *testing.T) {
	d := fanDef("proto-dead", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss", Children: 2}
	res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	two := agentsOf(res.Instructions)
	if len(two) != 2 {
		t.Fatalf("first next offered %d, want 2", len(two))
	}
	for _, in := range two {
		fetchAs(t, h, run, in, codexChild("boss", "kid-"+in.Name))
	}
	h.now = h.now.Add(21 * time.Minute) // past the stall default, inside the 30m lease
	if res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond); err != nil || !res.Timeout || len(res.Stalled) != 0 {
		t.Fatalf("inside the lease next = %+v, %v; want nothing yet", res, err)
	}
	h.now = h.now.Add(10 * time.Minute) // past the lease
	res, err = e.NextWait(run, wf.NextAuto, co, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Timeout || len(res.Stalled) != 2 || len(agentsOf(res.Instructions)) != 0 {
		t.Fatalf("expired children with the cap full = %+v; want both asked about, nothing offered", res)
	}
	out := wf.RenderNext(res, "codex", func(v string, a ...string) string { return v + " " + strings.Join(a, " ") })
	for _, in := range two {
		if !strings.Contains(out, "heartbeat died "+in.Name) || !strings.Contains(out, "heartbeat active "+in.Name) {
			t.Errorf("the question lacks the two answers for %s:\n%s", in.Name, out)
		}
	}
	if err := e.Heartbeat(run, wf.HeartbeatTarget{Name: two[0].Name}, false); err != nil {
		t.Fatal(err)
	}
	res, err = e.NextWait(run, wf.NextAuto, co, time.Second)
	if err != nil || len(agentsOf(res.Instructions)) != 1 {
		t.Fatalf("next after one child was declared dead = %+v, %v; want one unit in the freed place", res, err)
	}
}

// With nobody answering, an expired lease frees no place: silence is no
// evidence of death (rule 5). The places stay held and the question stays
// open until the coordinator answers.
func TestLeaseExpiryAsksInsteadOfFreeingACap(t *testing.T) {
	d := fanDef("proto-dead-lease", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss", Children: 2}
	res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range agentsOf(res.Instructions) {
		fetchAs(t, h, run, in, codexChild("boss", "kid-"+in.Name))
	}
	for _, later := range []time.Duration{31 * time.Minute, 3 * time.Hour} {
		h.now = h.now.Add(later)
		res, err = e.NextWait(run, wf.NextAuto, co, time.Second)
		if err != nil || len(agentsOf(res.Instructions)) != 0 || len(res.Stalled) != 2 {
			t.Fatalf("next %v past the lease = %+v, %v; want both asked about, no place freed", later, res, err)
		}
	}
	if lost := lostRecords(t, h, run); len(lost) != 0 {
		t.Fatalf("lost without an answer: %+v", lost)
	}
}

// Review round 1 P1: a child that fetched with an earlier session's offer is
// not retired on elapsed time when a new session meets the run. When its
// lease runs out, the new session is asked about it, by the claim its
// child fetched with, and nothing is lost until it answers.
func TestReplacementCoordinatorIsAskedNotOrphaning(t *testing.T) {
	d := fanDef("proto-overdue-owner", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	a := handOutAsH(t, h, run, 1, "old", "pi") // pi children outlive their coordinator
	if again := handOutAsH(t, h, run, 1, "new", "pi"); len(again) != 1 || again[0].Name != a[0].Name {
		t.Fatalf("the new session = %+v, want %s offered again", again, a[0].Name)
	}
	fetchAs(t, h, run, a[0], hn.Caller{Harness: "pi", Session: "old", Agent: "kid", Is: hn.IsChild, FromEnv: true}) // the old offer's token wins
	co := wf.Coordinator{Harness: "pi", Session: "new", Children: 1}
	h.now = h.now.Add(21 * time.Minute) // quiet past the stall default
	if _, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if lost := lostRecords(t, h, run); len(lost) != 0 {
		t.Fatalf("the new session retired the old session's quiet child: %+v", lost)
	}
	h.now = h.now.Add(10 * time.Minute) // past the lease
	res, err := e.NextWait(run, wf.NextAuto, co, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stalled) != 1 || res.Stalled[0].Name != a[0].Name || res.Stalled[0].Claim != claimToken(t, a[0]) {
		t.Fatalf("stalled = %+v, want %s asked about by its child's claim", res.Stalled, a[0].Name)
	}
	if lost := lostRecords(t, h, run); len(lost) != 0 {
		t.Fatalf("lost before an answer: %+v", lost)
	}
}

// Review (3665c750) P2: a class whose samples finished at once (a zero
// duration) gives no zero threshold: a just-fetched child is not overdue.
func TestZeroDurationClassIsNoZeroThreshold(t *testing.T) {
	d := fanDef("proto-overdue-zero", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss", Children: 1}
	for i := 0; i < 2; i++ {
		res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		in := agentsOf(res.Instructions)
		if len(in) != 1 {
			t.Fatalf("call %d offered %+v, want one unit", i, res)
		}
		fetchAs(t, h, run, in[0], codexChild("boss", "kid-"+in[0].Name))
		if i == 0 {
			if _, err := e.Done(run, in[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := e.NextWait(run, wf.NextAuto, co, 20*time.Millisecond)
	if err != nil || !res.Timeout || len(res.Stalled) != 0 {
		t.Fatalf("a just-fetched child after a zero-duration sample = %+v, %v; want nothing yet", res, err)
	}
}

// Review (3665c750) P3 and round 1 P2: the stalled question is
// observational: it never prints a harness's stall check (pi's told the
// coordinator to read pi's session files and kill a quiet process), and it
// names the claim its child fetched with.
func TestStalledQuestionPrintsNoHarnessCheck(t *testing.T) {
	res := wf.NextResult{Stalled: []wf.StalledView{{Name: "u#1", Since: time.Date(2026, 11, 20, 2, 20, 0, 0, time.UTC), Claim: "0123456789abcdef"}}}
	for _, h := range []string{"pi", "claude", "codex", "hermes", "agy", "generic"} {
		out := wf.RenderNext(res, h, func(v string, a ...string) string { return v + " " + strings.Join(a, " ") })
		// Final review P1: not seeing a child is no proof it is gone (an
		// unreadable environment, another PID namespace): died is for a
		// confirmed exit, and an unknown status leaves the claim pending.
		for _, bad := range []string{"SIGTERM", "SIGKILL", "/proc/", "sessions", "kill", "no child you can see"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: the stalled question prints %q:\n%s", h, bad, out)
			}
		}
		for _, want := range []string{"stalled u#1", "0123456789abcdef", "without stopping it", "heartbeat active u#1", "heartbeat died u#1", "confirmed", "If you cannot tell", "run neither", "stays pending"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: the stalled question lacks %q:\n%s", h, want, out)
			}
		}
	}
}

// piReofferWon sets up the final review's shape on pi: a zero-duration
// sample of the class, then unit A offered to session old, offered again to
// session new, and its child started from old's offer (old's token wins).
func piReofferWon(t *testing.T) (*harness, wf.RunID, wf.Instruction) {
	t.Helper()
	d := fanDef("proto-pi-won", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	piKid := func(session, agent string) hn.Caller {
		return hn.Caller{Harness: "pi", Session: session, Agent: agent, Is: hn.IsChild, FromEnv: true}
	}
	old, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "pi", Session: "old"})
	if err != nil || len(agentsOf(old)) != 2 {
		t.Fatalf("old session = %+v, %v; want 2 units", old, err)
	}
	a, b := agentsOf(old)[0], agentsOf(old)[1]
	fetchAs(t, h, run, b, piKid("old", "kid-b"))
	if _, err := e.Done(run, b.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil { // a zero-duration sample
		t.Fatal(err)
	}
	again, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "pi", Session: "new"})
	if err != nil || len(agentsOf(again)) != 1 || agentsOf(again)[0].Name != a.Name {
		t.Fatalf("new session = %+v, %v; want %s offered again", again, err, a.Name)
	}
	fetchAs(t, h, run, a, piKid("old", "kid-a")) // old's token wins
	return h, run, a
}

// Final review (974b23f1) P1a: a zero threshold is no evidence anywhere: the
// new session's next does not sweep the just-fetched child of the old
// session's offer as an orphan gone quiet.
func TestFreshChildOfAReplacedSessionIsNoOrphanAtAgeZero(t *testing.T) {
	h, run, a := piReofferWon(t)
	e := h.e.(*wf.Engine)
	before := len(h.kind(run, wf.JournalDegraded))
	if _, err := e.NextWait(run, wf.NextAuto, wf.Coordinator{Harness: "pi", Session: "new", Children: 1}, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for _, rec := range h.kind(run, wf.JournalDegraded)[before:] {
		var dd wf.DegradedData
		if json.Unmarshal(rec.Data, &dd) == nil && dd.Unit == a.Name && dd.Kind == wf.DegradedOrphan {
			t.Fatalf("the just-fetched child was swept as an orphan: %+v", dd)
		}
	}
	// Its child goes on and finishes on its own token.
	if _, err := e.Done(run, a.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatalf("the child's done after the new session's next = %v", err)
	}
}

// Final review (974b23f1) P1b: the child belongs to the session whose offer
// it started from. A newer session reporting it died loses it as an orphan
// of the session it replaced (no lost-cap charge), never as its own child's
// death.
func TestHeartbeatDiedOnAReplacedSessionsChildIsAnOrphan(t *testing.T) {
	h, run, a := piReofferWon(t)
	e := h.e.(*wf.Engine)
	if err := e.HeartbeatAs(run, wf.HeartbeatTarget{Name: a.Name}, false, wf.Coordinator{Harness: "pi", Session: "new"}); err != nil {
		t.Fatal(err)
	}
	var causes []string
	for _, rec := range h.kind(run, wf.JournalAttemptLost) {
		var d wf.AttemptLostData
		if json.Unmarshal(rec.Data, &d) == nil && d.Name == a.Name {
			causes = append(causes, d.Cause)
		}
	}
	if len(causes) != 1 || causes[0] != wf.LostCauseOrphan {
		t.Fatalf("lost causes of %s = %v, want one %q", a.Name, causes, wf.LostCauseOrphan)
	}
}
