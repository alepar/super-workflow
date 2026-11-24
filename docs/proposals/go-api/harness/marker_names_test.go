package harness

import (
	"slices"
	"testing"
)

func TestMarkerNamesListsEveryAdapter(t *testing.T) {
	got := MarkerNames()
	for _, want := range []string{"CLAUDECODE", "CODEX_THREAD_ID", "HERMES_SESSION_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID"} {
		if !slices.Contains(got, want) {
			t.Errorf("MarkerNames() = %v, missing %s", got, want)
		}
	}
}
