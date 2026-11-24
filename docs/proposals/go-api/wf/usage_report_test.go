package wf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Rule 5 step 2: a usage number never blocks a hand-back. A done whose
// --usage is not tokens=N is accepted, counts zero, and leaves an unparsed
// usage note.
func TestMalformedDoneUsageIsANoteNotARefusal(t *testing.T) {
	def := plainAgentDef("usage-malformed")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--session", "s", "--agent", "a", "--state-dir", state)
	writeAllocated(t, in.Output, `{"OK":true}`)
	code, _, er := singleCLI(def, "wf", "done", in.Name, "--usage", "about 12k", "--state-dir", state)
	if code != 0 {
		t.Fatalf("done with a malformed --usage exited %d: %s", code, er)
	}
	if !strings.Contains(er, "--usage") {
		t.Errorf("done printed no warning about the ignored --usage: %q", er)
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)), &st); err != nil {
		t.Fatal(err)
	}
	if st.Tokens != 0 {
		t.Errorf("status tokens %d, want 0", st.Tokens)
	}
	found := false
	for _, d := range st.Degraded {
		if d.Kind == wf.DegradedUsageUnparsed && d.Unit == in.Name && strings.Contains(d.Cause, "about 12k") {
			found = true
		}
	}
	if !found {
		t.Errorf("no unparsed usage note for %s in %+v", in.Name, st.Degraded)
	}
}

// The child is told how to report usage, on done only and only when its
// harness shows the count: in the body's finishing rules, which the page
// (held under 2 KB) tells it to read in full.
func TestPromptAsksForUsageOnDoneWhenTheHarnessShowsIt(t *testing.T) {
	def := plainAgentDef("usage-guidance")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	page := mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, in), "--state-dir", state)
	text := withBody(t, page)
	_, finishing, _ := strings.Cut(text, "## Finishing")
	if !strings.Contains(finishing, "--usage tokens=N") || !strings.Contains(finishing, "otherwise omit it") {
		t.Errorf("the finishing rules do not ask for --usage tokens=N on done when the harness shows it:\n%s", text)
	}
}
