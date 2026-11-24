package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A child whose prompt page 1 bound its claim holds the unit. When a requested
// page is out of range, the tail tells it to run failed with what it needs,
// so the run routes the unit: it is not told to exit with the unit claimed.
// A first fetch that bound nothing (another child owns the unit) is still
// told to tell its coordinator and end.
func TestAPageFailureOnAHeldClaimIsRoutedWithFailed(t *testing.T) {
	strictCallers(t)
	def := fanDef("held-claim", chItems(1), 1, wf.AllSettled(), func(m *wf.Map[chFan, chItem, string, chRes]) {})
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1", "--paged-prompts")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next: %v %v", ins, err)
	}
	claim := claimToken(t, ins[0])
	writeCodexChild(t, os.Getenv("CODEX_HOME"), "boss", "kid", claim)
	t.Setenv("CODEX_THREAD_ID", "kid")
	const failedTail = "run failed with the same --claim and --reason saying what blocked you and what you need, so the run can route it."
	const exitTail = "end only your child task"

	// Before any fetch nothing is bound: a refused prompt ends the child.
	code, _, er := singleCLI(def, "wf", "prompt", "--claim", claim, "--page", "999", "--state-dir", state)
	if code != 4 || !strings.Contains(er, "page must be between") || !strings.Contains(er, exitTail) || strings.Contains(er, failedTail) {
		t.Errorf("a refused fetch that bound nothing: exit %d, %q, want the exit tail", code, er)
	}
	if code, _, er := singleCLI(def, "wf", "prompt", "--claim", claim, "--state-dir", state); code != 0 {
		t.Fatalf("page 1: exit %d, %s", code, er)
	}
	// Page 1 bound the claim: an out-of-range page failure is routed.
	for _, verb := range [][]string{
		{"wf", "prompt", "--claim", claim, "--page", "999", "--state-dir", state},
		{"wf", "read", "nosuchref", "--claim", claim, "--state-dir", state},
	} {
		code, _, er = singleCLI(def, verb...)
		if code != 4 || !strings.Contains(er, failedTail) || strings.Contains(er, exitTail) {
			t.Errorf("%s on a held claim: exit %d, %q, want the failed tail", verb[1], code, er)
		}
	}
	if code, _, er := singleCLI(def, "wf", "failed", "--claim", claim, "--reason", "requested page is out of range", "--state-dir", state); code != 0 {
		t.Errorf("failed on the held claim: exit %d, %s", code, er)
	}
}

// The finishing rules and the CLI's tail agree about a spent attempt: the run
// has recorded it, so the child does not run failed; it tells its coordinator.
// (The short child page says the same: TestBusyRunExitsNineAndChildRulesSayRunItAgain.)
func TestTheFinishingRulesAgreeWithTheTailOnASpentAttempt(t *testing.T) {
	page := childPrompt(t)
	body := withBody(t, page)
	for _, want := range []string{
		"When done says this attempt is spent, the run has recorded it: do not run failed; tell your coordinator the output and end your task.",
		"On any other non-zero exit of done, run the failed command above with --reason saying what blocked you and what you need",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the finishing rules lack %q:\n%s", want, body)
		}
	}
}
