package wf_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func promptHashDef(body string) *wf.Def {
	d := wf.New("recording")
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptText(body), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	return d
}

func recordPrompt(t *testing.T, body string, lines ...func(wf.RunID, string, ...string) string) ([]wf.JournalRecord, string) {
	t.Helper()
	d := promptHashDef(body)
	j := wf.MemJournal()
	root := t.TempDir()
	opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: root, RunDir: func(id wf.RunID) string { return filepath.Join(root, "runs", string(id)) }}
	if len(lines) > 0 {
		opts.VerbLine = lines[0]
	}
	e := wf.NewEngine(j, nil, opts).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("handoff: %v %v", ins, err)
	}
	f, err := e.PromptFetch(run, ins[0].Name, "")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(f.Prompt)))
	var fetched map[string]any
	b, _ := json.Marshal(f)
	_ = json.Unmarshal(b, &fetched)
	if fetched["promptHash"] != want {
		t.Fatalf("hash does not cover delivered body: %s", b)
	}
	if _, err := e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`"ok"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HandOut(run, 0); err != nil {
		t.Fatal(err)
	}
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	return recs, filepath.Join(root, "runs", string(run))
}

func TestReplayShapeIgnoresCommandTransport(t *testing.T) {
	recs, dir := recordPrompt(t, "same instructions", func(run wf.RunID, verb string, args ...string) string {
		return strings.Join(append([]string{"/installed/super", "wf", verb, "--state-dir", "/state/root", "--assets", "/assets/root", "--run-id", string(run)}, args...), " ")
	})
	report, err := wf.Replay(recs, promptHashDef("same instructions"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Stale) != 0 {
		t.Fatalf("only command transport changed: %s", report)
	}
}

func TestReplayPromptStalenessIsAdvisory(t *testing.T) {
	recs, dir := recordPrompt(t, "original")
	for _, body := range []string{"original", "changed"} {
		rep, err := wf.Replay(recs, promptHashDef(body), dir)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(rep)
		var report struct{ Stale []json.RawMessage }
		_ = json.Unmarshal(b, &report)
		want := 0
		if body == "changed" {
			want = 1
		}
		if len(report.Stale) != want {
			t.Fatalf("body %s: want %d stale: %s", body, want, b)
		}
		if rep.Stop.Kind != "ended" || len(rep.Differences) != 0 {
			t.Fatalf("staleness changed execution: %s", rep)
		}
	}
}

func TestPromptHashRecordedAndInheritedBySubmission(t *testing.T) {
	recs := recordProbe(t)
	hashes := map[string]string{}
	submissions := 0
	for _, rec := range recs {
		var data struct{ PromptHash string }
		if err := json.Unmarshal(rec.Data, &data); err != nil {
			t.Fatal(err)
		}
		key := rec.Instance + "/" + rec.State
		switch rec.Kind {
		case wf.JournalPromptFetched:
			if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(data.PromptHash) {
				t.Fatalf("fetch lacks content hash: %s", rec.Data)
			}
			hashes[key] = data.PromptHash
		case wf.JournalSubmitted:
			submissions++
			if data.PromptHash == "" || data.PromptHash != hashes[key] {
				t.Fatalf("submission lost fetched prompt identity: %s", rec.Data)
			}
		}
	}
	if submissions == 0 {
		t.Fatal("no submissions exercised")
	}
}
