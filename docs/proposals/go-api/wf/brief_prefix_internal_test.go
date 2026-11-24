package wf

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"strings"
	"testing"
)

// A context-only Hermes delegation clips its kickoff at about 600 bytes.
// The command must survive that clipping even with the real long run paths.
func TestBriefClaimSurvivesKickoffPrefix(t *testing.T) {
	const root = "/example/projects/workflow-demo/runs/scope-sample-with-a-long-workflow-name"
	const token = "006f2384be64557c"
	cmd := workspaceCommand("", root, wfLine(root+"/.superpowers/bin/super", root+"/.superpowers/state/examplewf", "examplewf-102030405060", "prompt", "--claim", token))
	c := &cycle{run: "examplewf-102030405060", st: &runState{started: &RunStartedData{Workspace: root}}, e: &Engine{opts: EngineOptions{VerbLine: func(run RunID, verb string, args ...string) string {
		return wfLine(root+"/.superpowers/bin/super", root+"/.superpowers/state/examplewf", run, verb, args...)
	}}}}
	brief := c.brief(token)
	for _, h := range []string{"claude", "codex", "hermes", "generic"} {
		t.Run(h, func(t *testing.T) {
			first, _, _ := strings.Cut(brief, "\n")
			if first != cmd {
				t.Fatalf("brief first line is not command: %q", first)
			}
			end := strings.Index(brief, token) + len(token)
			if end > 300 || end < len(token) {
				t.Fatalf("claim ends at byte%d, want <=300", end)
			}
			if got, ok := BriefCommand(brief); !ok || got != cmd {
				t.Fatalf("BriefCommand=%q,%v", got, ok)
			}
			rendered := harness.For(h).SpawnLine(harness.Spawn{Brief: brief})
			if !strings.Contains(rendered, token) {
				t.Fatal("spawn lost claim")
			}
		})
	}
}

func TestPagedClaimSurvivesKickoffPrefix(t *testing.T) {
	const root = "/example/projects/workflow-demo/runs/example"
	const token = "006f2384be64557c"
	command := workspaceCommand("", root, wfLine(root+"/.superpowers/bin/super", root+"/.superpowers/state/examplewf", "examplewf-102030405060", "prompt", "--claim", token))
	pages, _, err := pageText("instruction", "contract", harness.PromptEnvelope{Bytes: 3072, Lines: 150, Columns: 100}, func(int, string) string { return command }, "nonce", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pages[0], command+"\n") {
		t.Fatal("first page must start with prompt command")
	}
	end := strings.Index(pages[0], token) + len(token)
	if end < len(token) || end > 300 {
		t.Fatalf("claim ends at byte %d, want <=300", end)
	}
	if strings.Index(pages[0], "--state-dir") < end {
		t.Fatal("state path precedes claim")
	}
}
