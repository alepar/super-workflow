package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type pathCommandSetup struct{}
type pathCommandRead struct{}
type pathCommandAudit struct{}
type pathCommandDone struct{}
type pathCommandInput struct{ Spec wf.RunPath }

func pathCommandDef(detached, idempotent bool) *wf.Def {
	d := wf.New("typed-command-path")
	setup := wf.NewCompute[pathCommandSetup, pathCommandInput](d, func(wf.Ctx) (pathCommandInput, error) { return pathCommandInput{wf.RunPath("records/spec.md")}, nil })
	command := wf.NewCommand[pathCommandRead](d, "cat", "{spec}").Key("read-spec")
	if idempotent {
		command.Idempotent()
	}
	if detached {
		command.Detached(time.Minute)
	}
	audit := wf.NewAgent[pathCommandAudit, string](d)
	done := wf.NewTerminal[pathCommandDone](d, wf.Success)
	setup.Then(setup.To(command, "read"))
	command.Then(command.To(audit, "audit"))
	edge := audit.To(done, "done")
	audit.Spec(wf.PromptText("Review {{result}}", wf.As("result", wf.Out(command))), wf.Retry(wf.Const(1), edge))
	audit.Route(func(string, wf.Ctx) wf.Edge[pathCommandAudit] { return edge }, edge)
	return d
}

func writePathCommandSpec(t *testing.T, state string, run wf.RunID) {
	t.Helper()
	dir := filepath.Join(state, "runs", string(run), "records")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.md"), []byte("portable spec\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func pathCommandOptions(d *wf.Def, workspace, state string) wf.EngineOptions {
	return wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: workspace, StateRoot: state, RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }}
}

func assertPathCommandIntent(t *testing.T, state string, run wf.RunID) json.RawMessage {
	t.Helper()
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Kind != wf.JournalEffectIntent {
			continue
		}
		var intent wf.EffectIntentData
		if err = json.Unmarshal(record.Data, &intent); err != nil {
			t.Fatal(err)
		}
		if !intent.Command {
			continue
		}
		argv, ok := intent.Effect.Args["argv"].([]any)
		if !ok || len(argv) != 2 {
			t.Fatalf("argv: %#v", intent.Effect.Args["argv"])
		}
		marker, ok := argv[1].(map[string]any)
		if !ok || marker["$wf.runPath"] != "records/spec.md" {
			t.Fatalf("command intent eagerly resolved typed argument: %s", record.Data)
		}
		return record.Data
	}
	t.Fatal("no command intent")
	return nil
}

func assertPathCommandAudit(t *testing.T, e *wf.Engine, run wf.RunID) {
	t.Helper()
	offers, err := e.Next(run, "")
	if err != nil || len(offers) != 1 || offers[0].State != "pathCommandAudit" {
		t.Fatalf("audit: %+v %v", offers, err)
	}
	var result wf.RunResult
	if err = json.Unmarshal([]byte(offers[0].Inputs["result"]), &result); err != nil || result.Exit != 0 || result.Stdout != "portable spec\n" {
		t.Fatalf("command read wrong world: %+v %v", result, err)
	}
}

func TestCommandRunPathSnapshotForkUsesCurrentRunDirectory(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "data"), []byte("world"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{shell.Runtime(shell.Config{}), &snapshotRuntime{}} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := pathCommandDef(false, false)
	opts := pathCommandOptions(d, workspace, state)
	opts.Snapshots = true
	e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	writePathCommandSpec(t, state, run)
	assertPathCommandAudit(t, e, run)
	assertPathCommandIntent(t, state, run)
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var before, after int64
	for _, r := range records {
		if r.Kind != wf.JournalWorldSnapshot {
			continue
		}
		var saved wf.WorldSnapshotData
		if err = json.Unmarshal(r.Data, &saved); err != nil {
			t.Fatal(err)
		}
		if r.State == "pathCommandSetup" && saved.Before == wf.JournalTransition {
			before = r.Seq
		}
		if r.State == "pathCommandAudit" && saved.Before == wf.JournalInstructionIssued {
			after = r.Seq
		}
	}
	if before == 0 || after == 0 {
		t.Fatal("missing command frontiers")
	}
	// The completed command's history must remain forkable too: persisting an
	// absolute source argv would trigger the intentional legacy-path refusal.
	completed, err := e.ForkSnapshot(run, after, filepath.Join(t.TempDir(), "completed"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := e.ForkSnapshot(run, before, filepath.Join(t.TempDir(), "pending"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{workspace, state} {
		if err = os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, fork := range []wf.SnapshotFork{completed, pending} {
		restored := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, pathCommandOptions(d, fork.Workspace, fork.StateDir)).(*wf.Engine)
		assertPathCommandAudit(t, restored, run)
		assertPathCommandIntent(t, fork.StateDir, run)
	}
}

type pathCommandCrashJournal struct {
	wf.Journal
	crashed bool
}

var errPathCommandCrash = errors.New("crash after command intent")

func (j *pathCommandCrashJournal) Append(r wf.JournalRecord) error {
	if err := j.Journal.Append(r); err != nil {
		return err
	}
	if r.Kind == wf.JournalEffectIntent && !j.crashed {
		var d wf.EffectIntentData
		if json.Unmarshal(r.Data, &d) == nil && d.Command {
			j.crashed = true
			return errPathCommandCrash
		}
	}
	return nil
}

type pathCommandShell struct {
	wf.AdapterRuntime
	checked []map[string]any
}

func (r *pathCommandShell) Check(workspace string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	r.checked = append(r.checked, e.Args)
	return r.AdapterRuntime.Check(workspace, e)
}

func TestCommandRunPathRecoveryUsesCurrentRunDirectory(t *testing.T) {
	for _, mode := range []string{"keyed", "idempotent", "detached"} {
		t.Run(mode, func(t *testing.T) {
			workspace, state := t.TempDir(), filepath.Join(t.TempDir(), "source-state")
			d := pathCommandDef(mode == "detached", mode == "idempotent")
			rt := &pathCommandShell{AdapterRuntime: shell.Runtime(shell.Config{})}
			reg := wf.NewAdapters()
			if err := reg.Register(rt); err != nil {
				t.Fatal(err)
			}
			clock := time.Unix(1000, 0)
			opts := pathCommandOptions(d, workspace, state)
			opts.Clock = func() time.Time { return clock }
			runner := newDetRunner()
			if mode == "detached" {
				opts.EffectRunner = runner
			}
			crash := &pathCommandCrashJournal{Journal: wf.FileJournal(state)}
			e := wf.NewEngine(crash, reg, opts).(*wf.Engine)
			run, err := e.Run(d, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			writePathCommandSpec(t, state, run)
			if _, err = e.Next(run, ""); !errors.Is(err, errPathCommandCrash) {
				t.Fatalf("failed to crash after intent: %v", err)
			}
			original := assertPathCommandIntent(t, state, run)
			moved := filepath.Join(t.TempDir(), "restored-state")
			if err = os.Rename(state, moved); err != nil {
				t.Fatal(err)
			}
			opts = pathCommandOptions(d, workspace, moved)
			clock = clock.Add(20 * time.Second)
			opts.Clock = func() time.Time { return clock }
			if mode == "detached" {
				opts.EffectRunner = runner
			}
			restored := wf.NewEngine(wf.FileJournal(moved), reg, opts).(*wf.Engine)
			if mode == "detached" {
				if _, err = restored.Next(run, ""); err != nil {
					t.Fatal(err)
				}
				if runner.startCount() != 1 {
					t.Fatalf("detached launches %d", runner.startCount())
				}
				req := runner.request(0)
				want := filepath.Join(moved, "runs", string(run), "records/spec.md")
				if len(req.Argv) != 2 || req.Argv[1] != want {
					t.Fatalf("detached argv: %q", req.Argv)
				}
			} else {
				assertPathCommandAudit(t, restored, run)
			}
			if mode == "keyed" {
				if len(rt.checked) != 1 {
					t.Fatalf("checks: %d", len(rt.checked))
				}
				raw, _ := json.Marshal(rt.checked[0])
				if strings.Contains(string(raw), "$wf.runPath") || !strings.Contains(string(raw), moved) {
					t.Fatalf("Check received unresolved or source arguments: %s", raw)
				}
			}
			after := assertPathCommandIntent(t, moved, run)
			if string(after) != string(original) {
				t.Fatalf("recovery rewrote durable intent: %s -> %s", original, after)
			}
		})
	}
}

// Mirrors RunGoalCheck: a shell script receives an allocated test artifact as
// a whole positional argument, rather than embedding its path in shell text.
func TestCommandArtifactPathSnapshotForkUsesCurrentRunDirectory(t *testing.T) {
	d := wf.New("artifact-command-path")
	produce := wf.NewAgent[pathCommandSetup, string](d)
	artifact := wf.NewArtifact[string](produce, "goal_test.go")
	command := wf.NewCommand[pathCommandRead](d, "sh", "-c", `cat "$1"`, "goal-check", wf.ArtifactPath(artifact)).Key("goal-check")
	audit := wf.NewAgent[pathCommandAudit, string](d)
	done := wf.NewTerminal[pathCommandDone](d, wf.Success)
	toCommand := produce.To(command, "check")
	produce.Spec(wf.PromptText("Write goal test"), wf.Retry(wf.Const(1), toCommand))
	produce.Route(func(string, wf.Ctx) wf.Edge[pathCommandSetup] { return toCommand }, toCommand)
	command.Then(command.To(audit, "audit"))
	end := audit.To(done, "done")
	audit.Spec(wf.PromptText("Review {{result}}", wf.As("result", wf.Out(command))), wf.Retry(wf.Const(1), end))
	audit.Route(func(string, wf.Ctx) wf.Edge[pathCommandAudit] { return end }, end)
	workspace, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "data"), []byte("world"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{shell.Runtime(shell.Config{}), &snapshotRuntime{}} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	opts := pathCommandOptions(d, workspace, state)
	opts.Snapshots = true
	e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	offers, err := e.Next(run, "")
	if err != nil || len(offers) != 1 {
		t.Fatalf("produce: %+v %v", offers, err)
	}
	var path string
	for _, line := range strings.Split(offers[0].Prompt, "\n") {
		if p, ok := strings.CutPrefix(line, "- goal_test.go: "); ok {
			path = p
		}
	}
	if path == "" {
		t.Fatalf("missing artifact: %s", offers[0].Prompt)
	}
	if err = os.WriteFile(path, []byte("portable spec\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in := offers[0]
	result, err := e.Submit(run, wf.Submission{Child: in.Child, Token: in.Token, Attempt: in.Attempt, Output: json.RawMessage(`"written"`), Artifacts: map[string]string{"goal_test.go": path}})
	if err != nil || result.Class != wf.Accepted {
		t.Fatalf("submit: %+v %v", result, err)
	}
	assertPathCommandAudit(t, e, run)
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var before, after int64
	for _, r := range records {
		if r.Kind == wf.JournalEffectIntent {
			var intent wf.EffectIntentData
			if err := json.Unmarshal(r.Data, &intent); err != nil {
				t.Fatal(err)
			}
			if intent.Command {
				argv := intent.Effect.Args["argv"].([]any)
				marker, ok := argv[4].(map[string]any)
				if !ok || !strings.HasPrefix(fmt.Sprint(marker["$wf.runPath"]), "records/artifacts/") {
					t.Fatalf("artifact argv lost typed path: %s", r.Data)
				}
			}
		}
		if r.Kind != wf.JournalWorldSnapshot {
			continue
		}
		var saved wf.WorldSnapshotData
		if err = json.Unmarshal(r.Data, &saved); err != nil {
			t.Fatal(err)
		}
		if r.State == "pathCommandSetup" && saved.Before == wf.JournalTransition {
			before = r.Seq
		}
		if r.State == "pathCommandAudit" && saved.Before == wf.JournalInstructionIssued {
			after = r.Seq
		}
	}
	if before == 0 || after == 0 {
		t.Fatal("missing frontiers")
	}
	var forks []wf.SnapshotFork
	for _, seq := range []int64{before, after} {
		fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
		if err != nil {
			t.Fatal(err)
		}
		forks = append(forks, fork)
	}
	for _, root := range []string{workspace, state} {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
	}
	for _, fork := range forks {
		restored := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, pathCommandOptions(d, fork.Workspace, fork.StateDir)).(*wf.Engine)
		assertPathCommandAudit(t, restored, run)
	}
}
