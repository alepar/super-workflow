package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// snapshotFanEngine is a file-journal engine with snapshots on, over a Map of
// n one-agent children, and the runtime that counts captures.
func snapshotFanEngine(t *testing.T, n int) (*wf.Engine, *wf.Def, *snapshotRuntime, string, string) {
	t.Helper()
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	rt := &snapshotRuntime{}
	reg := wf.NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	ns := make([]int, n)
	for i := range ns {
		ns[i] = i + 1
	}
	d := fanDef("snap-fan", chItems(ns...), n, wf.AllSettled())
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	return e, d, rt, ws, state
}

// One cycle that issues a whole fan-out captures the world once: nothing
// changes it between the steps, so each later step's checkpoint names the
// same capture instead of copying the world again (FANOUT-2 run B: a done
// that drove 147 steps held the run lock ~80 s, one capture per step).
func TestSnapshotOneCapturePerUnchangedCycle(t *testing.T) {
	e, d, rt, ws, state := snapshotFanEngine(t, 5)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := rt.captures
	ins, err := e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 5 {
		t.Fatalf("want 5 units, got %d", len(ins))
	}
	if got := rt.captures - before; got != 1 {
		t.Fatalf("captures in the fan-out cycle = %d, want 1", got)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	var issued int
	var last wf.JournalRecord
	var forkAt int64
	for _, r := range recs {
		if r.Kind == wf.JournalInstructionIssued {
			issued++
			if last.Kind != wf.JournalWorldSnapshot {
				t.Fatalf("instruction %d not preceded by a world snapshot (seq %d)", issued, r.Seq)
			}
			var d wf.WorldSnapshotData
			if err := json.Unmarshal(last.Data, &d); err != nil || d.Unavailable != "" || d.Directory == "" || d.Before != wf.JournalInstructionIssued {
				t.Fatalf("snapshot before issue %d: %+v %v", issued, d, err)
			}
			dirs[d.Directory] = true
			if issued == 3 {
				forkAt = last.Seq
			}
		}
		last = r
	}
	if issued != 5 || len(dirs) != 1 {
		t.Fatalf("issued %d, capture directories %v", issued, dirs)
	}
	entries, err := os.ReadDir(filepath.Join(state, "runs", string(run), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 2 { // the start step's capture and the fan-out's
		t.Fatalf("snapshot directories on disk: %d", len(entries))
	}
	if plan, err := e.PlanFanout(run, "all"); err != nil || len(plan.Targets) != 5 {
		t.Fatalf("fanout over reused checkpoints: %d targets, %v", len(plan.Targets), err)
	}
	// A reused checkpoint forks like any other: the world it names is the
	// world at that step.
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	fork, err := e.ForkSnapshot(run, forkAt, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(fork.Workspace, "data")); err != nil || string(got) != "before" {
		t.Fatalf("fork world %q %v", got, err)
	}
}

type (
	srWork struct{}
	srOK   struct{}
	srBad  struct{}
	srFan  struct{}
	srDone struct{}
	srPart struct{}
)

// A fan-out whose children each allocate an artifact: issuing the second
// creates its records/artifacts directory, which the first unit's capture
// lacks, so the second checkpoint captures again and a fork at it holds the
// directory its producer writes into.
func TestSnapshotReuseSeesRunDataCreatedInTheCycle(t *testing.T) {
	child := wf.New("sr-child")
	w := wf.NewAgent[srWork, chOut](child)
	ok := wf.NewTerminal[srOK](child, wf.Success)
	bad := wf.NewTerminal[srBad](child, wf.Stopped)
	child.OnFailure(wf.Blocked, bad)
	wf.NewArtifact[string](w, "report.md")
	w.Spec(wf.PromptText("write {{item}}", wf.ChildInput()), wf.Retry[srWork](wf.Const(1), w.To(bad, "gave up")))
	toOK := w.To(ok, "ok")
	w.Route(func(chOut, wf.Ctx) wf.Edge[srWork] { return toOK }, toOK)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	d := wf.New("sr-fan")
	m := wf.NewMap[srFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](child))
	m.Parallel(2).Join(wf.AllSettled(), m.To(wf.NewTerminal[srDone](d, wf.Success), "met"), m.To(wf.NewTerminal[srPart](d, wf.Stopped), "partial"))

	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 2 {
		t.Fatalf("next: %d %v", len(ins), err)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var issued int
	var forkAt int64
	var last wf.JournalRecord
	for _, r := range recs {
		if r.Kind == wf.JournalInstructionIssued {
			if issued++; issued == 2 {
				forkAt = last.Seq
			}
		}
		last = r
	}
	fork, err := e.ForkSnapshot(run, forkAt, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	source, _ := filepath.Glob(filepath.Join(state, "runs", string(run), "records", "artifacts", "*"))
	forked, _ := filepath.Glob(filepath.Join(fork.StateDir, "runs", string(run), "records", "artifacts", "*"))
	if len(source) != 2 || len(forked) != 2 {
		t.Fatalf("artifact directories: source %d, fork at the second issue %d; want 2 and 2", len(source), len(forked))
	}
	for _, dir := range forked {
		if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("x"), 0o644); err != nil {
			t.Errorf("the producer cannot write its allocated path in the fork: %v", err)
		}
	}
}

// A writer outside the lock changes a run-data file during a capture's copy
// and changes it back: the capture holds the changed bytes, the live file
// the first ones, so the next checkpoint of the cycle captures again and a
// fork at each checkpoint holds what the source held there.
func TestSnapshotReuseComparesTheCopiedBytes(t *testing.T) {
	e, d, _, _, state := snapshotFanEngine(t, 2)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(state, "runs", string(run), "records", "f")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	fired := false
	defer wf.SetSnapshotRunDataHook(func(when string) {
		if fired {
			return
		}
		switch when {
		case "before-copy":
			_ = os.WriteFile(f, []byte("B"), 0o644)
		case "after-copy":
			_ = os.WriteFile(f, []byte("A"), 0o644)
			fired = true
		}
	})()
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints []int64
	for i, r := range recs {
		if r.Kind == wf.JournalInstructionIssued && i > 0 && recs[i-1].Kind == wf.JournalWorldSnapshot {
			checkpoints = append(checkpoints, recs[i-1].Seq)
		}
	}
	if len(checkpoints) != 2 {
		t.Fatalf("issue checkpoints %v", checkpoints)
	}
	for i, want := range []string{"B", "A"} {
		fork, err := e.ForkSnapshot(run, checkpoints[i], filepath.Join(t.TempDir(), "fork"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(fork.StateDir, "runs", string(run), "records", "f"))
		if err != nil || string(got) != want {
			t.Errorf("fork at issue %d: records/f %q %v, want %q", i+1, got, err, want)
		}
	}
}
