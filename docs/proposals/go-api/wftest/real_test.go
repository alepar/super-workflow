package wftest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/gh"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	realAsk  struct{}
	realOut  struct{ OK bool }
	realDone struct{}
	realBad  struct{}
)

// stubGitHub is a gh script that remembers whether it created the issue: a
// search finds it, with its key marker, once it has.
func stubGitHub(t *testing.T, marker string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "gh"), filepath.Join(dir, "calls")
	created := filepath.Join(dir, "created")
	found, _ := json.Marshal([]gh.Issue{{Number: 7, Title: "t", Body: "b\n\n" + marker, State: "OPEN"}})
	script := "#!/bin/sh\necho \"$1 $2\" >> '" + log + "'\n" +
		"if [ \"$2\" = list ]; then if [ -e '" + created + "' ]; then cat <<'EOF'\n" + string(found) + "\nEOF\nelse echo '[]'; fi; exit 0; fi\n" +
		"touch '" + created + "'\necho https://example.invalid/o/r/issues/7\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// A real adapter runs under the simulator: the in-tree gh's IssueCreate
// lands, the engine dies before journaling its result, and on resume gh's
// own check finds the issue by its marker. It is created once, and the done
// result carries the issue the check found.
func TestRealAdapterSurvivesCrashAfterApply(t *testing.T) {
	key := wf.KeyOf("synthetic-issue")
	bin, log := stubGitHub(t, "<!-- effect-key: synthetic-issue -->")
	d := wf.New("real-gh")
	a := wf.NewAgent[realAsk, realOut](d)
	done := wf.NewTerminal[realDone](d, wf.Success)
	bad := wf.NewTerminal[realBad](d, wf.Stopped)
	a.Spec(wf.PromptText("p"), wf.Retry[realAsk](wf.Const(2), a.To(bad, "gave up")))
	a.Then(a.To(done, "filed").With(gh.IssueCreate(key, "o/r", "t", "b")))

	w := wftest.NewWorld()
	wftest.Real(w, gh.Runtime(gh.Config{Bin: bin}))
	tr := wftest.New(d, wftest.WithWorld(w), wftest.WithTB(t)).Run(
		wftest.Submit(a, realOut{OK: true}),
		wftest.CrashAfterIntent("gh", "issue_create"),
		wftest.Resume(),
	)
	if tr.Terminal() != "realDone" {
		t.Fatalf("terminal = %q, want realDone:\n%s", tr.Terminal(), tr)
	}
	calls, _ := os.ReadFile(log)
	if n := strings.Count(string(calls), "issue create"); n != 1 {
		t.Fatalf("gh issue create ran %d times, want 1:\n%s", n, calls)
	}
	var done0 *wf.EffectResultData
	for _, r := range tr.Journal() {
		if r.Kind == wf.JournalEffectResult {
			var rd wf.EffectResultData
			if err := json.Unmarshal(r.Data, &rd); err != nil {
				t.Fatal(err)
			}
			done0 = &rd
		}
	}
	if done0 == nil || done0.Outcome != "done" || !strings.Contains(string(done0.Result), `"Number":7`) {
		t.Fatalf("effect result = %+v, want done with issue 7", done0)
	}
}

// FailApply still fails a real op without reaching it.
func TestRealAdapterKeepsFailApply(t *testing.T) {
	bin, log := stubGitHub(t, "<!-- effect-key: k -->")
	d := wf.New("real-gh-fail")
	a := wf.NewAgent[realAsk, realOut](d)
	done := wf.NewTerminal[realDone](d, wf.Success)
	bad := wf.NewTerminal[realBad](d, wf.Stopped)
	d.OnFailure(wf.Blocked, bad)
	a.Spec(wf.PromptText("p"), wf.Retry[realAsk](wf.Const(2), a.To(bad, "gave up")))
	a.Then(a.To(done, "filed").With(gh.IssueCreate(wf.KeyOf("k"), "o/r", "t", "b")))
	w := wftest.NewWorld()
	wftest.Real(w, gh.Runtime(gh.Config{Bin: bin}))
	wftest.FailApply(w, "gh", "issue_create", "synthetic failure")
	tr := wftest.New(d, wftest.WithWorld(w), wftest.WithTB(t)).Run(wftest.Submit(a, realOut{OK: true}))
	if tr.Terminal() != "realBad" {
		t.Fatalf("terminal = %q, want realBad", tr.Terminal())
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("gh ran although FailApply failed the call")
	}
}
