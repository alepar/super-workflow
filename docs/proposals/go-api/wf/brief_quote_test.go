package wf_test

import (
	"os"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// quoteHarness is a one-unit run whose definition describes work, with opts.
func quoteHarness(t *testing.T, work string, opts wf.EngineOptions) (*harness, wf.RunID) {
	t.Helper()
	d, _ := simple("brief-quote", wf.PromptText("Do the task."))
	d.Describe(work)
	h := newHarnessOpts(t, d, nil, opts)
	return h, h.run(d, nil)
}

func quoteHandOut(t *testing.T, h *harness, run wf.RunID, harnessName string) []wf.Instruction {
	t.Helper()
	ins, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: harnessName, Session: "coord"})
	if err != nil {
		t.Fatal(err)
	}
	return withoutEnded(ins)
}

// refusedHandOut hands out to pi and checks nothing was offered, no hand-out
// was journaled and the run halted with a reason holding every want.
func refusedHandOut(t *testing.T, h *harness, run wf.RunID, want ...string) {
	t.Helper()
	if ins := quoteHandOut(t, h, run, "pi"); len(ins) != 0 {
		t.Fatalf("a refused brief was offered: %+v", ins)
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != 0 {
		t.Fatalf("%d handed_out records journaled for a refused brief", n)
	}
	rs := refusedRecords(t, h, run)
	if len(rs) != 1 || !rs[0].Halt || len(rs[0].Reasons) != 1 {
		t.Fatalf("refused records = %+v, want one halt", rs)
	}
	for _, w := range append([]string{"hand-out of ", "single quote"}, want...) {
		if !strings.Contains(rs[0].Reasons[0], w) {
			t.Fatalf("halt reason %q does not hold %q", rs[0].Reasons[0], w)
		}
	}
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("run phase = %s, want blocked by the halt", st.Phase)
	}
}

func TestBriefRulesHoldNoQuote(t *testing.T) {
	if strings.Contains(wf.RulesForSpawnBrief, "'") {
		t.Fatalf("RulesForSpawnBrief holds a single quote: %s", wf.RulesForSpawnBrief)
	}
	for _, name := range []string{"claude", "pi"} {
		h, run := quoteHarness(t, "tidy the repo", wf.EngineOptions{Workspace: t.TempDir()})
		ins := quoteHandOut(t, h, run, name)
		if len(ins) != 1 || strings.Contains(ins[0].Brief, "'") {
			t.Fatalf("%s brief = %+v, want one offer without a single quote", name, ins)
		}
	}
}

func TestPiBriefRewritesProseApostrophes(t *testing.T) {
	const work = "don't touch the user's files"
	h, run := quoteHarness(t, work, wf.EngineOptions{})
	ins := quoteHandOut(t, h, run, "pi")
	if len(ins) != 1 || !strings.Contains(ins[0].Brief, "don’t touch the user’s files") || strings.Contains(ins[0].Brief, "'") {
		t.Fatalf("pi hand-out = %+v, want the work text with U+2019", ins)
	}
	h, run = quoteHarness(t, work, wf.EngineOptions{})
	ins = quoteHandOut(t, h, run, "claude")
	if len(ins) != 1 || !strings.Contains(ins[0].Brief, work) {
		t.Fatalf("claude hand-out = %+v, want the work text unchanged", ins)
	}
}

// The pi launch line carries its brief as a quoted here-document, so a single
// quote from the work text or the workspace path is offered, not refused.
func TestPiBriefCarriesAQuotedArgument(t *testing.T) {
	const work = "run grep 'x' on it"
	h, run := quoteHarness(t, work, wf.EngineOptions{})
	if ins := quoteHandOut(t, h, run, "pi"); len(ins) != 1 || !strings.Contains(ins[0].Brief, work) {
		t.Fatalf("pi hand-out = %+v, want offered with the work text", ins)
	}
}

func TestPiBriefCarriesAQuotedWorkspace(t *testing.T) {
	ws := t.TempDir() + "/it's"
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	h, run := quoteHarness(t, "tidy the repo", wf.EngineOptions{Workspace: ws})
	if ins := quoteHandOut(t, h, run, "pi"); len(ins) != 1 {
		t.Fatalf("pi hand-out = %+v, want the unit offered", ins)
	}
}

func verbLineOf(first string) func(wf.RunID, string, ...string) string {
	return func(run wf.RunID, verb string, args ...string) string {
		return strings.Join(append([]string{first, verb, string(run)}, args...), " ")
	}
}

func TestPiBriefAbsoluteBinaryPasses(t *testing.T) {
	h, run := quoteHarness(t, "tidy the repo", wf.EngineOptions{VerbLine: verbLineOf("/example/x/workflow")})
	if ins := quoteHandOut(t, h, run, "pi"); len(ins) != 1 {
		t.Fatalf("pi hand-out = %+v, want the unit offered", ins)
	}
}

func TestPiBriefQuotedBinaryPasses(t *testing.T) {
	h, run := quoteHarness(t, "tidy the repo", wf.EngineOptions{VerbLine: verbLineOf("'/example/x y/workflow'")})
	if ins := quoteHandOut(t, h, run, "pi"); len(ins) != 1 {
		t.Fatalf("pi hand-out = %+v, want the unit offered", ins)
	}
}

// A unit already handed out is not checked again: the refusal check runs only
// inside a hand-out, so a re-list of a handed unit is neither refused nor halted.
func TestRefusedBriefLeavesHandedUnitsAlone(t *testing.T) {
	d, _ := simple("brief-quote-two", wf.PromptText("Do the task."))
	d.Describe("run grep 'x' on it")
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{})
	run := h.run(d, nil)
	first := quoteHandOut(t, h, run, "claude")
	if len(first) != 1 {
		t.Fatalf("claude hand-out = %+v", first)
	}
	// The unit is handed to the claude session; a pi call from the same
	// session finds nothing pending to check.
	if ins := quoteHandOut(t, h, run, "claude"); len(ins) > 1 {
		t.Fatalf("re-list = %+v", ins)
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != 1 {
		t.Fatalf("handed_out records = %d, want the one from the claude hand-out", n)
	}
	if rs := refusedRecords(t, h, run); len(rs) != 0 {
		t.Fatalf("a claude hand-out was refused: %+v", rs)
	}
}
