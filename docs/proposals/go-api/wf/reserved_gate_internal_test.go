package wf

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

type reservedQuestion struct{}
type reservedFinished struct{}
type reservedAnswer string

func reservedGateFixture(t *testing.T, caller harness.Caller, reservation Reservation, timeout ...time.Duration) (*Engine, Journal, RunID) {
	t.Helper()
	d := New("reserved-core")
	gate := NewGate[reservedQuestion, reservedAnswer](d, "Ship the release?", NewEnum(d, "decision", reservedAnswer("yes")), reservation)
	done := NewTerminal[reservedFinished](d, Success)
	gate.Answers(On(reservedAnswer("yes"), gate.To(done, "approved")))
	if len(timeout) > 0 {
		gate.Timeout(timeout[0], gate.To(done, "timeout"))
	}
	j := MemJournal()
	e := NewEngine(j, NewAdapters(), EngineOptions{Defs: []*Def{d}, caller: caller}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e, j, run
}

func reservedRecords(t *testing.T, j Journal, run RunID) []JournalRecord {
	t.Helper()
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	records, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestReservedGateAnswersRemainCallableAndRecordSession(t *testing.T) {
	for _, session := range []string{"", "coordinator-session"} {
		for _, visit := range []bool{false, true} {
			e, j, run := reservedGateFixture(t, harness.Caller{FromEnv: true, Session: session, Harness: "codex"}, Reserved)
			var err error
			if visit {
				err = e.AnswerVisit(run, GateAnswer{Gate: "reservedQuestion", Value: "yes"})
			} else {
				err = e.Answer(run, "reservedQuestion", "yes")
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range reservedRecords(t, j, run) {
				if r.Kind == JournalGateAnswered {
					found = true
					var data GateAnsweredData
					if err := json.Unmarshal(r.Data, &data); err != nil {
						t.Fatal(err)
					}
					if data.Session != session || data.Answer != "yes" {
						t.Fatalf("answer provenance: %+v", data)
					}
				}
			}
			if !found {
				t.Fatal("answer not journaled")
			}
		}
	}
}

func TestReservedGateCoordinatorRepeatsWholeHumanHandoff(t *testing.T) {
	e, j, run := reservedGateFixture(t, harness.Caller{}, Reserved)
	wave, err := e.Next(run, "")
	if err != nil || len(wave) != 1 || wave[0].Kind != InstructionHumanGate {
		t.Fatalf("machine gate seam: %+v %v", wave, err)
	}
	answer := e.opts.AnswerLine(run, "reservedQuestion") + " --child '' --visit 1"
	want := "This run is waiting for a person at reservedQuestion: Ship the release? Choices: yes. Ask the person who started the run and wait for their reply. Answer only with what they tell you; do not choose for them. Run this with their choice as <value>:\n" + answer + "\n"
	for attempt := 0; attempt < 2; attempt++ {
		result, err := e.NextWait(run, -1, Coordinator{Session: "coordinator"}, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Instructions) != 1 || result.Instructions[0].Kind != InstructionStopped {
			t.Fatalf("coordinator handoff: %+v", result)
		}
		result.More = 3
		result.Blocked = &BlockedView{State: "unrelated", Reason: "must not leak"}
		result.Stalled = []StalledView{{Name: "unrelated"}}
		got := RenderNext(result, "codex", func(string, ...string) string { return "unexpected extra command" })
		if got != want {
			t.Fatalf("whole output:\n%s\nwant:\n%s", got, want)
		}
		if result.Instructions[0].Submit != answer {
			t.Fatal("legitimate answer command missing")
		}
	}
	for _, r := range reservedRecords(t, j, run) {
		if r.Kind == JournalPaused || r.Kind == JournalGateAnswered {
			t.Fatalf("handoff mutated gate: %+v", r)
		}
	}
	if err := e.Answer(run, "reservedQuestion", "yes"); err != nil {
		t.Fatal(err)
	}
	after, err := e.HandOutAs(run, -1, Coordinator{})
	if err != nil || len(after) != 1 || after[0].Kind != InstructionEnded {
		t.Fatalf("answer did not continue run: %+v %v", after, err)
	}
}

func TestPolicyAnswerableGateKeepsCoordinatorInstructions(t *testing.T) {
	e, _, run := reservedGateFixture(t, harness.Caller{}, PolicyAnswerable)
	ins, err := e.HandOutAs(run, -1, Coordinator{})
	if err != nil || len(ins) != 1 || ins[0].Kind != InstructionHumanGate {
		t.Fatalf("policy gate changed: %+v %v", ins, err)
	}
	if !strings.Contains(ins[0].Submit, "answer") {
		t.Fatal("answer command missing")
	}
}

func TestReservedCoordinatorPreservesGateDeadline(t *testing.T) {
	e, _, run := reservedGateFixture(t, harness.Caller{}, Reserved, 15*time.Second)
	machine, err := e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := e.HandOutAs(run, 0, Coordinator{})
	if err != nil {
		t.Fatal(err)
	}
	if len(coordinator) != 1 || coordinator[0].Deadline.IsZero() || !coordinator[0].Deadline.Equal(machine[0].Deadline) || coordinator[0].RetryAfterSeconds != machine[0].RetryAfterSeconds || coordinator[0].Visit != machine[0].Visit {
		t.Fatalf("gate metadata lost: machine=%+v coordinator=%+v", machine, coordinator)
	}
}
