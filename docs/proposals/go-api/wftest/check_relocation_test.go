package wftest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func TestCheckRelocationIncludesCalibrationOnlyRuns(t *testing.T) {
	workspace, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	factory := func() *wftest.Sim {
		d := wf.New("relocation")
		d.Param("tool")
		wf.NewTerminal[fuzzDone](d, wf.Success)
		return wftest.New(d, wftest.WithParams(map[string]string{"tool": unmappable(filepath.Join(workspace, "bin", "super"))}))
	}
	// The only available run is calibration discovery. Findings must not depend
	// on a later mixed seed being allocated or on an agent producing an output.
	got := wftest.Check(factory, wftest.CheckConfig{MaxRuns: 1, CalibrationConfig: wftest.CalibrationConfig{Seeds: 1, MaxSteps: 2}})
	problems := strings.Join(got.RealProblems, "\n")
	for _, want := range []string{"relocation", "journal 1", "Params.tool"} {
		if !strings.Contains(problems, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
}

func TestCheckRelocationExplicitWorkspaceAndExternalPaths(t *testing.T) {
	workspace := t.TempDir()
	for _, tc := range []struct {
		name, path string
		bad        bool
	}{
		{"owned", filepath.Join(workspace, "bin", "tool"), false}, // a fork rewrites it to its own workspace
		{"owned-unmappable", unmappable(filepath.Join(workspace, "bin", "tool")), true},
		{"external", filepath.Join(t.TempDir(), "tool"), false},
		{"relative", "bin/tool", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := func() *wftest.Sim {
				d := wf.New("relocation-control")
				d.Param("tool")
				wf.NewTerminal[fuzzDone](d, wf.Success)
				return wftest.New(d, wftest.WithWorkspace(workspace), wftest.WithParams(map[string]string{"tool": tc.path}))
			}
			cfg := wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 2, MaxSteps: 2}}
			got := wftest.Check(factory, cfg)
			count := 0
			for _, problem := range got.RealProblems {
				if strings.HasPrefix(problem, "relocation:") {
					count++
				}
			}
			if tc.bad && count != 1 || !tc.bad && count != 0 {
				t.Fatalf("bad=%v diagnostics=%d: %s", tc.bad, count, got)
			}
			if !tc.bad && len(got.RealProblems) > 0 {
				t.Fatalf("valid relocation failed: %s", got)
			}
		})
	}
}

// unmappable spells absolute path p with a doubled separator after its first
// component: the same file, but a spelling a fork's root rewrite does not
// match, so the relocation check still refuses it.
func unmappable(p string) string {
	return "/" + strings.Replace(p[1:], "/", "//", 1)
}

func TestCheckRelocationNormalizesRelativeWorkspace(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("relative-root")
		d.Param("note")
		wf.NewTerminal[fuzzDone](d, wf.Success)
		return wftest.New(d, wftest.WithWorkspace("."), wftest.WithParams(map[string]string{"note": "done."}))
	}
	got := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1, MaxSteps: 2}})
	if len(got.RealProblems) > 0 {
		t.Fatalf("punctuation treated as root: %s", got)
	}
}

func TestFuzzWorkspacePreservesPromptReadReceipts(t *testing.T) {
	workspace := t.TempDir()
	factory := func() *wftest.Sim {
		d := wf.New("workspace-prompt")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "failed")))
		a.Then(a.To(done, "accepted"))
		return wftest.New(d, wftest.WithWorkspace(workspace))
	}
	result := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 8, FailPercent: -1, SchemaPercent: -1, Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}})
	if result.Finding != nil || result.End != "success" {
		t.Fatalf("workspace simulation cannot read its prompt: %s", result)
	}
}

func TestSimulationWorkspaceDefaultAndOverride(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	explicit := t.TempDir()
	for _, tc := range []struct {
		name, want string
		options    []wftest.Option
	}{
		{"default", cwd, nil},
		{"override", explicit, []wftest.Option{wftest.WithWorkspace(explicit)}},
		{"empty", cwd, []wftest.Option{wftest.WithWorkspace("")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := func() *wftest.Sim {
				d := wf.New("default-workspace")
				wf.NewTerminal[fuzzDone](d, wf.Success)
				return wftest.New(d, tc.options...)
			}
			traces := []*wftest.Trace{factory().Run(), wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 2}).Trace}
			for _, trace := range traces {
				if trace == nil || len(trace.Journal()) == 0 {
					t.Fatal("missing journal")
				}
				var start wf.RunStartedData
				if err := json.Unmarshal(trace.Journal()[0].Data, &start); err != nil {
					t.Fatal(err)
				}
				if start.Workspace != tc.want {
					t.Fatalf("workspace=%q, want %q", start.Workspace, tc.want)
				}
			}
		})
	}
}
