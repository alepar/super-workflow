package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestCheckJournalRelocationReportsFieldAndPreservesRecords(t *testing.T) {
	roots := wf.PathRoots{Workspace: "/source/work", State: "/source/state"}
	for _, tc := range []struct {
		kind        wf.JournalKind
		data, field string
	}{
		{wf.JournalCleanupDeferred, `{"Effect":{"Args":{"path":"/source/work/tree"}}}`, "Effect.Args.path"},
		{wf.JournalRunStarted, `{"Options":{"tool":"/source/work/bin/super"}}`, "Options.tool"},
		{wf.JournalRunStarted, `{"Params":{"tool":"/source//work/bin/super"}}`, "Params.tool"}, // a spelling the fork's rewrite cannot map
		{wf.JournalAccepted, `{"Output":{"files":["/source/state/spec.md"]}}`, "Output.files[0]"},
		{wf.JournalChildSpawned, `{"Input":{"spec":{"$wf.path":{"base":"external","path":"/source/state/spec.md"}}}}`, "Input.spec"},
		{wf.JournalEffectIntent, `{"Command":true,"Effect":{"Args":{"dir":"/source/work","argv":["/source/work/bin/super"]}}}`, "Effect.Args.argv[0]"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			records := []wf.JournalRecord{{Seq: 7, Kind: tc.kind, Data: json.RawMessage(tc.data)}}
			err := wf.CheckJournalRelocation(records, roots)
			if !errors.Is(err, wf.ErrUnavailable) || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "journal 7") {
				t.Fatalf("wrong refusal: %v", err)
			}
			if string(records[0].Data) != tc.data {
				t.Fatal("preflight rewrote journal")
			}
		})
	}
	records := []wf.JournalRecord{{Seq: 1, Kind: wf.JournalEffectIntent, Data: json.RawMessage(`{"Command":true,"Effect":{"Args":{"dir":"/source/work","argv":["/external/super"]}}}`)}}
	if err := wf.CheckJournalRelocation(records, roots); err != nil {
		t.Fatalf("owned command cwd or external tool rejected: %v", err)
	}
	// an earlier revision: an untyped param naming a source root as a path is the
	// fork's to rewrite, not a refusal.
	records = []wf.JournalRecord{{Seq: 1, Kind: wf.JournalRunStarted, Data: json.RawMessage(`{"Params":{"handoff":"cat > /source/work/.superpowers/finish-handoff.md"}}`)}}
	if err := wf.CheckJournalRelocation(records, roots); err != nil {
		t.Fatalf("relocatable untyped param rejected: %v", err)
	}
}

func TestCheckJournalRelocationUsesRecordedRoots(t *testing.T) {
	old := wf.PathRoots{Workspace: "/old/work", State: "/old/state"}
	records := []wf.JournalRecord{{Seq: 1, Kind: wf.JournalRunStarted, Roots: &old, Data: json.RawMessage(`{"Options":{"spec":"/old/state/spec.md"}}`)}}
	err := wf.CheckJournalRelocation(records, wf.PathRoots{Workspace: "/new/work", State: "/new/state"})
	if err == nil || !strings.Contains(err.Error(), "Options.spec") {
		t.Fatalf("missed old root: %v", err)
	}
	// an earlier revision: a moved run's untyped param naming its recorded root is the
	// fork's to rewrite from that root too.
	records[0].Data = json.RawMessage(`{"Params":{"spec":"/old/state/spec.md"}}`)
	if err := wf.CheckJournalRelocation(records, wf.PathRoots{Workspace: "/new/work", State: "/new/state"}); err != nil {
		t.Fatalf("param under the recorded root refused: %v", err)
	}
	records[0].Data = json.RawMessage(`{"Options":{"spec":{"$wf.runPath":"spec.md"}}}`)
	if err := wf.CheckJournalRelocation(records, old); err != nil {
		t.Fatalf("typed path rejected: %v", err)
	}
}

func TestCheckJournalRelocationOwnedPathIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, marker string
		bad          bool
	}{
		{"workspace", `{"base":"workspace","path":"worktree"}`, false},
		{"run", `{"base":"run","path":"spec.md"}`, false},
		{"external", `{"base":"external","path":"/external/tool"}`, false},
		{"mislabeled-external", `{"base":"external","path":"/source/work/tool"}`, true},
		{"malformed", `{"base":"workspace","path":"../escape"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := wf.JournalRecord{Seq: 9, Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":{"spec":{"$wf.path":` + tc.marker + `}}}`)}
			err := wf.CheckJournalRelocation([]wf.JournalRecord{record}, wf.PathRoots{Workspace: "/source/work", State: "/source/state"})
			if (err != nil) != tc.bad {
				t.Fatalf("bad=%v err=%v", tc.bad, err)
			}
			if err != nil && (!strings.Contains(err.Error(), "Output.spec") || !strings.Contains(err.Error(), "journal 9")) {
				t.Fatalf("lost field/record diagnostic: %v", err)
			}
		})
	}
}

// an earlier revision: an effect intent that has its result in the prefix is history:
// its args (an asset script under the workspace, as an e2e run's proxy
// assets are) are not refused; an open one is still applied from its args,
// so it still refuses.
func TestCheckJournalRelocationReadsADoneIntentAsHistory(t *testing.T) {
	roots := wf.PathRoots{Workspace: "/source/work", State: "/source/state"}
	intent := wf.JournalRecord{Seq: 3, Kind: wf.JournalEffectIntent, Data: json.RawMessage(`{"Effect":{"Kind":"external","Adapter":"shell","Op":"run","Args":{"argv":["bash","/source/work/.superpowers/proxy-assets/s"]}}}`)}
	if err := wf.CheckJournalRelocation([]wf.JournalRecord{intent}, roots); err == nil || !strings.Contains(err.Error(), "Effect.Args.argv[1]") {
		t.Fatalf("open intent naming the source root: %v, want a refusal", err)
	}
	result := wf.JournalRecord{Seq: 4, Kind: wf.JournalEffectResult, Data: json.RawMessage(`{"Intent":3,"OK":true}`)}
	if err := wf.CheckJournalRelocation([]wf.JournalRecord{intent, result}, roots); err != nil {
		t.Fatalf("done intent refused: %v", err)
	}
	settled := wf.JournalRecord{Seq: 4, Kind: wf.JournalEffectSettled, Data: json.RawMessage(`{"Intent":3,"Applied":true}`)}
	if err := wf.CheckJournalRelocation([]wf.JournalRecord{intent, settled}, roots); err != nil {
		t.Fatalf("settled intent refused: %v", err)
	}
}

// A chosen edge's Plan whose instance has since transitioned is history: the
// fork never binds it again, so a literal path its effect args spell under
// the source state root (a records file the definition joined into a
// PathText argument) does not refuse the fork. A Plan still pending is bound
// from its record, so it still refuses.
func TestCheckJournalRelocationReadsAConsumedPlanAsHistory(t *testing.T) {
	roots := wf.PathRoots{Workspace: "/source/work", State: "/source/state"}
	plan := `{"Plan":{"Steps":[{"From":"collect","To":"stop","Effects":[{"Kind":"external","Adapter":"shell","Op":"run",` +
		`"Args":{"argv":["bash","-c","write","/source/state/runs/r/records/receipt.json"]},"BindingVersion":1,"Literal":true,` +
		`"ArgTemplates":[{"Path":["argv","3"],"Spans":[{"Text":"/source/state/runs/r/records/receipt.json","Path":true}]}]}]}]}}`
	accepted := wf.JournalRecord{Seq: 5, Kind: wf.JournalAccepted, Instance: "Gate#1", State: "collect", Data: json.RawMessage(plan)}
	if err := wf.CheckJournalRelocation([]wf.JournalRecord{accepted}, roots); err == nil || !strings.Contains(err.Error(), "Plan.Steps[0].Effects[0]") {
		t.Fatalf("pending plan naming the source root: %v, want a refusal", err)
	}
	other := wf.JournalRecord{Seq: 6, Kind: wf.JournalTransition, Instance: "Other#1", Data: json.RawMessage(`{"To":"next"}`)}
	if err := wf.CheckJournalRelocation([]wf.JournalRecord{accepted, other}, roots); err == nil {
		t.Fatal("another instance's transition consumed this plan")
	}
	moved := wf.JournalRecord{Seq: 7, Kind: wf.JournalTransition, Instance: "Gate#1", State: "collect", Data: json.RawMessage(`{"To":"stop"}`)}
	if err := wf.CheckJournalRelocation([]wf.JournalRecord{accepted, other, moved}, roots); err != nil {
		t.Fatalf("consumed plan refused: %v", err)
	}
	// A typed path in a consumed plan is still checked.
	typed := wf.JournalRecord{Seq: 5, Kind: wf.JournalAccepted, Instance: "Gate#1", State: "collect",
		Data: json.RawMessage(`{"Plan":{"Steps":[{"Effects":[{"Args":{"p":{"$wf.path":{"base":"external","path":"/source/state/x"}}}}]}]}}`)}
	if err := wf.CheckJournalRelocation([]wf.JournalRecord{typed, moved}, roots); err == nil {
		t.Fatal("mislabeled external path in a consumed plan passed")
	}
}
