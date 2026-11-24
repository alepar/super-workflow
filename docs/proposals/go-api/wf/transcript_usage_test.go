package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

const agyProbeChildID = "00000000-0000-4000-8000-000000000001"

// agyChildTranscript copies the probe child's transcript into a temp file.
func agyChildTranscript(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "harness", "testdata", "agy", "probe", "brain",
		agyProbeChildID, ".system_generated", "logs", "transcript_full.jsonl")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

func startedAgy(t *testing.T, name string) (*harness, wf.RunID, wf.Instruction) {
	t.Helper()
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	d := clAgentDef(name)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := handOutAsH(t, h, run, 1, "S", "agy")
	err := h.e.Started(run, ins[0].Name, wf.Claim{Harness: "agy", Session: "S", Agent: agyProbeChildID, Transcript: agyChildTranscript(t)})
	if err != nil {
		t.Fatal(err)
	}
	return h, run, ins[0]
}

// Rule 5 step 2: the budget counts only usage a child reports. A child that
// passes no --usage counts zero, however much its transcript records, and a
// failed attempt journals no tokens from its transcript.
func TestTranscriptUsageIsNotCounted(t *testing.T) {
	t.Run("agy done", func(t *testing.T) {
		h, run, in := startedAgy(t, "child-usage")
		if _, err := h.e.Done(run, in.Name, wf.Submission{Output: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		recs := h.kind(run, wf.JournalSubmitted)
		if len(recs) != 1 {
			t.Fatalf("%d submitted records", len(recs))
		}
		var d wf.SubmittedData
		if err := json.Unmarshal(recs[0].Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Tokens != 0 {
			t.Fatalf("submitted tokens %d, want 0 (no --usage)", d.Tokens)
		}
	})

	t.Run("agy failed attempt", func(t *testing.T) {
		h, run, in := startedAgy(t, "child-usage-failed")
		if _, err := h.e.FailedAs(run, in.Name, "gave up", wf.FailTransient); err != nil {
			t.Fatal(err)
		}
		for _, r := range h.kind(run, wf.JournalMetadataAttached) {
			var d wf.MetadataAttachedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				t.Fatal(err)
			}
			if d.Tokens != 0 {
				t.Fatalf("metadata_attached carries %d transcript tokens", d.Tokens)
			}
		}
	})
}
