package harness_test

import (
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

func TestPromptEnvelopeRespectsConfiguredRoute(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "settings.json"), `{"bashOutputMaxChars":4000}`)
	env := envOf(map[string]string{"CLAUDE_CONFIG_DIR": root})
	if got := harness.PromptEnvelopeFor("claude", env); got.Bytes >= 4000 || got.Lines > 150 || got.Columns > 100 {
		t.Fatalf("ignores configured route: %+v", got)
	}
	if got := harness.PromptEnvelopeFor("codex", env); got.Bytes != 6144 {
		t.Fatalf("known route: %+v", got)
	}
	if got := harness.PromptEnvelopeFor("unknown", env); got.Bytes != 3072 {
		t.Fatalf("unknown route: %+v", got)
	}
	if got := harness.PromptEnvelopeFor("claude", nil); got.Bytes != 3072 {
		t.Fatalf("unavailable configuration assumed safe: %+v", got)
	}
}
