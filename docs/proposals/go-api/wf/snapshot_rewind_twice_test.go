package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	rrStart  struct{}
	rrSetup  struct{}
	rrChild  struct{}
	rrDone   struct{}
	rrStop   struct{}
	rrOffer  struct{}
	rrSecond struct{}
	rrEnd    struct{}
	rrFailed struct{}
)

// rrInput copies the run's records root into a child's input as a plain
// string, as a definition that resolves {records} once and hands it down
// does. Its field shares the builtin's name.
type rrInput struct {
	Records string `json:"records"`
}

// rewindTwiceDef: an agent (the first fork's checkpoint), a Compute whose
// route writes a receipt to a literal path under the records root, then a
// child that offers an agent (the second fork's checkpoint) and runs a
// command on {records}.
func rewindTwiceDef() *wf.Def {
	child := wf.New("rr-child")
	offer := wf.NewAgent[rrOffer, string](child)
	second := wf.NewCommand[rrSecond](child, "cat", wf.PathTemplate("{records}/note"))
	end := wf.NewTerminal[rrEnd](child, wf.Success)
	failed := wf.NewTerminal[rrFailed](child, wf.Stopped)
	child.OnFailure(wf.Blocked, failed)
	offer.Spec(wf.PromptText("go on"), wf.Retry[rrOffer](wf.Const(1), offer.To(failed, "gave up")))
	offer.ThenTo(second, "next")
	second.RouteExit(second.To(end, "ran"), second.To(failed, "red"))
	wf.Yield(end, func(wf.Ctx) string { return "ok" })
	wf.Yield(failed, func(wf.Ctx) string { return "" })

	d := wf.New("rewind-twice")
	start := wf.NewAgent[rrStart, string](d)
	setup := wf.NewCompute[rrSetup](d, func(c wf.Ctx) (string, error) {
		return wf.ResolvePath(c, wf.PathTemplate("{records}/receipt"))
	})
	sub := wf.NewSub[rrChild](d, wf.Typed[rrInput, string](child), func(c wf.Ctx) rrInput {
		records, _ := wf.ResolvePath(c, wf.PathTemplate("{records}"))
		return rrInput{Records: records}
	})
	done := wf.NewTerminal[rrDone](d, wf.Success)
	stop := wf.NewTerminal[rrStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	start.Spec(wf.PromptText("start"), wf.Retry[rrStart](wf.Const(1), start.To(stop, "gave up")))
	start.ThenTo(setup, "started")
	toChild := setup.To(sub, "receipt written")
	setup.Route(func(path string, _ wf.Ctx) wf.Edge[rrSetup] {
		return toChild.With(shell.Effect(wf.KeyOf("rr-receipt"), shell.RunWith("touch", wf.PathText(path))))
	}, toChild)
	sub.Exits(wf.OnExit[rrEnd](sub.To(done, "child done")), wf.OnExit[rrFailed](sub.To(stop, "child stopped")))
	return d
}

// A fork of a fork (sw-3b1q): the first fork resolves its records root into
// a literal receipt path and a child's input; the second fork, cut inside
// that child, runs the child's {records} command on its own records root.
// The first fork's literal receipt path, in a plan its transition consumed,
// does not refuse the second fork, and the input field named "records" does
// not shadow the engine's {records}.
func TestRewindOfARewindBindsItsOwnRecords(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	sh := &recordingShell{print: func(string) string { return "" }}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, sh} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := rewindTwiceDef()
	engineAt := func(stateDir, workspace string) *wf.Engine {
		opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: workspace, StateRoot: stateDir, Snapshots: true,
			RunDir: func(r wf.RunID) string { return filepath.Join(stateDir, "runs", string(r)) }}
		return wf.NewEngine(wf.FileJournal(stateDir), reg, opts).(*wf.Engine)
	}
	checkpoint := func(stateDir string, run wf.RunID, at string) int64 {
		t.Helper()
		recs, err := wf.ReadJournal(stateDir, run)
		if err != nil {
			t.Fatal(err)
		}
		var seq int64
		for _, r := range recs {
			if r.Kind == wf.JournalWorldSnapshot && strings.HasSuffix(r.State, at) {
				seq = r.Seq
			}
		}
		if seq == 0 {
			t.Fatalf("no checkpoint at %s", at)
		}
		return seq
	}
	submit := func(e *wf.Engine, run wf.RunID, state string) {
		t.Helper()
		ins, err := e.Next(run, "")
		if err != nil || len(ins) != 1 || !strings.HasSuffix(ins[0].State, state) {
			t.Fatalf("next to %s: %+v %v", state, ins, err)
		}
		if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
			t.Fatal(err)
		}
	}

	e := engineAt(state, ws)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ins, err := e.Next(run, ""); err != nil || len(ins) != 1 || ins[0].State != "rrStart" {
		t.Fatalf("start: %+v %v", ins, err)
	}
	first, err := e.ForkSnapshot(run, checkpoint(state, run, "rrStart"), filepath.Join(t.TempDir(), "first"))
	if err != nil {
		t.Fatalf("first fork: %v", err)
	}

	fe := engineAt(first.StateDir, first.Workspace)
	submit(fe, run, "rrStart")
	if ins, err := fe.Next(run, ""); err != nil || len(ins) != 1 || !strings.HasSuffix(ins[0].State, "rrOffer") {
		t.Fatalf("first fork's offer: %+v %v", ins, err)
	}
	second, err := fe.ForkSnapshot(run, checkpoint(first.StateDir, run, "rrOffer"), filepath.Join(t.TempDir(), "second"))
	if err != nil {
		t.Fatalf("fork of the fork: %v", err)
	}

	se := engineAt(second.StateDir, second.Workspace)
	before := len(sh.runs)
	submit(se, run, "rrOffer")
	if _, err := se.Next(run, ""); err != nil {
		t.Fatalf("next into the command: %v", err)
	}
	recs, _ := wf.ReadJournal(second.StateDir, run)
	for _, r := range recs {
		if strings.Contains(string(r.Data), "a root of the run this one was forked from") {
			t.Fatalf("second fork refused its own records: journal %d %s", r.Seq, r.Data)
		}
	}
	if len(sh.runs) <= before {
		t.Fatal("the second fork's command did not run")
	}
	argv, _ := sh.runs[len(sh.runs)-1]["argv"].([]any)
	want := filepath.Join(second.StateDir, "runs", string(run), "records", "note")
	if len(argv) != 2 || argv[1] != want {
		t.Fatalf("command argv %v, want cat %s", argv, want)
	}
}
