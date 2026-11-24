package wftest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func TestSimulationExplicitAssets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("selected fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	d := wf.New("assets")
	a := wf.NewAgent[fuzzAgent, bool](d)
	end := wf.NewTerminal[fuzzDone](d, wf.Success)
	edge := a.To(end, "done")
	a.Spec(wf.PromptFile("{assets}/prompt.md"), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	tr := wftest.New(d, wftest.WithAssets(root)).Run(wftest.SubmitAt("fuzzAgent", true))
	if tr.Terminal() != "fuzzDone" {
		t.Fatalf("override not used: %s", tr)
	}
	seen := false
	for _, r := range tr.Journal() {
		if r.Kind == wf.JournalInstructionIssued && strings.Contains(string(r.Data), "selected fixture") {
			seen = true
		}
	}
	if !seen {
		t.Fatal("selected asset was not rendered")
	}
}
