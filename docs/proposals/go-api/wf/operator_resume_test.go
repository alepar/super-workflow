package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A stop is cleared by an operator session that names itself and says why,
// whatever harness it runs in; the journal keeps who and why. The one
// refusal is a coordinator session of the run clearing its own stop. Nothing
// asks whether a person is at the terminal.
func TestAnOperatorSessionClearsAStopAndTheCoordinatorCannot(t *testing.T) {
	d := migDef(false)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunID: "op1"})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	if _, err := e.NextWait(run, 1, wf.Coordinator{Harness: "claude", Session: "coord-1"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "lane paused"); err != nil {
		t.Fatal(err)
	}
	before := len(h.records(run))

	err := e.ResumeAs(run, wf.Operator{Name: "coordinator", Reason: "carry on", Harness: "claude", Session: "coord-1"})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "its own stop") {
		t.Fatalf("the run's coordinator resuming = %v, want refused: a coordinator does not clear its own stop", err)
	}
	if err := e.ResumeAs(run, wf.Operator{Harness: "claude", Session: "sup-3"}); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "--operator") {
		t.Fatalf("an unnamed operator = %v, want refused naming --operator and --reason", err)
	}
	if n := len(h.records(run)); n != before {
		t.Fatalf("refused resumes appended %d records", n-before)
	}

	op := wf.Operator{Name: "the run supervisor", Reason: "settle repaired; lane resumes", Harness: "claude", Session: "sup-3"}
	if err := e.ResumeAs(run, op); err != nil {
		t.Fatalf("an operator session resuming: %v", err)
	}
	resumed := h.kind(run, wf.JournalResumed)
	var rd wf.ResumedData
	if len(resumed) != 1 || json.Unmarshal(resumed[0].Data, &rd) != nil || rd.Operator != op.Name || rd.Reason != op.Reason || rd.Session != op.Session {
		t.Fatalf("resumed = %+v, want one record naming operator %q, its session and why", resumed, op.Name)
	}
	if st, _ := e.Status(run); st.Stopped {
		t.Fatalf("status = %+v, want the stop cleared", st)
	}
}

// Review round 1 (operator resume): an operator in a harness must show its
// session, identity is (harness, session), and blank names and reasons do
// not count. A caller in a plain environment (no harness) is a generic
// operator: it can never be a coordinator session, which is always a
// harness session.
func TestAnOperatorIsIdentifiedBeforeItClearsAStop(t *testing.T) {
	d := migDef(false)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunID: "op2"})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	if _, err := e.NextWait(run, 1, wf.Coordinator{Harness: "claude", Session: "coord-1"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "lane paused"); err != nil {
		t.Fatal(err)
	}
	before := len(h.records(run))
	for _, tc := range []struct {
		name string
		op   wf.Operator
		want string
	}{
		{"a harness caller with no session", wf.Operator{Name: "sup", Reason: "go", Harness: "claude"}, "session"},
		{"no identity at all", wf.Operator{Name: "sup", Reason: "go"}, "session"},
		{"the coordinator's (harness, session)", wf.Operator{Name: "sup", Reason: "go", Harness: "claude", Session: "coord-1"}, "its own stop"},
		{"a blank name", wf.Operator{Name: "  ", Reason: "go", Harness: "claude", Session: "sup-3"}, "--operator"},
		{"a blank reason", wf.Operator{Name: "sup", Reason: "\t", Harness: "claude", Session: "sup-3"}, "--reason"},
	} {
		if err := e.ResumeAs(run, tc.op); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: resume = %v, want refused naming %q", tc.name, err, tc.want)
		}
	}
	if n := len(h.records(run)); n != before {
		t.Fatalf("refused resumes appended %d records", n-before)
	}
	// The same session ID in another harness is another session.
	if err := e.ResumeAs(run, wf.Operator{Name: "sup", Reason: "go", Harness: "codex", Session: "coord-1"}); err != nil {
		t.Fatalf("another harness's session of the same ID: %v", err)
	}
	var rd wf.ResumedData
	resumed := h.kind(run, wf.JournalResumed)
	if len(resumed) != 1 || json.Unmarshal(resumed[0].Data, &rd) != nil || rd.Harness != "codex" || rd.Session != "coord-1" {
		t.Fatalf("resumed = %+v, want the operator's harness and session", resumed)
	}
	if err := e.Stop(run, "again"); err != nil {
		t.Fatal(err)
	}
	if err := e.ResumeAs(run, wf.Operator{Name: "alex", Reason: "checked the lane", Harness: "generic"}); err != nil {
		t.Fatalf("a plain shell's operator: %v", err)
	}
}
