package wftest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A coordinator killed mid-wave, and a new session started at once (inside
// every child's stall threshold): its children are offered again as degraded
// orphans that spend none of the lost cap, and the new session finishes the
// run. On Codex the children are threads of the dead process, so the new
// session's first next re-offers them all. On a harness whose children may
// outlive their coordinator they are kept until the new coordinator reports
// one died, which then counts as an orphan too.

// claimOf is the claim token in an instruction's one-line brief.
func claimOf(t *testing.T, in wf.Instruction) string {
	t.Helper()
	f := strings.Fields(in.Brief)
	for i := range f[:len(f)-1] {
		if f[i] == "--claim" {
			return strings.TrimSuffix(f[i+1], ",")
		}
	}
	t.Fatalf("no claim token in brief %q", in.Brief)
	return ""
}

func agentUnits(ins []wf.Instruction) []wf.Instruction {
	var out []wf.Instruction
	for _, in := range ins {
		if in.Kind == wf.InstructionAgent {
			out = append(out, in)
		}
	}
	return out
}

// orphanRun is six parallel units under LostCap 1: any loss that counted
// toward the cap would fail its unit.
func orphanRun(t *testing.T) *drv {
	return newDrv(t, false, crFanDef(6, wf.AllSettled()), nil, func(o *wf.EngineOptions) { o.LostCap = 1 })
}

// finishAs fetches and finishes every unit as children of session under h.
func (h *drv) finishAs(co wf.Coordinator, units []wf.Instruction) {
	h.t.Helper()
	e := h.e.(*wf.Engine)
	for _, in := range units {
		_, err := e.PromptFetchAs(h.id, in.Name, claimOf(h.t, in), hn.Caller{Harness: co.Harness, Session: co.Session, Agent: "kid-" + in.Name, Is: hn.IsChild, FromEnv: true})
		h.must(err)
		_, err = e.Done(h.id, in.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
		h.must(err)
	}
}

func (h *drv) lostCauses() []string {
	var causes []string
	for _, r := range h.records() {
		if r.Kind == wf.JournalAttemptLost {
			var d wf.AttemptLostData
			h.must(json.Unmarshal(r.Data, &d))
			causes = append(causes, d.Cause)
		}
	}
	return causes
}

func TestCoordinatorKilledMidWaveCodex(t *testing.T) {
	h := orphanRun(t)
	e := h.e.(*wf.Engine)
	s1 := wf.Coordinator{Harness: "codex", Session: "s1"}
	wave, err := e.HandOutAs(h.id, 6, s1)
	h.must(err)
	wave = agentUnits(wave)
	if len(wave) != 6 {
		t.Fatalf("first wave = %d units, want 6", len(wave))
	}
	for _, in := range wave[:5] { // five spawned and claimed; the sixth never spawned
		_, err := e.PromptFetchAs(h.id, in.Name, claimOf(t, in), hn.Caller{Harness: "codex", Session: "s1", Agent: "kid-" + in.Name, Is: hn.IsChild, FromEnv: true})
		h.must(err)
	}
	// kill -TERM on s1: its five child threads die with it. A new session
	// starts 65 s later and calls next --wait.
	h.now = h.now.Add(65 * time.Second)
	s2 := wf.Coordinator{Harness: "codex", Session: "s2"}
	res, err := e.NextWait(h.id, 6, s2, time.Second)
	h.must(err)
	again := agentUnits(res.Instructions)
	if res.Timeout || len(again) != 6 {
		t.Fatalf("the new session's first next offered %d units (timeout %v), want all 6 at once", len(again), res.Timeout)
	}
	if causes := h.lostCauses(); len(causes) != 5 || strings.Count(strings.Join(causes, ","), wf.LostCauseOrphan) != 5 {
		t.Fatalf("lost causes = %v, want five orphans", causes)
	}
	if n := kindCount(h.records(), wf.JournalDegraded); n != 6 {
		t.Fatalf("%d degraded records, want 6 (five dead children, one never-claimed hand-out)", n)
	}
	h.finishAs(s2, again)
	if out := h.outcome(); out != "ended/crFMerge retries=0" {
		t.Fatalf("outcome = %s, want every unit merged with no retry spent", out)
	}
}

func TestCoordinatorKilledMidWaveChildrenMayOutliveIt(t *testing.T) {
	h := orphanRun(t)
	e := h.e.(*wf.Engine)
	s1 := wf.Coordinator{Harness: "remote", Session: "s1"}
	wave, err := e.HandOutAs(h.id, 6, s1)
	h.must(err)
	wave = agentUnits(wave)
	for _, in := range wave {
		_, err := e.PromptFetchAs(h.id, in.Name, claimOf(t, in), hn.Caller{Harness: "remote", Session: "s1", Agent: "kid-" + in.Name, Is: hn.IsChild, FromEnv: true})
		h.must(err)
	}
	h.now = h.now.Add(65 * time.Second)
	s2 := wf.Coordinator{Harness: "remote", Session: "s2"}
	if again := agentUnits(must1(t)(e.HandOutAs(h.id, 6, s2))); len(again) != 0 {
		t.Fatalf("children that may still run were offered again: %d", len(again))
	}
	// Three of s1's children did outlive it and finish; the new coordinator
	// reports the other three died.
	for _, in := range wave[:3] {
		_, err := e.Done(h.id, in.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
		h.must(err)
	}
	for _, in := range wave[3:] {
		h.must(e.HeartbeatAs(h.id, wf.HeartbeatTarget{Name: in.Name}, false, s2))
	}
	if causes := h.lostCauses(); len(causes) != 3 || strings.Count(strings.Join(causes, ","), wf.LostCauseOrphan) != 3 {
		t.Fatalf("lost causes = %v, want three orphans", causes)
	}
	again := agentUnits(must1(t)(e.HandOutAs(h.id, 6, s2)))
	if len(again) != 3 {
		t.Fatalf("re-offered %d units, want 3", len(again))
	}
	h.finishAs(s2, again)
	if out := h.outcome(); out != "ended/crFMerge retries=0" {
		t.Fatalf("outcome = %s, want every unit merged with no retry spent", out)
	}
}

func must1(t *testing.T) func([]wf.Instruction, error) []wf.Instruction {
	return func(ins []wf.Instruction, err error) []wf.Instruction {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ins
	}
}

// A crash at the coordinator record of a new session's first next: the
// restarted engine's next from the same session journals it again and still
// offers the dead session's units, and the run finishes as uncrashed.
func init() {
	crashPoints[wf.JournalCoordinator] = scenario(wf.JournalCoordinator, func(t *testing.T, crash bool) *drv {
		return newDrv(t, crash, crFanDef(2, wf.AllSettled()), nil)
	}, func(h *drv) {
		e := func() *wf.Engine { return h.e.(*wf.Engine) }
		s1 := wf.Coordinator{Harness: "codex", Session: "s1"}
		wave, err := e().HandOutAs(h.id, 2, s1)
		h.must(err)
		for _, in := range agentUnits(wave) {
			_, err := e().PromptFetchAs(h.id, in.Name, claimOf(h.t, in), hn.Caller{Harness: "codex", Session: "s1", Agent: "kid-" + in.Name, Is: hn.IsChild, FromEnv: true})
			h.must(err)
		}
		s2 := wf.Coordinator{Harness: "codex", Session: "s2"}
		var again []wf.Instruction
		h.at(wf.JournalCoordinator, func() { again, _ = e().HandOutAs(h.id, 2, s2) })
		if h.crash {
			// The crash cut the first call: the restarted engine's next from
			// the same session does what it would have.
			again, err = e().HandOutAs(h.id, 2, s2)
			h.must(err)
		}
		h.finishAs(s2, agentUnits(again))
	})
}

// A crash at the rewound record of a fork (the prefix written, the record not):
// the next rewind finishes that fork instead of starting another, and the
// fork carries on to the end as the uncrashed one does.
func init() {
	crashPoints[wf.JournalRewound] = scenario(wf.JournalRewound, simpleRun, func(h *drv) {
		h.finish(`{"OK":true}`)
		var fork wf.RunID
		h.at(wf.JournalRewound, func() {
			rep, err := h.e.(*wf.Engine).Rewind(h.id, "died")
			if err == nil {
				fork = rep.Run
			}
		})
		if h.crash {
			rep, err := h.e.(*wf.Engine).Rewind(h.id, "died")
			h.must(err)
			fork = rep.Run
		}
		if fork != wf.RunID(string(h.id)+".rw1") {
			h.t.Fatalf("fork = %s, want the first fork finished", fork)
		}
		h.id = fork
		h.finish(`{"OK":true}`)
	})
}
