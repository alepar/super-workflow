package wf_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"os"
	"testing"
)

func TestToolEnvironmentOverridesArePerInvocation(t *testing.T) {
	t.Setenv("WF_SNAPSHOT_TEST", "original")
	r, err := wf.RunTool(wf.Tool{Bin: "sh", Args: []string{"-c", `printf '%s' "$WF_SNAPSHOT_TEST"`}, Env: []string{"WF_SNAPSHOT_TEST=override"}})
	if err != nil || r.Exit != 0 || r.Stdout != "override" {
		t.Fatalf("override: %+v %v", r, err)
	}
	if os.Getenv("WF_SNAPSHOT_TEST") != "original" {
		t.Fatal("tool changed parent environment")
	}
}

func TestToolEnvironmentRemovalPrecedesOverrides(t *testing.T) {
	t.Setenv("WF_REMOVE_TEST", "parent")
	t.Setenv("WF_KEEP_TEST", "kept")
	for _, overrides := range [][]string{nil, {"WF_REMOVE_TEST=owned"}} {
		r, err := wf.RunTool(wf.Tool{Bin: "sh", Args: []string{"-c", `printf '%s/%s' "${WF_REMOVE_TEST-unset}" "$WF_KEEP_TEST"`}, UnsetEnv: []string{"WF_REMOVE_TEST"}, Env: overrides})
		want := "unset/kept"
		if overrides != nil {
			want = "owned/kept"
		}
		if err != nil || r.Exit != 0 || r.Stdout != want {
			t.Fatalf("environment: %+v %v; want %s", r, err, want)
		}
	}
	if os.Getenv("WF_REMOVE_TEST") != "parent" {
		t.Fatal("parent environment mutated")
	}
}
