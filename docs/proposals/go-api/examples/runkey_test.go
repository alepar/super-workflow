package examples_test

// Owns: TestEveryExampleStartsUnderRunKey, the start check of every engine
// example definition under the run-key requirement of `run` (wf/main_run.go).
// Does not: drive a run; starting one only journals run_started.

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/agentless"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/approval"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/broaden"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/checkrepair"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/dispatch"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/escalation"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/fanout"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/gatedchain"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/noprogress"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/orchestrator"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/park"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/resumable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/router"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/sendback"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/stale"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tournament"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/typedexit"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/unavailable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// keyParams names the run-key parameter of each keyed definition; every other
// definition is keyless and starts only with --run-id.
var keyParams = map[string]string{
	"resumable":  "dataset",
	"feature":    "goal",
	"gatedchain": "topic",
	"tournament": "seed",
}

func runCLI(cfg wf.MainConfig, args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = wf.CLI(cfg, args, &o, &e)
	return code, strings.TrimSpace(o.String()), strings.TrimSpace(e.String())
}

func allDefs() []*wf.Def {
	return []*wf.Def{
		agentless.Definition(), approval.Definition(), broaden.Definition(), checkrepair.Definition(),
		critique.Definition(), dispatch.Definition(), escalation.Definition(), fanout.Definition(), feature.Definition(),
		gatedchain.Definition(), noprogress.Definition(), orchestrator.Definition(), park.Definition(),
		resumable.Definition(), router.Definition(), sendback.Definition(), stale.Definition(), tour.Definition(), tournament.Definition(),
		typedexit.Definition(), unavailable.Definition(),
	}
}

func TestEveryExampleStartsUnderRunKey(t *testing.T) {
	// These examples pin Claude models, independently of the hosting harness.
	for _, key := range harnessEnvVars() {
		t.Setenv(key, "")
	}
	t.Setenv("CLAUDECODE", "1")
	defs := allDefs()
	listed := map[string]bool{}
	for _, d := range defs {
		listed[d.Name()] = true
	}
	// Guard the list: a new example directory must be added to allDefs.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(e.Name(), "def.go")); e.IsDir() && err == nil && !listed[e.Name()] {
			t.Errorf("examples/%s has a def.go but is not in allDefs", e.Name())
		}
	}
	if len(defs) != len(listed) {
		t.Fatalf("allDefs lists %d definitions under %d names", len(defs), len(listed))
	}

	regs := make([]wf.Registration, len(defs))
	for i, d := range defs {
		regs[i] = wf.Registration{Def: d}
	}
	cfg := wf.MainConfig{Defs: regs, Adapters: wf.NewAdapters()}

	for _, d := range defs {
		name := d.Name()
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// --harness pins the tier models: harness detection reads the ambient env.
			base := []string{"run", name, "--state-dir", dir, "--workspace", dir, "--harness", "claude"}
			key, keyed := keyParams[name]
			if !keyed {
				code, _, stderr := runCLI(cfg, base...)
				if code != 2 || !strings.Contains(stderr, "has no run key: pass --run-id") {
					t.Fatalf("keyless run without --run-id = %d %q, want exit 2 naming the missing run key", code, stderr)
				}
				code, out, stderr := runCLI(cfg, append(slices.Clone(base), "--run-id", name+"-1")...)
				if code != 0 || out != name+"-1" {
					t.Fatalf("keyless run with --run-id = %d %q (stderr %q), want exit 0 printing %s-1", code, out, stderr, name)
				}
				return
			}
			// Keyed: the key param is missing, then present twice (resume by key).
			params := []string{"--param", key + "=v1"}
			code, _, stderr := runCLI(cfg, base...)
			if code != 2 || !strings.Contains(stderr, key) {
				t.Fatalf("keyed run without %s = %d %q, want exit 2 naming the param", key, code, stderr)
			}
			code, first, stderr := runCLI(cfg, append(slices.Clone(base), params...)...)
			if code != 0 || first == "" {
				t.Fatalf("keyed run = %d %q (stderr %q), want exit 0 printing the id", code, first, stderr)
			}
			code, again, stderr := runCLI(cfg, append(slices.Clone(base), params...)...)
			if code != 0 || again != first {
				t.Fatalf("second keyed run = %d %q (stderr %q), want exit 0 and the same id %q", code, again, stderr, first)
			}
		})
	}
}

// Missing stop reasons or unbounded cycles in a served example must fail CI.
func TestEveryExampleIsSound(t *testing.T) {
	for _, d := range allDefs() {
		t.Run(d.Name(), func(t *testing.T) {
			for _, p := range d.LintSound() {
				t.Error(p.Line())
			}
		})
	}
}
