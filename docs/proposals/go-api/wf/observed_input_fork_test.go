package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// trackerRuntime is a tracker outside the snapshotted world: a fork reads it
// as it stands, never as it stood at the checkpoint.
type trackerRuntime struct {
	mu    sync.Mutex
	title string
}

func (r *trackerRuntime) set(title string) { r.mu.Lock(); r.title = title; r.mu.Unlock() }
func (*trackerRuntime) Ops() []string      { return []string{"tracker.show"} }
func (r *trackerRuntime) Read(string, wf.ObservationDesc) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.Marshal(obsTicket{Title: r.title})
}
func (*trackerRuntime) Apply(string, wf.ExternalEffect) (json.RawMessage, error) { return nil, nil }
func (*trackerRuntime) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	return wf.EffectDone, nil
}

// observedFanDef maps two items onto a child agent whose prompt binds the
// observed ticket.
func observedFanDef() *wf.Def {
	child := wf.New("observed-child")
	w := wf.NewAgent[chWork, chOut](child)
	ok := wf.NewTerminal[chOK](child, wf.Success)
	bad := wf.NewTerminal[chBad](child, wf.Stopped)
	child.OnFailure(wf.Blocked, bad)
	obs := wf.Observe[obsTicket]("tracker", "show", map[string]any{"id": "T-1"})
	w.Spec(wf.PromptText("do {{item}} on {{TICKET}}", wf.ChildInput(), wf.ObservedAs("TICKET", obs, renderTicket)),
		wf.Retry[chWork](wf.Const(1), w.To(bad, "gave up")))
	toOK := w.To(ok, "ok")
	w.Route(func(chOut, wf.Ctx) wf.Edge[chWork] { return toOK }, toOK)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	d := wf.New("observed-fan")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](child))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// promptFor is the prompt of the agent instruction for item n.
func promptFor(t *testing.T, ins []wf.Instruction, n int) string {
	t.Helper()
	for _, in := range ins {
		if in.Kind == wf.InstructionAgent && strings.Contains(in.Inputs["item"], fmt.Sprintf(`"N":%d`, n)) {
			return in.Prompt
		}
	}
	t.Fatalf("no instruction for item %d in %+v", n, ins)
	return ""
}

// A snapshot fork cut after an item's issue keeps that instruction's
// journaled text; one cut before an item's issue re-reads the input in the
// fork's world, though the source's pre-issue read is in its prefix.
func TestObservedInputAcrossASnapshotFork(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := &trackerRuntime{title: "Fix login"}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, tracker} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := observedFanDef()
	opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
	e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		if p := promptFor(t, ins, n); !strings.Contains(p, "on Fix login []") {
			t.Fatalf("source item %d prompt %q", n, p)
		}
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	// The checkpoint before the second issue: the first item is issued in
	// its prefix, the second has only its pre-issue read.
	var issues int
	var cut int64
	for _, r := range recs {
		switch r.Kind {
		case wf.JournalInstructionIssued:
			issues++
		case wf.JournalWorldSnapshot:
			if issues == 1 && cut == 0 {
				cut = r.Seq
			}
		}
	}
	if cut == 0 {
		t.Fatal("no checkpoint between the two issues")
	}
	tracker.set("Fix logout") // the world moves after the cut
	fork, err := e.ForkSnapshot(run, cut, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	opts.Workspace, opts.StateRoot = fork.Workspace, fork.StateDir
	opts.RunDir = func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }
	forked := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, opts)
	next, err := forked.Next(fork.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	delivered := map[string]string{}
	for _, u := range next {
		if u.Kind != wf.InstructionAgent || u.Name == "" {
			continue
		}
		f, err := forked.(*wf.Engine).PromptFetch(fork.Run, u.Name, "")
		if err != nil {
			t.Fatal(err)
		}
		delivered[u.Child] = f.Prompt + "\ninput: " + f.Inputs["TICKET"]
	}
	if p := delivered["chFan[1]"]; !strings.Contains(p, "on Fix login []") || !strings.Contains(p, "input: Fix login []") {
		t.Fatalf("child issued before the cut delivers %q, want the journaled ticket", p)
	}
	if p := delivered["chFan[2]"]; !strings.Contains(p, "on Fix logout []") || !strings.Contains(p, "input: Fix logout []") {
		t.Fatalf("child issued after the cut delivers %q, want the fork's world read", p)
	}
}
