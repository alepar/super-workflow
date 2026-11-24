package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// scratchIn is the {scratch} path a prompt "s=[{{scratch}}]" rendered.
func scratchIn(t *testing.T, prompt string) string {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, "s=[")
	p, _, ok2 := strings.Cut(rest, "]")
	if !ok || !ok2 || p == "" {
		t.Fatalf("no scratch path in %q", prompt)
	}
	return p
}

// {scratch} is a directory the engine made for this instance's state before
// the instruction is handed out, under the run directory (an earlier revision: agents ran
// mkdir -p, a denied command). It stays the same across attempts.
func TestScratchBindsADirectoryThatExistsAndIsStableAcrossAttempts(t *testing.T) {
	d := wf.New("scratch-attempts")
	a := wf.NewAgent[scA, string](d)
	done := wf.NewTerminal[scDone](d, wf.Success)
	d.OnFailure(wf.Blocked, done)
	a.Spec(wf.PromptText("s=[{{scratch}}]"), wf.Retry[scA](wf.Const(2), a.To(done, "gave up")))
	fin := a.To(done, "done")
	a.Route(func(string, wf.Ctx) wf.Edge[scA] { return fin }, fin)
	runDir := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions %+v", ins)
	}
	p := scratchIn(t, ins[0].Prompt)
	if !strings.HasPrefix(p, filepath.Join(runDir, "scratch")+"/") {
		t.Fatalf("scratch %s, want it under %s/scratch", p, runDir)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Fatalf("scratch %s does not exist as a directory: %v", p, err)
	}
	// A failed attempt is retried; the next attempt keeps the path.
	if _, err := h.e.(*wf.Engine).Fail(run, ins[0].Token, "try again"); err != nil {
		t.Fatal(err)
	}
	again := h.next(run)
	if len(again) != 1 || again[0].Token == ins[0].Token {
		t.Fatalf("no new attempt: %+v", again)
	}
	if q := scratchIn(t, again[0].Prompt); q != p {
		t.Fatalf("the retry's scratch %s, want %s", q, p)
	}
}

// Without a run directory {scratch} has nowhere to live: binding it fails
// the step with that reason, never a relative path.
func TestScratchNeedsARunDir(t *testing.T) {
	d := tierDef("s=[{{scratch}}]")
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{})
	run := h.run(d, nil)
	if _, err := h.e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, r := range h.kind(run, wf.JournalRefused) {
		named = named || strings.Contains(string(r.Data), "{scratch} needs the engine's RunDir")
	}
	if !named {
		t.Fatalf("no refusal names the missing RunDir: %v", h.records(run))
	}
}

// scratch is a reserved name: a run parameter cannot shadow the builtin.
func TestScratchIsReserved(t *testing.T) {
	d := tierDef("s=[{{scratch}}]")
	d.Param("scratch")
	var named bool
	for _, p := range d.Lint() {
		named = named || strings.Contains(p.Msg, "scratch is an engine binding")
	}
	if !named {
		t.Fatalf("lint %v, want scratch refused as a parameter", d.Lint())
	}
}

type (
	scA    struct{}
	scB    struct{}
	scDone struct{}
)

// A fork never copies scratch: it is not run history. The fork's {scratch}
// is its own, under its run directory.
func TestScratchIsNotCopiedByAFork(t *testing.T) {
	d := wf.New("scratch-fork")
	a := wf.NewAgent[scA, string](d)
	b := wf.NewAgent[scB, string](d)
	done := wf.NewTerminal[scDone](d, wf.Success)
	a.Spec(wf.PromptText("s=[{{scratch}}]"), wf.Retry[scA](wf.Const(1), a.To(done, "stop")))
	toB := a.To(b, "next")
	a.Route(func(string, wf.Ctx) wf.Edge[scA] { return toB }, toB)
	b.Spec(wf.PromptText("s=[{{scratch}}]"), wf.Retry[scB](wf.Const(1), b.To(done, "stop")))
	fin := b.To(done, "done")
	b.Route(func(string, wf.Ctx) wf.Edge[scB] { return fin }, fin)
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
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
	if err != nil || len(ins) != 1 {
		t.Fatalf("a: %+v %v", ins, err)
	}
	aScratch := scratchIn(t, ins[0].Prompt)
	if err := os.WriteFile(filepath.Join(aScratch, "notes.txt"), []byte("work in progress"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
		t.Fatal(err)
	}
	if ins, err = e.Next(run, ""); err != nil || len(ins) != 1 || ins[0].State != "scB" {
		t.Fatalf("b: %+v %v", ins, err)
	}
	if scratchIn(t, ins[0].Prompt) == aScratch {
		t.Fatal("two states share a scratch directory")
	}
	recs, _ := wf.ReadJournal(state, run)
	var seq int64
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot && r.State == "scB" {
			seq = r.Seq
		}
	}
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	forkRunDir := filepath.Join(fork.StateDir, "runs", string(run))
	if _, err := os.Stat(filepath.Join(forkRunDir, "scratch")); !os.IsNotExist(err) {
		t.Fatalf("the fork has a scratch tree: %v", err)
	}
	fe := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: fork.Workspace, StateRoot: fork.StateDir, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }})
	fins, err := fe.Next(run, "")
	if err != nil || len(fins) != 1 {
		t.Fatalf("fork b: %+v %v", fins, err)
	}
	if p := scratchIn(t, fins[0].Prompt); !strings.HasPrefix(p, filepath.Join(forkRunDir, "scratch")+"/") {
		t.Fatalf("the fork's scratch %s, want it under %s/scratch", p, forkRunDir)
	}
}

type (
	snOut  struct{ Scratch string }
	snWork struct{}
	snNext struct{}
)

// {scratch} is the engine's, whatever else is named scratch (an earlier revision review
// round 1, P1): a run param named scratch is refused at Run, and an output
// field or a prompt input named scratch never stands in for the builtin, in
// a prompt or bound whole into a command argument.
func TestScratchIsNeverShadowed(t *testing.T) {
	d := tierDef("s=[{{scratch}}]")
	runDir := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	if _, err := h.e.Run(d, map[string]string{"scratch": "/elsewhere"}, nil); err == nil || !strings.Contains(err.Error(), "scratch") {
		t.Fatalf("a scratch param: %v", err)
	}

	d = wf.New("scratch-shadow")
	work := wf.NewAgent[snWork, snOut](d)
	next := wf.NewAgent[snNext, string](d)
	done := wf.NewTerminal[scDone](d, wf.Success)
	work.Spec(wf.PromptText("go"), wf.Retry[snWork](wf.Const(1), work.To(done, "stop")))
	toNext := work.To(next, "next")
	work.Route(func(snOut, wf.Ctx) wf.Edge[snWork] { return toNext }, toNext)
	next.Spec(wf.PromptText("s=[{{scratch}}]", wf.Bound("scratch", "/a/prompt/input")), wf.Retry[snNext](wf.Const(1), next.To(done, "stop")))
	fin := next.To(done, "done")
	next.Route(func(string, wf.Ctx) wf.Edge[snNext] { return fin }, fin)
	h = newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	run := h.run(d, nil)
	ins := h.next(run)
	h.submit(run, ins[0], `{"Scratch":"/an/output/field"}`)
	ins = h.next(run)
	if len(ins) != 1 {
		t.Fatalf("next %+v", ins)
	}
	if p := scratchIn(t, ins[0].Prompt); !strings.HasPrefix(p, filepath.Join(runDir, "scratch")+"/") {
		t.Fatalf("scratch rendered %s, want the engine's directory", p)
	}
}

// A journal from before the builtin may carry a scratch path parameter.
// Binding a new whole argument {scratch} on resume is still the engine's
// directory, never that parameter's typed marker (an earlier revision review round 2).
func TestScratchBeatsALegacyScratchPathParam(t *testing.T) {
	d := wf.New("scratch-legacy")
	exits := wf.NewEnum(d, "d-exit", dExit(0), dExit(1), dExit(124), dExit(125))
	cmd := wf.NewCommand[dRun](d, "ls", "{scratch}")
	ok := wf.NewTerminal[dOK](d, wf.Success)
	fail := wf.NewTerminal[dFail](d, wf.Stopped)
	cmd.Key("ls")
	cmd.Idempotent()
	cmd.RouteCases(wf.Cases[dRun, wf.RunResult](wf.ExitAs[dExit], exits,
		wf.On(dExit(0), cmd.To(ok, "passes")),
		wf.On(dExit(1), cmd.To(fail, "fails")),
		wf.On(dExit(124), cmd.To(fail, "timed out")),
		wf.On(dExit(125), cmd.To(fail, "runner failed"))))
	runDir := t.TempDir()
	h := newHarnessOpts(t, d, newWorld(), wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	unlock, err := h.j.Lock("old")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(wf.RunStartedData{Def: d.Name(), Harness: "claude",
		Params: map[string]string{"scratch": "legacy-dir"}, ParamPaths: map[string]wf.PathBase{"scratch": wf.RunRelative}})
	err = h.j.Append(wf.JournalRecord{Kind: wf.JournalRunStarted, Run: "old", DefHash: d.Hash(), At: h.now, Data: data})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Next("old", ""); err != nil {
		t.Fatal(err)
	}
	intents := h.kind("old", wf.JournalEffectIntent)
	if len(intents) != 1 {
		t.Fatalf("effect intents %d", len(intents))
	}
	var intent wf.EffectIntentData
	if err := json.Unmarshal(intents[0].Data, &intent); err != nil {
		t.Fatal(err)
	}
	argv, _ := intent.Effect.Args["argv"].([]any)
	if len(argv) != 2 {
		t.Fatalf("argv %v", argv)
	}
	got, isString := argv[1].(string)
	if !isString || !strings.HasPrefix(got, filepath.Join(runDir, "scratch")+"/") {
		t.Fatalf("argv[1] = %v, want the engine's scratch directory, not the legacy parameter", argv[1])
	}
}
