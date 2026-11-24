package wf_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	rpaStep struct{}
	rpaDone struct{}
)

// an earlier revision: a run started with --assets pins those roots in run_started; its
// replay binds {assets} to them, not to the replaying binary's own root, so
// an installed run's journal replays under a dev build.
func TestReplayBindsTheRunsPinnedAssets(t *testing.T) {
	installed, dev := pinRoot(t, "installed"), pinRoot(t, "dev")
	workspace := t.TempDir()
	state := filepath.Join(workspace, "state")
	d := wf.New("replay-assets")
	step := wf.NewCompute[rpaStep, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	done := wf.NewTerminal[rpaDone](d, wf.Success)
	edge := step.To(done, "ran").With(shell.Effect(wf.KeyOf("assets", "run"), shell.Run("printf", "%s", "{assets}/p.md")))
	step.Route(func(bool, wf.Ctx) wf.Edge[rpaStep] { return edge }, edge)
	d.Assets(dev)
	h := newHarnessOpts(t, d, shell.Runtime(shell.Config{}), wf.EngineOptions{Workspace: workspace, StateRoot: state, Assets: installed,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }})
	run := h.run(d, nil)
	h.next(run)
	if st := h.status(run); st.Phase != wf.RunEnded {
		t.Fatalf("recorded run did not end: %+v", st)
	}
	report, err := wf.Replay(h.records(run), d, filepath.Join(state, "runs", string(run)))
	if err != nil || report.Stop.Kind != "ended" || len(report.Differences) != 0 {
		t.Fatalf("replay of a run pinned to --assets: %v\n%s", err, report)
	}
}

type (
	rpfStep struct{}
	rpfAsk  struct{}
	rpfDone struct{}
)

// an earlier revision: a replay answers effects from the journal without applying them,
// so a prompt file an earlier effect wrote under the run's records is read
// from the source run, as a submitted artifact is, and noted.
func TestReplayReadsAPromptFileAnEffectWroteFromTheSourceRun(t *testing.T) {
	workspace := t.TempDir()
	state := filepath.Join(workspace, "state")
	d := wf.New("replay-prompt-file")
	step := wf.NewCompute[rpfStep, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	ask := wf.NewAgent[rpfAsk, apOut](d)
	done := wf.NewTerminal[rpfDone](d, wf.Success)
	d.OnFailure(wf.Blocked, done)
	write := step.To(ask, "args written").With(shell.Effect(wf.KeyOf("args", "write"),
		shell.Run("bash", "-c", `mkdir -p "$(dirname "$1")" && printf 'review the args' > "$1"`, "write", wf.PathTemplate("{records}/args.md"))))
	step.Route(func(bool, wf.Ctx) wf.Edge[rpfStep] { return write }, write)
	end := ask.To(done, "reviewed")
	ask.Spec(wf.PromptFile("{records}/args.md"), wf.Retry[rpfAsk](wf.Const(1), end))
	ask.Route(func(apOut, wf.Ctx) wf.Edge[rpfAsk] { return end }, end)
	h := newHarnessOpts(t, d, shell.Runtime(shell.Config{}), wf.EngineOptions{Workspace: workspace, StateRoot: state,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }})
	run := h.run(d, nil)
	if st := h.drive(run, map[string][]string{"rpfAsk": {`{"OK":true}`}}); st.Phase != wf.RunEnded || st.Terminal != "rpfDone" {
		t.Fatalf("recorded run = %+v, want ended at rpfDone", st)
	}
	report, err := wf.Replay(h.records(run), d, filepath.Join(state, "runs", string(run)))
	if err != nil || report.Stop.Kind != "ended" || len(report.Differences) != 0 {
		t.Fatalf("replay of a prompt file an effect wrote: %v\n%s", err, report)
	}
	if !strings.Contains(strings.Join(report.Notes, "\n"), "args.md") {
		t.Errorf("notes = %q, want the prompt file read from the source run named", report.Notes)
	}
}

// an earlier revision round 1 (3): an unchanged definition whose installed and dev
// prompts differ reports the hash the replay ran on, the run's pinned one,
// not the binary's own.
func TestReplayReportsTheHashItRanOn(t *testing.T) {
	installed, dev := pinRoot(t, "installed prompt"), pinRoot(t, "dev prompt")
	workspace := t.TempDir()
	state := filepath.Join(workspace, "state")
	d := pinDef(dev)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: workspace, StateRoot: state, Assets: installed,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }})
	run := h.run(d, nil)
	if st := h.drive(run, map[string][]string{"apAsk": {`{"OK":true}`}}); st.Phase != wf.RunEnded {
		t.Fatalf("recorded run = %+v, want ended", st)
	}
	report, err := wf.Replay(h.records(run), d, filepath.Join(state, "runs", string(run)))
	if err != nil || report.Stop.Kind != "ended" {
		t.Fatalf("replay: %v\n%s", err, report)
	}
	if report.Then != report.Now {
		t.Errorf("recorded on %s, replayed on %s: the replay ran on the run's pinned assets, so the same hash", report.Then, report.Now)
	}
}
