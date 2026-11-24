package wf_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// D1 of the loud-failures plan: each sandbox escalation a child passes to done
// or failed, or the coordinator to started, is journaled as a degraded record
// of kind escalated, is folded again on every later command (the replay), and
// is listed by status, once each, including one sent after the run ended.
func TestEscalationsAreDegradedRecords(t *testing.T) {
	def := plainAgentDef("degraded-escalated")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--session", "s", "--agent", "a", "--state-dir", state)
	for _, bad := range []string{"", "go test", ": : ", "go test: no reason"} {
		if code, _, er := singleCLI(def, "wf", "done", in.Name, "--escalated", bad, "--state-dir", state); code != 2 {
			t.Errorf("--escalated %q exited %d, want 2: %s", bad, code, er)
		}
	}
	// The coordinator's own escalations for the unit, while the run is live.
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--state-dir", state,
		"--escalated", "git fetch origin: .git is read-only: refused",
		"--escalated", "bd sync: dolt remote unreachable: ran")
	if code, _, _ := singleCLI(def, "wf", "dev", "started", "no-such-unit", "--escalated", "a: b: c", "--state-dir", state); code == 0 {
		t.Error("an escalation for an unknown unit was accepted")
	}
	// The child's, with a repeat of one already journaled: it is not recorded twice.
	writeAllocated(t, in.Output, `{"OK":true}`)
	mustSingle(t, def, "wf", "done", in.Name, "--state-dir", state,
		"--escalated", "go test ./...: no network in the sandbox: approved, ran",
		"--escalated", "git fetch origin: .git is read-only: refused")
	// The run has ended with that done; the coordinator's escalation for its
	// last unit still lands, beside its usage.
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--usage", "tokens=5",
		"--escalated", "go mod download: proxy.golang.org unreachable: refused by reviewer", "--state-dir", state)

	want := []wf.DegradedData{
		{Unit: in.Name, Kind: wf.DegradedEscalated, Cause: "git fetch origin: .git is read-only", Action: "refused"},
		{Unit: in.Name, Kind: wf.DegradedEscalated, Cause: "bd sync: dolt remote unreachable", Action: "ran"},
		{Unit: in.Name, Kind: wf.DegradedEscalated, Cause: "go test ./...: no network in the sandbox", Action: "approved, ran"},
		{Unit: in.Name, Kind: wf.DegradedEscalated, Cause: "go mod download: proxy.golang.org unreachable", Action: "refused by reviewer"},
	}
	var recs []wf.DegradedData
	for _, line := range strings.Split(strings.TrimSpace(mustSingle(t, def, "wf", "journal", "--state-dir", state)), "\n") {
		var r struct {
			Kind wf.JournalKind
			Data wf.DegradedData
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("journal line %q: %v", line, err)
		}
		if r.Kind == wf.JournalDegraded {
			recs = append(recs, r.Data)
		}
	}
	if !slices.Equal(recs, want) {
		t.Errorf("journaled degraded records = %+v, want %+v", recs, want)
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)), &st); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Degraded, want) {
		t.Errorf("status Degraded = %+v, want %+v", st.Degraded, want)
	}
	if st.Phase != wf.RunEnded || st.Tokens != 5 {
		t.Errorf("status phase %s, tokens %d; want the ended run with the attached usage 5", st.Phase, st.Tokens)
	}
	text := mustSingle(t, def, "wf", "status", "--state-dir", state)
	if line := "degraded: " + in.Name + ` escalated: "git fetch origin: .git is read-only": "refused"`; !strings.Contains(text, line) {
		t.Errorf("status lacks %q:\n%s", line, text)
	}
}

// A blocked failure carries the escalation that was refused.
func TestFailedCarriesEscalations(t *testing.T) {
	def := plainAgentDef("degraded-failed")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--session", "s", "--agent", "a", "--state-dir", state)
	mustSingle(t, def, "wf", "failed", in.Name, "--class", "blocked", "--reason", "BLOCKED_AUTH: curl refused",
		"--escalated", "curl https://proxy.golang.org: no network: refused", "--state-dir", state)
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)), &st); err != nil {
		t.Fatal(err)
	}
	want := []wf.DegradedData{{Unit: in.Name, Kind: wf.DegradedEscalated, Cause: "curl https://proxy.golang.org: no network", Action: "refused"}}
	if !slices.Equal(st.Degraded, want) {
		t.Errorf("status Degraded = %+v, want %+v", st.Degraded, want)
	}
}
