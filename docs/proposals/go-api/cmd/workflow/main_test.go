package main

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// TestRegistersEveryRuntime: each of fsx, shell, git, gh and bd is served. A
// tool missing from PATH is "unavailable", which is not the same as "no
// runtime for this op".
func TestRegistersEveryRuntime(t *testing.T) {
	reg := registry()
	for _, op := range []string{"fsx.exists", "shell.run", "git.head", "gh.issue_view", "bd.ready"} {
		adapter, name, _ := strings.Cut(op, ".")
		_, err := reg.Read(t.TempDir(), wf.ObservationDesc{Adapter: adapter, Op: name})
		if err != nil && strings.Contains(err.Error(), "no runtime for") {
			t.Errorf("%s: %v", op, err)
		}
	}
	if _, err := reg.Read(t.TempDir(), wf.ObservationDesc{Adapter: "nope", Op: "x"}); err == nil || !strings.Contains(err.Error(), "no runtime for") {
		t.Errorf("an unregistered op should report no runtime, got %v", err)
	}
}
