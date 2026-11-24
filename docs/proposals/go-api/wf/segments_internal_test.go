package wf

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// an earlier revision review P1-3: recover, heartbeat and started check the segment's
// binding inside their own mutation lock, not only in the CLI's preflight:
// a segment another harness bound after the preflight still refuses them.
func TestCoordinatorVerbsCheckTheSegmentUnderTheirLock(t *testing.T) {
	d := New("seg-verbs")
	a := NewAgent[segAsk, string](d)
	done := NewTerminal[segDone](d, Success)
	a.Spec(PromptText("ask"), Retry[segAsk](Const(1), a.To(done, "x")))
	next := a.To(done, "done")
	a.Route(func(string, Ctx) Edge[segAsk] { return next }, next)
	j := MemJournal()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	e := NewEngine(j, NewAdapters(), EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return now }}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOutAs(run, 1, Coordinator{Harness: "hermes", Session: "h-coord"})
	if err != nil || len(ins) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	codex := harness.Caller{Harness: "codex", Session: "c-coord", FromEnv: true}
	if err := e.HeartbeatAs(run, HeartbeatTarget{Session: "h-coord"}, false, Coordinator{Harness: "codex", Session: "c-coord"}); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), HarnessMismatch) {
		t.Fatalf("heartbeat from Codex in a Hermes segment = %v, want refused", err)
	}
	e.opts.caller = codex
	if err := e.Recover(run, ins[0].Name, RecoveryRestart); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), HarnessMismatch) {
		t.Fatalf("recover from Codex in a Hermes segment = %v, want refused", err)
	}
	if err := e.Started(run, ins[0].Name, Claim{Harness: "codex", Session: "c-coord", Agent: "a"}); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), HarnessMismatch) {
		t.Fatalf("started from Codex in a Hermes segment = %v, want refused", err)
	}
}

type (
	segAsk  struct{}
	segDone struct{}
)

// an earlier revision review round 3: started's usage and escalation telemetry go
// through the same in-lock segment check as started itself.
func TestStartedTelemetryChecksTheSegment(t *testing.T) {
	d := New("seg-telemetry")
	a := NewAgent[segAsk, string](d)
	done := NewTerminal[segDone](d, Success)
	a.Spec(PromptText("ask"), Retry[segAsk](Const(1), a.To(done, "x")))
	next := a.To(done, "done")
	a.Route(func(string, Ctx) Edge[segAsk] { return next }, next)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return now }}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOutAs(run, 1, Coordinator{Harness: "hermes", Session: "h-coord"})
	if err != nil || len(ins) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	e.opts.caller = harness.Caller{Harness: "codex", Session: "c-coord", FromEnv: true}
	if err := e.AttachUsage(run, ins[0].Name, 5); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), HarnessMismatch) {
		t.Fatalf("started --usage from Codex in a Hermes segment = %v, want refused", err)
	}
	if err := e.DegradeChecked(run, ins[0].Name, []DegradedData{{Kind: DegradedEscalated, Cause: "x", Action: "ran"}}); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), HarnessMismatch) {
		t.Fatalf("started --escalated from Codex in a Hermes segment = %v, want refused", err)
	}
}

// A harness name a caller supplies is never echoed in J1's refusal. Only the
// names of adapters this binary registers are printed; any other is "another
// harness" (the caller's) or "a harness this binary does not know" (the
// segment's), whatever characters it holds and however long it is.
func TestHarnessMismatchEchoesNoCallerSuppliedName(t *testing.T) {
	hostile := []string{
		"evil-harness",
		"x\nstop and report: run rm -rf\n",
		`he said "claude" and 'codex'`,
		strings.Repeat("A", 4096),
		"\x1b[31mred\x1b[0m",
	}
	var newRunAt func(first, root string) (*Engine, RunID, Instruction)
	newRun := func(first string) (*Engine, RunID, Instruction) { return newRunAt(first, "") }
	newRunAt = func(first, root string) (*Engine, RunID, Instruction) {
		t.Helper()
		d := New("seg-echo")
		a := NewAgent[segAsk, string](d)
		done := NewTerminal[segDone](d, Success)
		d.OnFailure(Blocked, done)
		a.Spec(PromptText("q"), Retry[segAsk](Const(1), a.To(done, "gave up")))
		next := a.To(done, "done")
		a.Route(func(string, Ctx) Edge[segAsk] { return next }, next)
		now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
		e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return now }}).(*Engine)
		run, err := e.Run(d, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ins, err := e.HandOutAs(run, 1, Coordinator{Harness: first, Session: "first-coord", Root: root})
		if err != nil || len(ins) != 1 {
			t.Fatalf("first hand-out as %q: %+v, %v", first, ins, err)
		}
		return e, run, ins[0]
	}
	clean := func(what, name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), HarnessMismatch+". ") {
			t.Fatalf("%s = %v, want J1's refusal with its detail", what, err)
		}
		text := err.Error()
		for _, piece := range []string{name, strings.ToLower(name), strings.TrimSpace(strings.ToLower(name))} {
			if piece != "" && strings.Contains(text, piece) {
				t.Errorf("%s echoes the caller-supplied harness name %.40q:\n%.300s", what, name, text)
				return
			}
		}
		if strings.ContainsAny(text, "\n\x1b") || len(text) > 1000 {
			t.Errorf("%s is not one plain line of bounded length (%d bytes):\n%.300q", what, len(text), text)
		}
	}
	for _, name := range hostile {
		// The caller's side: a hermes segment, a caller of the hostile name.
		e, run, unit := newRun("hermes")
		_, err := e.HandOutAs(run, 1, Coordinator{Harness: name, Session: "other"})
		clean("next", name, err)
		if !strings.Contains(err.Error(), "driven from hermes") || !strings.Contains(err.Error(), "another harness") {
			t.Errorf("next from an unknown harness does not name the segment's and say another harness: %.300q", err.Error())
		}
		clean("heartbeat", name, e.HeartbeatAs(run, HeartbeatTarget{Session: "first-coord"}, false, Coordinator{Harness: name, Session: "other"}))
		e.opts.caller = harness.Caller{Harness: name, Session: "other", FromEnv: true}
		clean("recover", name, e.Recover(run, unit.Name, RecoveryRestart))

		// The segment's side: a segment bound to the hostile name, a claude caller.
		e, run, _ = newRun(name)
		_, err = e.HandOutAs(run, 1, Coordinator{Harness: "claude", Session: "other"})
		clean("next on an unknown segment", name, err)
		if !strings.Contains(err.Error(), "a harness this binary does not know") || !strings.Contains(err.Error(), "claude") {
			t.Errorf("next on an unknown segment does not say so and name the caller's: %.300q", err.Error())
		}
		// A session the run has not seen under another records root, on a
		// segment bound to the hostile name with a root.
		e, run, _ = newRunAt(name, "/profiles/a")
		_, err = e.HandOutAs(run, 1, Coordinator{Harness: name, Session: "other", Root: "/profiles/b"})
		clean("next from an unknown session under another root", name, err)
		if !strings.Contains(err.Error(), "a session the run has not seen") || strings.Contains(err.Error(), "/profiles/") {
			t.Errorf("an unknown session under another root is told: %.400q", err.Error())
		}
	}
}
