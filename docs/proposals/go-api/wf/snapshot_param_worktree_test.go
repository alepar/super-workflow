package wf

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

type paramWorktreeSnapshotProbe struct {
	request SnapshotRequest
	stop    error
}

func (*paramWorktreeSnapshotProbe) Ops() []string { return []string{"git.worktree_add"} }
func (*paramWorktreeSnapshotProbe) Read(string, ObservationDesc) (json.RawMessage, error) {
	panic("unexpected read")
}
func (*paramWorktreeSnapshotProbe) Apply(string, ExternalEffect) (json.RawMessage, error) {
	panic("unexpected apply")
}
func (*paramWorktreeSnapshotProbe) Check(string, ExternalEffect) (EffectStatus, error) {
	panic("unexpected check")
}
func (p *paramWorktreeSnapshotProbe) SnapshotPreflight(request SnapshotRequest) error {
	p.request = request
	return p.stop
}
func (*paramWorktreeSnapshotProbe) Snapshot(SnapshotRequest) (json.RawMessage, error) {
	panic("unexpected capture")
}
func (*paramWorktreeSnapshotProbe) Restore(RestoreRequest, json.RawMessage) error {
	panic("unexpected restore")
}

func TestSnapshotCheckpointResolvesPathParamOwnedWorktree(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	runDir := filepath.Join(state, "runs", "r")
	probe := &paramWorktreeSnapshotProbe{stop: errors.New("preflight inspected")}
	reg := NewAdapters()
	if err := reg.Register(probe); err != nil {
		t.Fatal(err)
	}
	intent, err := json.Marshal(EffectIntentData{Effect: EffectData{Kind: "external", Adapter: "git", Op: "worktree_add", Args: map[string]any{"path": map[string]any{pathParamMarker: "tree"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(EffectResultData{Intent: 1, OK: true})
	if err != nil {
		t.Fatal(err)
	}
	c := &cycle{
		e:    &Engine{reg: reg, opts: EngineOptions{Workspace: workspace, StateRoot: state, RunDir: func(RunID) string { return runDir }}},
		run:  "r",
		st:   &runState{started: &RunStartedData{Workspace: workspace, Params: map[string]string{"tree": "worktrees/task"}, ParamPaths: map[string]PathBase{"tree": RunRelative}}, insts: map[string]*instance{}},
		recs: []JournalRecord{{Seq: 1, Kind: JournalEffectIntent, Data: intent}, {Seq: 2, Kind: JournalEffectResult, Data: result}},
	}
	if err := c.capture("root", "review", 1, JournalInstructionIssued, true); !errors.Is(err, probe.stop) {
		t.Fatalf("checkpoint: %v", err)
	}
	want := filepath.Join("runs", "r", "worktrees", "task")
	if len(probe.request.StateWorktrees) != 1 || probe.request.StateWorktrees[0] != want {
		t.Fatalf("state worktrees = %#v, want %q", probe.request.StateWorktrees, want)
	}
	if len(probe.request.Worktrees) != 0 || probe.request.StateRoot != state || probe.request.Workspace != workspace {
		t.Fatalf("wrong ownership roots: %+v", probe.request)
	}
}

func TestSnapshotDropsAppliedDeferredWorktree(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	probe := &paramWorktreeSnapshotProbe{stop: errors.New("inspected")}
	reg := NewAdapters()
	if err := reg.Register(probe); err != nil {
		t.Fatal(err)
	}
	effect := EffectData{Kind: "external", Adapter: "git", Op: "worktree_add", Args: map[string]any{"path": filepath.Join(workspace, "task")}}
	encode := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	recs := []JournalRecord{{Seq: 1, Kind: JournalEffectIntent, Data: encode(EffectIntentData{Effect: effect})}, {Seq: 2, Kind: JournalEffectResult, Data: encode(EffectResultData{Intent: 1, OK: true})}}
	effect.Op = "worktree_remove"
	recs = append(recs, JournalRecord{Seq: 3, Kind: JournalCleanupDeferred, Data: encode(CleanupDeferredData{Effect: effect})})
	c := &cycle{e: &Engine{reg: reg, opts: EngineOptions{StateRoot: state, RunDir: func(RunID) string { return filepath.Join(state, "runs", "r") }}}, run: "r", st: &runState{started: &RunStartedData{Workspace: workspace}}, recs: recs}
	for _, applied := range []bool{false, true} {
		if applied {
			c.recs = append(c.recs, JournalRecord{Seq: 4, Kind: JournalCleanupApplied, Data: encode(CleanupAppliedData{Deferred: 3})})
		}
		if err := c.capture("", "review", 1, JournalInstructionIssued, true); !errors.Is(err, probe.stop) {
			t.Fatal(err)
		}
		want := 1
		if applied {
			want = 0
		}
		if len(probe.request.Worktrees) != want {
			t.Fatalf("applied=%v worktrees=%v", applied, probe.request.Worktrees)
		}
	}
}

func TestRewindReportsAppliedDeferredRemoval(t *testing.T) {
	effect, _ := json.Marshal(CleanupDeferredData{Effect: EffectData{Adapter: "git", Op: "worktree_remove"}})
	applied, _ := json.Marshal(CleanupAppliedData{Deferred: 1})
	recs := []JournalRecord{{Seq: 1, Kind: JournalCleanupDeferred, State: "Close", Data: effect}, {Seq: 2, Kind: JournalCleanupApplied, Data: applied}}
	if got := appliedDropped(recs, nil); len(got) != 1 {
		t.Fatalf("dropped cleanup warning: %v", got)
	}
	if got := appliedDropped(recs, recs); len(got) != 0 {
		t.Fatalf("retained cleanup warned: %v", got)
	}
}

func TestChildRewindDropsCleanupConfirmationWithDeferral(t *testing.T) {
	deferred, _ := json.Marshal(CleanupDeferredData{Effect: EffectData{Adapter: "git", Op: "worktree_remove"}})
	applied, _ := json.Marshal(CleanupAppliedData{Deferred: 2})
	records := []JournalRecord{{Seq: 1, Kind: JournalNote}, {Seq: 2, Kind: JournalCleanupDeferred, Instance: "child", Data: deferred}, {Seq: 3, Kind: JournalCleanupApplied, Data: applied}}
	kept := forkRecords(records, []cutAt{{inst: "child", from: 1, decided: 1}})
	for _, r := range kept {
		if r.Kind == JournalCleanupApplied {
			t.Fatal("orphaned cleanup confirmation survived rewind")
		}
	}
	if len(appliedDropped(records, kept)) != 1 {
		t.Fatal("rewind did not warn about irreversible cleanup")
	}
}
