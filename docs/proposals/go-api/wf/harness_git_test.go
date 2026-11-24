package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestGitWritableRemedyCodexNamesVerifiedSetting(t *testing.T) {
	got := wf.GitWritableRemedy("codex")
	for _, want := range []string{"workspace-write", "[sandbox_workspace_write]", "writable_roots", "<repo>/.git", ".codex/config.toml", "--git-common-dir", ".git/hooks", ".git/config", "outside the sandbox"} {
		if !strings.Contains(got, want) {
			t.Errorf("codex remedy lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "does not cover") {
		t.Errorf("codex remedy must not claim the route fails: %s", got)
	}
	if wf.GitWritableRemedy("Codex-CLI") != got {
		t.Error("harness aliases must normalize to the codex text")
	}
}

func TestGitWritableRemedyGenericHasNoCodexSpecifics(t *testing.T) {
	for _, h := range []string{"claude", "", "other"} {
		got := wf.GitWritableRemedy(h)
		if !strings.Contains(got, "git dir writable") || !strings.Contains(got, ".git/hooks") {
			t.Errorf("%q: generic remedy lacks the writable-git-dir fix or its hooks caveat: %s", h, got)
		}
		for _, bad := range []string{"codex", "Codex", "writable_roots", "sandbox_workspace_write"} {
			if strings.Contains(got, bad) {
				t.Errorf("%q: generic remedy mentions %q: %s", h, bad, got)
			}
		}
	}
}

// The Codex git remedy is the git-writable check's (its Remedy); the loop's
// standing text carries no harness notes (C1).
func TestBootstrapCarriesNoHarnessNotes(t *testing.T) {
	for _, h := range []string{"codex", "claude"} {
		if out := wf.Bootstrap(wf.RunStatus{Run: "run", Harness: h}, "./super", "/state"); strings.Contains(out, "writable_roots") || strings.Contains(out, "approvals reviewer") {
			t.Errorf("%s bootstrap carries harness notes:\n%s", h, out)
		}
	}
}

// Effort arrives with each unit as the harness's own parameter (C2); an
// instruction with none passes none.
func TestSpawnLinePassesEffort(t *testing.T) {
	if codex := spawnLine("codex", wf.DispatchHints{Effort: "high"}); !strings.Contains(codex, `reasoning_effort="high"`) {
		t.Errorf("codex spawn line = %s", codex)
	}
	claude := spawnLine("claude", wf.DispatchHints{Effort: "high"})
	if !strings.Contains(claude, `effort="high"`) || strings.Contains(claude, "reasoning_effort") {
		t.Errorf("claude spawn line = %s", claude)
	}
	if none := spawnLine("claude", wf.DispatchHints{}); strings.Contains(none, "effort") {
		t.Errorf("an instruction with no effort passed one: %s", none)
	}
}
