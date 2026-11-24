package wf

import (
	"encoding/json"
	"strings"
	"testing"
)

func encHistory(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// prose is agent text that cites the source workspace (roast judge
// evidence; SweepFix's Tests[0].Evidence).
var prose = map[string]any{"Tests": []any{map[string]any{"Evidence": "/src/ws/x_test.go:17"}}}

// An agent's submission and the acceptance that decides it are history for
// relocation, and so are the values a definition derives from them: a Map's
// child_spawned Input and child_settled Result, a Compute acceptance
// (an earlier revision; b3 3622..3769). The bind-time source-root refusal guards their
// use.
func TestAgentResultsAndTheirCarriersAreHistory(t *testing.T) {
	src := PathRoots{Workspace: "/src/ws", State: "/src/state"}
	recs := []JournalRecord{
		{Seq: 1, Kind: JournalSubmitted, Instance: "c1", State: "Judge", Data: encHistory(t, SubmittedData{Token: "t", Output: encHistory(t, prose)})},
		{Seq: 2, Kind: JournalAccepted, Instance: "c1", State: "Judge", Data: encHistory(t, AcceptedData{Output: encHistory(t, prose)})},
		{Seq: 3, Kind: JournalChildSettled, State: "Seats", Data: encHistory(t, ChildSettledData{ID: "c1", Result: encHistory(t, prose)})},
		{Seq: 4, Kind: JournalAccepted, State: "ReporterInputs", Data: encHistory(t, AcceptedData{Output: encHistory(t, prose), Computed: true})},
		{Seq: 5, Kind: JournalChildSpawned, State: "Judges", Data: encHistory(t, ChildSpawnedData{ID: "c2", Input: encHistory(t, prose)})},
	}
	if err := CheckJournalRelocation(recs, src); err != nil {
		t.Fatalf("history naming the source workspace: %v", err)
	}
}

// An agent acceptance with no submission deciding it (a migrated or reused
// state name, a hand-written record) is not an agent's history: checked as
// any output is.
func TestAgentAcceptanceIsPairedWithItsSubmission(t *testing.T) {
	src := PathRoots{Workspace: "/src/ws", State: "/src/state"}
	lone := []JournalRecord{{Seq: 1, Kind: JournalAccepted, Instance: "c1", State: "Judge", Data: encHistory(t, AcceptedData{Output: encHistory(t, prose)})}}
	if err := CheckJournalRelocation(lone, src); err == nil {
		t.Fatal("an acceptance with no submission was taken for an agent's")
	}
	other := []JournalRecord{
		{Seq: 1, Kind: JournalSubmitted, Instance: "c1", State: "Judge", Data: encHistory(t, SubmittedData{Token: "t", Output: encHistory(t, "x")})},
		{Seq: 2, Kind: JournalTransition, Instance: "c1", State: "Judge", Data: encHistory(t, map[string]any{})},
		{Seq: 3, Kind: JournalAccepted, Instance: "c1", State: "Judge", Data: encHistory(t, AcceptedData{Output: encHistory(t, prose)})},
	}
	if err := CheckJournalRelocation(other, src); err == nil {
		t.Fatal("an acceptance after the instance moved on was paired with an old submission")
	}
}

// Typed path fields in history stay checked at fork time: an external path
// marker naming the source workspace is refused.
func TestTypedPathsInHistoryStayChecked(t *testing.T) {
	src := PathRoots{Workspace: "/src/ws", State: "/src/state"}
	typed := map[string]any{"Report": ownedPath{Base: externalPathBase, Path: "/src/ws/report.md"}.value()}
	recs := []JournalRecord{
		{Seq: 1, Kind: JournalSubmitted, Instance: "c1", State: "Judge", Data: encHistory(t, SubmittedData{Token: "t", Output: encHistory(t, typed)})},
	}
	err := CheckJournalRelocation(recs, src)
	if err == nil || !strings.Contains(err.Error(), "Output.Report") {
		t.Fatalf("a typed external path under the source: %v", err)
	}
}
