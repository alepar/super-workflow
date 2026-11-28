package wf_test

import (
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// loseRunning journals attempt_lost for a unit that fetched its prompt, the way
// a lease expiry would, and returns the next hand-out.
func loseRunning(t *testing.T, h *harness, run wf.RunID, unit wf.Instruction) wf.Instruction {
	t.Helper()
	last := h.kind(run, wf.JournalPromptFetched)[0]
	rec := wf.JournalRecord{
		Kind: wf.JournalAttemptLost, Run: run, Instance: last.Instance, State: last.State,
		Attempt: last.Attempt, DefHash: last.DefHash, At: h.now,
		Data: mustJSON(t, wf.AttemptLostData{Name: unit.Name, Cause: "lease expired"}),
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(rec)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("HandOut after the loss = %+v, want attempt 2", again)
	}
	return again[0]
}

func fetchPrompt(t *testing.T, h *harness, run wf.RunID, ins wf.Instruction, agent string) string {
	t.Helper()
	if err := h.e.Started(run, ins.Name, claim(agent)); err != nil {
		t.Fatal(err)
	}
	text, err := h.e.Prompt(run, ins.Name)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestReissuedPromptNamesPredecessor(t *testing.T) {
	d := lsDef("pred-a", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, first := handOneUnit(t, h, d)
	if text := fetchPrompt(t, h, run, first, "a"); strings.Contains(text, "was lost") {
		t.Fatalf("first attempt prompt has a predecessor line:\n%s", text)
	}
	h.now = h.now.Add(time.Minute)
	second := loseRunning(t, h, run, first)

	text := afterRules(t, fetchPrompt(t, h, run, second, "b"))
	line, _, _ := strings.Cut(text, "\n")
	pre := "this is unit " + second.Name + "; the attempt before it, unit " + first.Name + ", was lost while running; its workspace "
	if !strings.HasPrefix(line, pre) || !strings.HasSuffix(line, " may hold partial work") {
		t.Fatalf("first line = %q, want %q<dir> may hold partial work", line, pre)
	}
	if !strings.Contains(text, "Say hello.") {
		t.Fatalf("the prompt text is gone:\n%s", text)
	}
}

func TestHandOutExpiryNoPredecessorLine(t *testing.T) {
	d := lsDef("pred-b", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, first := handOneUnit(t, h, d)
	h.now = h.now.Add(121 * time.Second)
	h.handOut(run, 0)
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Attempt != 1 || again[0].Name != first.Name {
		t.Fatalf("HandOut after the window = %+v, want %s offered again at attempt 1", again, first.Name)
	}
	if text := fetchPrompt(t, h, run, again[0], "b"); strings.Contains(text, "was lost") {
		t.Fatalf("a predecessor that never ran is named:\n%s", text)
	}
}

func TestFirstAttemptNoPredecessorLine(t *testing.T) {
	d := lsDef("pred-c", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, first := handOneUnit(t, h, d)
	text := afterRules(t, fetchPrompt(t, h, run, first, "a"))
	if !strings.HasPrefix(text, "Say hello.") {
		t.Fatalf("first attempt prompt = %q, want it to open with the prompt text", text)
	}
}

// afterRules is an issued prompt after the rules that open it; a prompt that
// does not open with them fails the test.
func afterRules(t *testing.T, prompt string) string {
	t.Helper()
	rules, body, ok := strings.Cut(prompt, "\n\n")
	if !ok || (rules != wf.RulesForAnyAgent && rules != trackerFreeAgentRules) {
		t.Fatalf("the prompt does not open with the rules: %q", prompt)
	}
	return body
}
