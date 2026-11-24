package wf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const agyProbeChild = "00000000-0000-4000-8000-000000000001"

func agyProbeTranscript() string {
	return filepath.Join("..", "harness", "testdata", "agy", "probe", "brain",
		agyProbeChild, ".system_generated", "logs", "transcript_full.jsonl")
}

func TestFanoutEvidenceCountsChildTokens(t *testing.T) {
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	started := func(path, h string) []JournalRecord {
		data, err := json.Marshal(AttemptStartedData{Transcript: path, Harness: h})
		if err != nil {
			t.Fatal(err)
		}
		return []JournalRecord{{Seq: 2, Kind: JournalAttemptStarted, Data: data}}
	}

	var agy fanoutResult
	collectFanoutEvidence(&agy, started(agyProbeTranscript(), "agy"), 0, "")
	if agy.ChildTokens == nil || *agy.ChildTokens != 378 {
		t.Fatalf("agy child tokens = %v", agy.ChildTokens)
	}

	path := filepath.Join(t.TempDir(), "claude.jsonl")
	lines := `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":40}}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":3,"output_tokens":7}}}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	var cl fanoutResult
	collectFanoutEvidence(&cl, started(path, "claude"), 0, "")
	if cl.ChildTokens == nil || *cl.ChildTokens != 60 {
		t.Fatalf("claude child tokens = %v", cl.ChildTokens)
	}
}
