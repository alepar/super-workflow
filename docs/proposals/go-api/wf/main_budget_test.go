package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// run --budget: the registration's default is computed from the run's
// options, the flag overrides the limits it names, and run_started journals
// the result. Attaching to a live run never changes it.
func TestRunBudgetFlagOverRegistrationDefault(t *testing.T) {
	d := clAgentDef("budget-cli")
	wf.Options[wiOpts](d)
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d, Budget: func(o any) wf.RunBudget {
		if o.(wiOpts).Fast {
			return wf.RunBudget{Turns: 3}
		}
		return wf.RunBudget{Turns: 10, Tokens: 500}
	}}}}
	state, ws := t.TempDir(), t.TempDir()
	opts := filepath.Join(t.TempDir(), "opts.json")
	if err := os.WriteFile(opts, []byte(`{"Fast":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	call := func(want int, args ...string) string {
		t.Helper()
		var out, er bytes.Buffer
		if code := fixtureCLI(cfg, append(args, "--state-dir", state, "--workspace", ws), &out, &er); code != want {
			t.Fatalf("%v: exit %d, want %d: %s", args, code, want, er.String())
		}
		return out.String() + er.String()
	}
	budgetOf := func(run string) wf.RunBudget {
		t.Helper()
		var st wf.RunStatus
		if err := json.Unmarshal([]byte(call(0, "status", run, "--json")), &st); err != nil {
			t.Fatal(err)
		}
		return st.Budget
	}
	cases := []struct {
		run  string
		args []string
		want wf.RunBudget
	}{
		{"b1", nil, wf.RunBudget{Turns: 10, Tokens: 500}},
		{"b2", []string{"--options", opts}, wf.RunBudget{Turns: 3}},
		{"b3", []string{"--budget", "tokens=0,wall=1h"}, wf.RunBudget{Turns: 10, Wall: time.Hour}},
	}
	for _, c := range cases {
		call(0, append([]string{"run", d.Name(), "--run-id", c.run}, c.args...)...)
		if got := budgetOf(c.run); got != c.want {
			t.Errorf("run %s %v: budget %v, want %v", c.run, c.args, got, c.want)
		}
	}
	if out := call(2, "run", d.Name(), "--run-id", "b1", "--budget", "turns=99"); !strings.Contains(out, "resume it with --budget") {
		t.Fatalf("attach with another budget: %s", out)
	}
	call(0, "run", d.Name(), "--run-id", "b1", "--budget", "turns=10")
	if out := call(2, "run", d.Name(), "--run-id", "b4", "--budget", "turns=lots"); !strings.Contains(out, "not a non-negative integer") {
		t.Fatalf("bad --budget: %s", out)
	}
}
