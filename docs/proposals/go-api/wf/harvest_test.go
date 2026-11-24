package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func harvestRecords() []wf.JournalRecord {
	return []wf.JournalRecord{
		{Seq: 1, Run: "run-a", Instance: "root", State: "flow/Review", Attempt: 1, Kind: wf.JournalInstructionIssued, Data: json.RawMessage(`{"PromptHash":"sha256:old"}`)},
		{Seq: 2, Run: "run-a", Instance: "root", State: "flow/Review", Attempt: 1, Kind: wf.JournalRefused, Data: json.RawMessage(`{}`)},
		{Seq: 3, Run: "run-a", Instance: "root", State: "flow/Review", Attempt: 2, Kind: wf.JournalInstructionIssued, Data: json.RawMessage(`{"PromptHash":"sha256:current"}`)},
		{Seq: 4, Run: "run-a", Instance: "root", State: "flow/Review", Attempt: 2, Kind: wf.JournalSubmitted, Data: json.RawMessage(`{"Output":{"ok":true}}`)},
		{Seq: 5, Run: "run-a", Instance: "root", State: "flow/Review", Attempt: 2, Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":{"ok":true}}`)},
		{Seq: 6, Run: "run-a", State: "flow/Compute", Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":42,"Computed":true}`)},
		{Seq: 7, Run: "run-a", State: "flow/Child", Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":[],"Computed":true}`)},
		{Seq: 8, Run: "run-a", State: "../../escape", Kind: wf.JournalInstructionIssued, Data: json.RawMessage(`{}`)},
		{Seq: 9, Run: "run-a", State: "../../escape", Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":{"legacy":true}}`)},
	}
}

func TestHarvestAcceptedAgentResults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "corpus")
	entries, err := wf.Harvest(harvestRecords(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	e := entries[0]
	if e.State != "flow/Review" || e.Run != "run-a" || e.Seq != 5 || e.PromptHash != "sha256:current" || string(e.Output) != `{"ok":true}` {
		t.Fatalf("entry: %+v", e)
	}
	if entries[1].PromptHash != "" {
		t.Fatalf("legacy hash invented: %+v", entries[1])
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files: %v", files)
	}
	for _, f := range files {
		if f.IsDir() {
			t.Fatalf("state created directory: %s", f.Name())
		}
	}
	loaded, err := wf.LoadCorpus(dir)
	if err != nil || len(loaded) != 2 {
		t.Fatalf("load: %+v %v", loaded, err)
	}
	if _, err = wf.Harvest(harvestRecords(), dir); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	path := filepath.Join(dir, files[0].Name())
	if err = os.WriteFile(path, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = wf.Harvest(harvestRecords(), dir); err == nil {
		t.Fatal("overwrote conflicting entry")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "keep me" {
		t.Fatalf("existing file changed: %s", got)
	}
}

func TestHarvestCLI(t *testing.T) {
	dir, corpus := t.TempDir(), t.TempDir()
	j := wf.FileJournal(dir)
	unlock, err := j.Lock("run-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range harvestRecords() {
		if err = j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	unlock()
	var out, stderr bytes.Buffer
	code := wf.CLI(wf.MainConfig{}, []string{"dev", "harvest", "run-a", "--state-dir", dir, "--into", corpus}, &out, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	entries, err := wf.LoadCorpus(corpus)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries: %+v %v", entries, err)
	}
}

func TestHarvestDoesNotFollowExistingEntrySymlink(t *testing.T) {
	dir := t.TempDir()
	if _, err := wf.Harvest(harvestRecords(), dir); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, files[0].Name())
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err = os.WriteFile(outside, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err = wf.Harvest(harvestRecords(), dir); err == nil {
		t.Fatal("accepted existing symlink")
	}
	got, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("outside file changed: %q %v", got, err)
	}
}

func TestHarvestPromptHashDoesNotLeakAcrossInstances(t *testing.T) {
	recs := harvestRecords()
	recs = append(recs, wf.JournalRecord{Seq: 10, Run: "run-a", Instance: "other-child", State: "flow/Review", Attempt: 2, Kind: wf.JournalInstructionIssued, Data: json.RawMessage(`{}`)}, wf.JournalRecord{Seq: 11, Run: "run-a", Instance: "other-child", State: "flow/Review", Attempt: 2, Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":{"ok":true}}`)})
	entries, err := wf.Harvest(recs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[2].PromptHash != "" {
		t.Fatalf("cross-instance hash: %+v", entries)
	}
}

func TestHarvestPrefersFetchedHashAndClearsBetweenVisits(t *testing.T) {
	recs := []wf.JournalRecord{
		{Seq: 1, Run: "r", Instance: "root", State: "flow/A", Attempt: 1, Kind: wf.JournalInstructionIssued, Data: json.RawMessage(`{"PromptHash":"issued"}`)},
		{Seq: 2, Run: "r", Instance: "root", State: "flow/A", Attempt: 1, Kind: wf.JournalPromptFetched, Data: json.RawMessage(`{"Name":"A#1","PromptHash":"fetched"}`)},
		{Seq: 3, Run: "r", Instance: "root", State: "flow/A", Attempt: 1, Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":1}`)},
		{Seq: 4, Run: "r", Instance: "root", State: "flow/A", Attempt: 1, Kind: wf.JournalInstructionIssued, Data: json.RawMessage(`{}`)},
		{Seq: 5, Run: "r", Instance: "root", State: "flow/A", Attempt: 1, Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":2}`)},
		{Seq: 6, Run: "r", Instance: "root", State: "flow/A", Attempt: 1, Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":3}`)},
	}
	entries, err := wf.Harvest(recs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].PromptHash != "fetched" || entries[1].PromptHash != "" {
		t.Fatalf("visit hashes: %+v", entries)
	}
}

func TestHarvestQualifiesNestedStatesAndRequiresAgentEvidence(t *testing.T) {
	recs := []wf.JournalRecord{
		{Seq: 1, Run: "r", Kind: wf.JournalRunStarted, Data: json.RawMessage(`{"Def":"flow","Shape":{"flow/Review":"agent string","kid/Review":"agent string","leaf/Review":"agent string","flow/Compute":"computed string"}}`)},
		{Seq: 2, Run: "r", State: "Review", Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":"root"}`)},
		{Seq: 3, Run: "r", State: "Fan", Kind: wf.JournalChildSpawned, Data: json.RawMessage(`{"ID":"kid-1","Def":"kid","State":"Fan"}`)},
		{Seq: 4, Run: "r", Instance: "kid-1", State: "Review", Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":"child"}`)},
		{Seq: 5, Run: "r", Instance: "kid-1", State: "Fan", Kind: wf.JournalChildSpawned, Data: json.RawMessage(`{"ID":"leaf-1","Def":"leaf","State":"Fan"}`)},
		{Seq: 6, Run: "r", Instance: "leaf-1", State: "Review", Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":"nested"}`)},
		{Seq: 7, Run: "r", State: "Compute", Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":"not an agent"}`)},
		{Seq: 8, Run: "r", State: "Unknown", Kind: wf.JournalAccepted, Data: json.RawMessage(`{"Output":"unknown"}`)},
	}
	entries, err := wf.Harvest(recs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries: %+v", entries)
	}
	for i, want := range []string{"Review", "kid/Review", "kid/leaf/Review"} {
		if entries[i].State != want {
			t.Fatalf("entry %d state=%q want %q", i, entries[i].State, want)
		}
	}
}
