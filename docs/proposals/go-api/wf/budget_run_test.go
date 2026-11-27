package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

var budgetOperator = wf.Operator{Name: "operator", Reason: "raise the budget", Harness: "generic"}

// resumeWith resumes run as an operator under an engine whose resume --budget
// is ch.
func resumeWith(h *harness, d *wf.Def, run wf.RunID, ch *wf.BudgetChange) error {
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, ResumeBudget: ch, Clock: func() time.Time { return h.now }}).(*wf.Engine)
	return e.ResumeAs(run, budgetOperator)
}

func mustBudget(t *testing.T, s string) *wf.BudgetChange {
	t.Helper()
	ch, err := wf.ParseBudget(s)
	if err != nil {
		t.Fatal(err)
	}
	return &ch
}

// Each limit stops the run at its next agent instruction, and the stop is a
// stop, not an end: the journal has no run_ended and status says why.
func TestRunBudgetStopsAtEachLimit(t *testing.T) {
	cases := []struct {
		name   string
		budget wf.RunBudget
		spend  func(h *harness)
		tokens int
		want   string
	}{
		{"turns", wf.RunBudget{Turns: 1}, nil, 0, "run budget spent: 1 turns"},
		{"tokens", wf.RunBudget{Tokens: 50}, nil, 60, "run budget spent: 60 tokens"},
		{"wall", wf.RunBudget{Wall: time.Minute}, func(h *harness) { h.now = h.now.Add(2 * time.Minute) }, 0, "run budget spent: 1m0s wall time"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := rsChainDef()
			h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: c.budget})
			run := h.run(d, nil)
			in := h.next(run)[0]
			if c.spend != nil {
				c.spend(h)
			}
			if _, err := h.e.Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(`{"V":"ok"}`), Tokens: c.tokens}); err != nil {
				t.Fatal(err)
			}
			if ins := h.next(run); len(ins) != 0 {
				t.Fatalf("instructions = %+v, want none: the budget is spent", ins)
			}
			k := kinds(h.records(run))
			if k[len(k)-1] != "budget_stopped" || len(h.kind(run, wf.JournalRunEnded)) != 0 {
				t.Fatalf("journal tail = %q, want budget_stopped and no run_ended", k[max(0, len(k)-3):])
			}
			st := h.status(run)
			if st.Phase == wf.RunEnded || !st.Stopped || st.BudgetStop != c.want || st.Budget != c.budget {
				t.Fatalf("status phase=%s stopped=%v budget stop=%q budget=%v, want stopped on %q", st.Phase, st.Stopped, st.BudgetStop, st.Budget, c.want)
			}
			// Status and next say the run is stopped on its budget, name the
			// spent limit and print the operator's exact resume command.
			limit := c.name + "=<above "
			if !strings.Contains(st.BudgetResume, " resume "+string(run)+" --operator <name> --reason <why> --budget "+limit) {
				t.Fatalf("budget resume = %q, want the resume command raising %s", st.BudgetResume, c.name)
			}
			if !strings.Contains(st.NothingReadyReason, c.want) || !strings.Contains(st.NothingReadyReason, st.BudgetResume) {
				t.Fatalf("nothing-ready reason = %q, want the spent limit and the resume command", st.NothingReadyReason)
			}
			if text := wf.RenderStatus(st); !strings.Contains(text, "stopped on its run budget, not ended: "+c.want) || !strings.Contains(text, st.BudgetResume) {
				t.Fatalf("status text:\n%s\nwant the stop and the resume command", text)
			}
			res, err := h.e.(*wf.Engine).NextWait(run, 1, wf.Coordinator{Harness: "claude", Session: "coord-1"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if ins := res.Instructions; len(ins) != 1 || ins[0].Kind != wf.InstructionStopped || !strings.Contains(ins[0].Prompt, c.want) || !strings.Contains(ins[0].Prompt, st.BudgetResume) || !strings.Contains(ins[0].Prompt, "not ended") {
				t.Fatalf("next = %+v, want the stopped instruction naming the limit and the resume command", ins)
			}
		})
	}
}

// A resume raises the budget and the run carries on from where it stopped; a
// resume that leaves the spent limit spent is refused and journals nothing.
func TestResumeRaisesRunBudget(t *testing.T) {
	d := rsChainDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1, Tokens: 1000}})
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{"V":"ok"}`)
	h.next(run)
	before := len(h.records(run))
	for _, ch := range []*wf.BudgetChange{nil, mustBudget(t, "turns=1"), mustBudget(t, "tokens=5000")} {
		if err := resumeWith(h, d, run, ch); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("resume with %+v: err = %v, want a refusal: the turn limit stays spent", ch, err)
		}
	}
	if after := len(h.records(run)); after != before {
		t.Fatalf("refused resumes appended %d records", after-before)
	}
	if err := resumeWith(h, d, run, mustBudget(t, "turns=2")); err != nil {
		t.Fatal(err)
	}
	resumed := h.kind(run, wf.JournalResumed)
	var rd wf.ResumedData
	if len(resumed) != 1 || json.Unmarshal(resumed[0].Data, &rd) != nil || rd.Budget == nil || *rd.Budget != (wf.RunBudget{Turns: 2, Tokens: 1000}) {
		t.Fatalf("resumed = %+v, want one record with budget turns=2 tokens=1000 (the token limit kept)", rd)
	}
	ins := h.next(run)
	if len(ins) != 1 || ins[0].State != "rsSecond" {
		t.Fatalf("instructions after the raise = %+v, want the second step", ins)
	}
	h.submit(run, ins[0], `{"V":"ok"}`)
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "rsDone")
}

// A running run's budget changes only when a resume clears a stop.
func TestResumeBudgetNeedsAStop(t *testing.T) {
	d := rsChainDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if err := resumeWith(h, d, run, mustBudget(t, "turns=9")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("err = %v, want a refusal: the run is not stopped", err)
	}
	if err := h.e.(*wf.Engine).Stop(run, "operator wants a budget"); err != nil {
		t.Fatal(err)
	}
	if err := resumeWith(h, d, run, mustBudget(t, "turns=9")); err != nil {
		t.Fatal(err)
	}
	if st := h.status(run); st.Budget != (wf.RunBudget{Turns: 9}) || st.Stopped {
		t.Fatalf("status budget=%v stopped=%v, want turns=9 and running", st.Budget, st.Stopped)
	}
}

// A journal an earlier binary wrote, whose budget stop it ended with
// run_ended, stays ended.
func TestEndedBudgetJournalStaysEnded(t *testing.T) {
	d := rsChainDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{"V":"ok"}`)
	h.next(run)
	recs := h.records(run)
	last := recs[len(recs)-1]
	raw, err := json.Marshal(wf.RunEndedData{Outcome: wf.Stopped})
	if err != nil {
		t.Fatal(err)
	}
	ended := wf.JournalRecord{Run: run, Seq: last.Seq + 1, At: last.At, Kind: wf.JournalRunEnded, State: last.State, DefHash: last.DefHash, Data: raw}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(ended)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	st := h.status(run)
	if st.Phase != wf.RunEnded || st.Outcome != wf.Stopped || st.Stopped {
		t.Fatalf("status phase=%s outcome=%v stopped=%v, want ended Stopped", st.Phase, st.Outcome, st.Stopped)
	}
	if err := resumeWith(h, d, run, mustBudget(t, "turns=5")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("err = %v, want a refusal: the run has ended", err)
	}
	if n := len(h.kind(run, wf.JournalResumed)); n != 0 || h.status(run).Phase != wf.RunEnded {
		t.Fatalf("resumed records = %d, phase %s: an ended run was resumed", n, h.status(run).Phase)
	}
}

func TestParseBudget(t *testing.T) {
	ch, err := wf.ParseBudget("turns=40, tokens=2000000,wall=90m")
	if err != nil {
		t.Fatal(err)
	}
	if got := ch.Over(wf.RunBudget{Turns: 1, Tokens: 1, Wall: time.Second}); got != (wf.RunBudget{Turns: 40, Tokens: 2000000, Wall: 90 * time.Minute}) {
		t.Fatalf("Over = %+v", got)
	}
	ch, err = wf.ParseBudget("tokens=0")
	if err != nil {
		t.Fatal(err)
	}
	if got := ch.Over(wf.RunBudget{Turns: 3, Tokens: 7}); got != (wf.RunBudget{Turns: 3}) {
		t.Fatalf("tokens=0 over turns=3,tokens=7 = %+v, want the token limit dropped and turns kept", got)
	}
	if ch, err := wf.ParseBudget(""); err != nil || ch.Over(wf.RunBudget{Turns: 2}) != (wf.RunBudget{Turns: 2}) {
		t.Fatalf("empty: %+v, %v", ch, err)
	}
	for _, bad := range []string{"turns", "turns=", "turns=-1", "turns=x", "tokens=1.5", "wall=10", "wall=-1m", "turns=1,turns=2", "cost=5", "turns=1,,wall=1h"} {
		if _, err := wf.ParseBudget(bad); err == nil {
			t.Errorf("ParseBudget(%q) succeeded, want an error", bad)
		}
	}
	if s := (wf.RunBudget{Turns: 4, Wall: time.Hour}).String(); s != "turns=4,wall=1h0m0s" {
		t.Errorf("String = %q", s)
	}
}

// A stop that reached the run through a Sub child: the raise readies the
// child and its parent again, and the child finishes its steps.
func TestResumeRaisesSubChainBudget(t *testing.T) {
	d := subDef("sub-budget-raise", twoStepChild())
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`)
	h.next(run)
	if st := h.status(run); !st.Stopped || st.BudgetStop == "" {
		t.Fatalf("status = %+v, want stopped on the budget", st)
	}
	if err := resumeWith(h, d, run, mustBudget(t, "turns=0")); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		ins := h.next(run)
		if len(ins) == 0 {
			break
		}
		for _, in := range ins {
			h.submit(run, in, `{}`)
		}
	}
	if st := h.status(run); st.Phase != wf.RunEnded || !st.Outcome.Delivered() {
		t.Fatalf("status phase=%s outcome=%v, want the run delivered after the raise", st.Phase, st.Outcome)
	}
}

// The run's coordinator never raises its own budget: its resume is refused
// with or without --budget, and the coordinator-facing verbs take no
// --budget at all.
func TestCoordinatorCannotRaiseItsBudget(t *testing.T) {
	d := rsChainDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunID: "cap1", RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	h.submit(run, h.next(run)[0], `{"V":"ok"}`)
	h.next(run)
	// The coordinator that drives the stopped run is told it is stopped.
	res, err := e.NextWait(run, 1, wf.Coordinator{Harness: "claude", Session: "coord-1"}, 0)
	if err != nil || len(res.Instructions) != 1 || res.Instructions[0].Kind != wf.InstructionStopped {
		t.Fatalf("next = %+v, %v, want the stopped instruction", res, err)
	}
	before := len(h.records(run))
	self := wf.Operator{Name: "coordinator", Reason: "need more turns", Harness: "claude", Session: "coord-1"}
	for _, ch := range []*wf.BudgetChange{nil, mustBudget(t, "turns=10")} {
		e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, ResumeBudget: ch, Clock: func() time.Time { return h.now }}).(*wf.Engine)
		if err := e2.ResumeAs(run, self); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "its own stop") {
			t.Fatalf("coordinator resume with %+v = %v, want refused: a coordinator does not clear its own stop", ch, err)
		}
	}
	if n := len(h.records(run)); n != before {
		t.Fatalf("refused resumes appended %d records", n-before)
	}
	for _, verb := range []string{"next", "done", "failed", "prompt", "status", "answer", "settle", "retry"} {
		var out, er bytes.Buffer
		if code := fixtureCLI(wf.MainConfig{Defs: []wf.Registration{{Def: d}}}, []string{verb, "cap1", "--budget", "turns=10"}, &out, &er); code != 2 || !strings.Contains(er.String(), "flag provided but not defined: -budget") {
			t.Errorf("%s --budget: exit %d, %s; want a usage error: only run and resume take --budget", verb, code, er.String())
		}
	}
}
