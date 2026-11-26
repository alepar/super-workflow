package wf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestLegacyRootRelocationDoesNotCascade(t *testing.T) {
	from := PathRoots{Workspace: "/old/state/workspace", State: "/old/state"}
	to := PathRoots{Workspace: "/old/state/new-workspace", State: "/new/state"}
	for source, want := range map[string]string{"/old/state/workspace/file": "/old/state/new-workspace/file", "/old/state/runs/r": "/new/state/runs/r"} {
		got := relocateString(source, from, to)
		if got != want {
			t.Fatalf("%s: %s", source, got)
		}
	}
}

func TestRelocationPreservesDomainDataAndHarnessEvidence(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			raw := MemJournal()
			old := PathRoots{Workspace: "/old/work", State: "/old/state"}
			records := []JournalRecord{
				{Kind: JournalRunStarted, Run: "r", Data: json.RawMessage(`{"Workspace":"/old/work","Params":{"quote":"the incident happened in /old/work yesterday"}}`)},
				{Kind: JournalAccepted, Run: "r", Data: json.RawMessage(`{"Output":{"quote":"the incident happened in /old/work yesterday"}}`)},
				{Kind: JournalSubmitted, Run: "r", Data: json.RawMessage(`{"Output":{"quote":"the incident happened in /old/work yesterday"},"Artifacts":{"report":{"Path":"/old/work/report.md","Hash":"hash"}}}`)},
				{Kind: JournalCoordinator, Run: "r", Data: json.RawMessage(`{"Harness":"codex","Root":"/old/work/external-harness","Session":"s"}`)},
				{Kind: JournalHandedOut, Run: "r", Data: json.RawMessage(`{"Root":"/old/work/external-harness","Name":"unit"}`)},
				{Kind: JournalAttemptStarted, Run: "r", Data: json.RawMessage(`{"Root":"/old/work/external-harness","Transcript":"/old/work/external-harness/rollout.jsonl"}`)},
				{Kind: JournalPromptFetched, Run: "r", Data: json.RawMessage(`{"PromptDir":"/old/state/runs/r/prompt/unit"}`)},
			}
			var writer Journal = &rootedJournal{Journal: raw, workspace: old.Workspace, state: old.State, active: map[RunID]PathRoots{}}
			if legacy {
				writer = raw
			}
			unlock, err := writer.Lock("r")
			if err != nil {
				t.Fatal(err)
			}
			for _, rec := range records {
				if err := writer.Append(rec); err != nil {
					t.Fatal(err)
				}
			}
			unlock()
			reader := &rootedJournal{Journal: raw, workspace: "/new/work", state: "/new/state", active: map[RunID]PathRoots{}}
			unlock, err = reader.Lock("r")
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			got, err := reader.Records("r")
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range []int{0, 1, 2} {
				if !bytes.Contains(got[i].Data, []byte("the incident happened in /old/work yesterday")) {
					t.Fatalf("domain data changed: %s", got[i].Data)
				}
			}
			for _, i := range []int{3, 4, 5} {
				if !bytes.Equal(got[i].Data, records[i].Data) {
					t.Fatalf("pinned evidence changed: %s", got[i].Data)
				}
			}
			if !bytes.Contains(got[0].Data, []byte(`"Workspace":"/new/work"`)) || !bytes.Contains(got[2].Data, []byte(`/new/work/report.md`)) || !bytes.Contains(got[6].Data, []byte(`/new/state/runs/r/prompt/unit`)) {
				t.Fatalf("owned paths did not move: %s %s %s", got[0].Data, got[2].Data, got[6].Data)
			}
		})
	}
}

func TestCommandDirectoryIsTheOnlyRelocatableEffectArgument(t *testing.T) {
	from := PathRoots{Workspace: "/old/work", State: "/old/state"}
	to := PathRoots{Workspace: "/new/work", State: "/new/state"}
	for _, command := range []bool{false, true} {
		for _, dir := range []string{"/old/work/sub", "/external/work"} {
			payload := EffectIntentData{Command: command, Effect: EffectData{Adapter: "shell", Op: "run", Args: map[string]any{"dir": dir, "argv": []string{"echo", "/old/work/history"}, "stdin": "/old/work/historical"}}}
			raw, _ := json.Marshal(payload)
			moved, err := mapJournalPaths(JournalEffectIntent, raw, func(s string) string { return relocateString(s, from, to) })
			if err != nil {
				t.Fatal(err)
			}
			var got EffectIntentData
			if err := json.Unmarshal(moved, &got); err != nil {
				t.Fatal(err)
			}
			want := dir
			if command && dir == "/old/work/sub" {
				want = "/new/work/sub"
			}
			if got.Effect.Args["dir"] != want || got.Effect.Args["stdin"] != "/old/work/historical" || got.Effect.Args["argv"].([]any)[1] != "/old/work/history" {
				t.Fatalf("unexpected arguments: %+v", got.Effect.Args)
			}
		}
	}
}

// A relative record with no path token reads back byte for byte; one with a
// token in a mapped field is still converted.
func TestRootedReadSkipsRecordsWithoutTokens(t *testing.T) {
	raw := MemJournal()
	writer := &rootedJournal{Journal: raw, workspace: "/old/work", state: "/old/state", active: map[RunID]PathRoots{}}
	unlock, err := writer.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []JournalRecord{
		{Kind: JournalRunStarted, Run: "r", Data: json.RawMessage(`{"Workspace":"/old/work"}`)},
		{Kind: JournalInstructionIssued, Run: "r", Data: json.RawMessage(`{"Workspace":"/elsewhere","Prompt":"a <prompt>"}`)},
		{Kind: JournalInstructionIssued, Run: "r", Data: json.RawMessage(`{"Workspace":"/old/work/sub"}`)},
	} {
		if err := writer.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := raw.Records("r")
	if err != nil {
		t.Fatal(err)
	}
	reader := &rootedJournal{Journal: raw, workspace: "/new/work", state: "/new/state", active: map[RunID]PathRoots{}}
	got, err := reader.Records("r")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[1].Data, stored[1].Data) {
		t.Fatalf("token-free record changed: %s, stored %s", got[1].Data, stored[1].Data)
	}
	if !bytes.Contains(got[2].Data, []byte(`"/new/work/sub"`)) {
		t.Fatalf("tokened record not converted: %s", got[2].Data)
	}
}

// A second read of the same records answers from the memo, byte for byte as
// the first; a record whose stored bytes differ at the same seq is mapped
// again, as are records read under other roots.
func TestRootedMemoMatchesAFreshConversion(t *testing.T) {
	raw := MemJournal()
	writer := &rootedJournal{Journal: raw, workspace: "/old/work", state: "/old/state", active: map[RunID]PathRoots{}}
	unlock, err := writer.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []JournalRecord{
		{Kind: JournalRunStarted, Run: "r", Data: json.RawMessage(`{"Workspace":"/old/work"}`)},
		{Kind: JournalInstructionIssued, Run: "r", Data: json.RawMessage(`{"Workspace":"/old/work/sub"}`)},
	} {
		if err := writer.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := raw.Records("r")
	unlock()
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored %d records: %v", len(stored), err)
	}
	reader := &rootedJournal{Journal: raw, workspace: "/new/work", state: "/new/state", active: map[RunID]PathRoots{}}
	first, _, err := reader.rooted(stored)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := reader.rooted(stored)
	if err != nil || !bytes.Equal(first[1].Data, again[1].Data) || !bytes.Contains(again[1].Data, []byte(`"/new/work/sub"`)) {
		t.Fatalf("memo read %s, first %s (%v)", again[1].Data, first[1].Data, err)
	}
	changed := append([]JournalRecord(nil), stored...)
	changed[1].Data = bytes.Replace(stored[1].Data, []byte("sub"), []byte("other"), 1)
	got, _, err := reader.rooted(changed)
	if err != nil || !bytes.Contains(got[1].Data, []byte(`"/new/work/other"`)) {
		t.Fatalf("changed bytes answered from the memo: %s (%v)", got[1].Data, err)
	}
	moved := &rootedJournal{Journal: raw, workspace: "/third/work", state: "/third/state", active: map[RunID]PathRoots{}, mapped: reader.mapped}
	got, _, err = moved.rooted(stored)
	if err != nil || !bytes.Contains(got[1].Data, []byte(`"/third/work/sub"`)) {
		t.Fatalf("other roots answered from the memo: %s (%v)", got[1].Data, err)
	}
}
