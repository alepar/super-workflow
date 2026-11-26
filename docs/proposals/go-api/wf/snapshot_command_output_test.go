package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	coSuite struct{}
	coAfter struct{}
	coDone  struct{}
	coStop  struct{}
)

// coDef is a round suite in small: a command whose output names an
// absolute path under the workspace (as examplewf's suite-measurement line
// names its client root and artifact directory), then an agent offer.
func coDef() *wf.Def {
	d := wf.New("command-output")
	suite := wf.NewCommand[coSuite](d, "sh", "-c", `printf 'suite-measurement: {"root":"%s"}\n' "$PWD"`)
	after := wf.NewAgent[coAfter, string](d)
	done := wf.NewTerminal[coDone](d, wf.Success)
	stop := wf.NewTerminal[coStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	suite.RouteExit(suite.To(after, "green"), suite.To(stop, "red"))
	after.Spec(wf.PromptText("after"), wf.Retry[coAfter](wf.Const(1), after.To(stop, "gave up")))
	fin := after.To(done, "done")
	after.Route(func(string, wf.Ctx) wf.Edge[coAfter] { return fin }, fin)
	return d
}

// A command's accepted output is what it printed: history, like an
// observation's value, never a path declaration. A run forked past it
// relocates (an earlier revision; the regression case journal 2469, the regression case/rewind1 5539: a
// RoundSuite output naming the source roots refused every later fork).
func TestForkPastACommandWhoseOutputNamesTheSourceRoots(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, shell.Runtime(shell.Config{})} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := coDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
		t.Fatalf("offers after the suite: %+v", ins)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	var named bool
	for _, r := range recs {
		if r.Kind == wf.JournalAccepted && r.State == "coSuite" {
			named = strings.Contains(string(r.Data), ws)
		}
		if r.Kind == wf.JournalWorldSnapshot {
			var s wf.WorldSnapshotData
			if json.Unmarshal(r.Data, &s) == nil && s.Unavailable == "" {
				seq = r.Seq
			}
		}
	}
	if !named {
		t.Fatal("the suite's output does not name the workspace: the test proves nothing")
	}
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatalf("fork past the suite: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fork.Workspace, "data")); err != nil {
		t.Fatalf("the fork's world: %v", err)
	}
}

// recordingShell is a shell adapter that records each run's args and
// prints what print returns for the workspace it runs in; it also answers
// probe.exists (false), recording the descriptor's args.
type recordingShell struct {
	print  func(ws string) string
	runs   []map[string]any
	probes []map[string]any
}

func (*recordingShell) Ops() []string { return []string{"shell.run", "probe.exists"} }
func (r *recordingShell) Read(_ string, d wf.ObservationDesc) (json.RawMessage, error) {
	r.probes = append(r.probes, d.Args)
	return json.RawMessage("false"), nil
}
func (r *recordingShell) Apply(ws string, e wf.ExternalEffect) (json.RawMessage, error) {
	r.runs = append(r.runs, e.Args)
	return json.Marshal(wf.RunResult{Stdout: r.print(ws)})
}
func (*recordingShell) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	return wf.EffectNotDone, nil
}

// ApplyAt stands in for the shell runtime's: it takes the run's context, so
// a fork (a sandboxed run) runs it rather than refusing it.
func (r *recordingShell) ApplyAt(ws string, _ wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, error) {
	return r.Apply(ws, e)
}

type (
	ccFirst  struct{}
	ccOffer  struct{}
	ccSecond struct{}
	ccDone   struct{}
	ccStop   struct{}
	ccBrief  struct{}
	ccRead   struct{}
)

// continuation is how the second command of continuationDef binds history.
type continuation struct {
	argv     []string
	dir      string
	doneWhen map[string]any // a probe.exists DoneWhen's args, when set
	prompt   string         // when set, the second step is an agent with this prompt file
	// artifact, when set, is a legacy artifact template declared on the
	// offer; the second step uses it as use says: "read" (a Compute reads
	// its content) or "prompt" (an agent's prompt input).
	artifact, use string
	// bound, with use "bound" or "shell-bound", is a wf.Bound or
	// wf.ShellBound prompt input SPEC of the second step, an agent.
	bound string
	// prepare, when set, runs on the fork's and the source's workspaces
	// after the fork is taken, before it is driven.
	prepare func(forkWS, sourceWS string)
}

// continuationDef: a command prints a path, an agent offer (where a fork is
// taken), then a command bound as k says.
func continuationDef(k continuation) *wf.Def {
	d := wf.New("continuation")
	first := wf.NewCommand[ccFirst](d, "print-root")
	offer := wf.NewAgent[ccOffer, string](d)
	done := wf.NewTerminal[ccDone](d, wf.Success)
	stop := wf.NewTerminal[ccStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	first.RouteExit(first.To(offer, "printed"), first.To(stop, "red"))
	offer.Spec(wf.PromptText("go on"), wf.Retry[ccOffer](wf.Const(1), offer.To(stop, "gave up")))
	if k.use == "read" {
		art := wf.NewArtifact[string](offer, "notes", k.artifact)
		read := wf.NewCompute[ccRead, string](d, func(c wf.Ctx) (string, error) { return wf.Read(c, art) })
		next := offer.To(read, "next")
		offer.Route(func(string, wf.Ctx) wf.Edge[ccOffer] { return next }, next)
		fin := read.To(done, "read")
		read.Route(func(string, wf.Ctx) wf.Edge[ccRead] { return fin }, fin)
		return d
	}
	if k.prompt != "" || k.use == "prompt" || k.use == "bound" || k.use == "shell-bound" {
		brief := wf.NewAgent[ccBrief, string](d)
		next := offer.To(brief, "next")
		offer.Route(func(string, wf.Ctx) wf.Edge[ccOffer] { return next }, next)
		p := wf.PromptFile(k.prompt)
		if k.use == "prompt" {
			p = wf.PromptText("brief", wf.NewArtifact[string](offer, "notes", k.artifact))
		}
		if k.use == "bound" {
			p = wf.PromptText("brief", wf.Bound("SPEC", k.bound))
		}
		if k.use == "shell-bound" {
			p = wf.PromptText("cat [SPEC]", wf.ShellBound("SPEC", k.bound))
		}
		brief.Spec(p, wf.Retry[ccBrief](wf.Const(1), brief.To(stop, "gave up")))
		fin := brief.To(done, "done")
		brief.Route(func(string, wf.Ctx) wf.Edge[ccBrief] { return fin }, fin)
		return d
	}
	second := wf.NewCommand[ccSecond](d, k.argv...)
	if k.dir != "" {
		second.In(k.dir)
	}
	if k.doneWhen != nil {
		wf.DoneWhen(second, wf.Observe[bool]("probe", "exists", k.doneWhen), "exists")
	}
	next := offer.To(second, "next")
	offer.Route(func(string, wf.Ctx) wf.Edge[ccOffer] { return next }, next)
	second.RouteExit(second.To(done, "ran"), second.To(stop, "red"))
	return d
}

// forked is a continuation run in its fork.
type forked struct {
	fork   wf.SnapshotFork
	run    wf.RunID
	sh     *recordingShell
	second map[string]any // the second command's args; nil when it never ran
	recs   []wf.JournalRecord
	ins    []wf.Instruction // what the fork offered after the offer was done
}

// continueFork forks the run past its first command at the agent offer and
// drives the fork into the second command. The first command prints
// print(its workspace, the source state root).
func continueFork(t *testing.T, k continuation, print func(ws, sourceState string) string) forked {
	t.Helper()
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	sh := &recordingShell{print: func(at string) string { return print(at, state) }}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, sh} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := continuationDef(k)
	opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
	e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ins, err := e.Next(run, ""); err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
		t.Fatalf("offer: %+v %v", ins, err)
	}
	recs, _ := wf.ReadJournal(state, run)
	var seq int64
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot && r.State == "ccOffer" {
			seq = r.Seq
		}
	}
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if k.prepare != nil {
		k.prepare(fork.Workspace, ws)
	}
	opts.Workspace, opts.StateRoot = fork.Workspace, fork.StateDir
	opts.RunDir = func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }
	fe := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, opts)
	before := len(sh.runs)
	ins, err := fe.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
		t.Fatalf("fork offer: %+v %v", ins, err)
	}
	if _, err := fe.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
		t.Fatal(err)
	}
	after, err := fe.Next(run, "")
	if err != nil {
		t.Fatalf("next into the second step: %v", err)
	}
	f := forked{fork: fork, run: run, sh: sh, ins: after}
	if f.recs, err = wf.ReadJournal(fork.StateDir, run); err != nil {
		t.Fatal(err)
	}
	if len(sh.runs) > before {
		f.second = sh.runs[len(sh.runs)-1]
	}
	return f
}

// refusedSecond reports whether the fork journaled the second step's
// refusal for naming a source root, at field.
func (f forked) refusedSecond(field string) bool {
	for _, r := range f.recs {
		if (r.State == "ccSecond" || r.State == "ccBrief" || r.State == "ccRead") && strings.Contains(string(r.Data), "a root of the run this one was forked from") && strings.Contains(string(r.Data), field) {
			return true
		}
	}
	return false
}

// In a fork, a value from history that names a source root anywhere, bound
// into an operational field, refuses the step and never reaches an adapter
// (an earlier revision review rounds 1 and 2: the source workspace reached a command's
// dir, a DoneWhen descriptor, and -I<source>/include; a sibling
// '<source>,backup' is refused too). Nothing is rewritten.
func TestForkRefusesSourceRootsInOperationalFields(t *testing.T) {
	ws := func(at, _ string) string { return at }
	for name, c := range map[string]struct {
		k     continuation
		print func(ws, state string) string
		field string
	}{
		"dir":            {continuation{argv: []string{"true"}, dir: "{Stdout}"}, ws, "command.dir"},
		"argv":           {continuation{argv: []string{"cat", "{Stdout}/x"}}, ws, "command.argv[1]"},
		"include flag":   {continuation{argv: []string{"cc", "-I{Stdout}/include"}}, ws, "command.argv[1]"},
		"sibling":        {continuation{argv: []string{"cat", "{Stdout},backup"}}, ws, "command.argv[1]"},
		"state":          {continuation{argv: []string{"cat", "{Stdout}"}}, func(_, state string) string { return filepath.Join(state, "elsewhere") }, "command.argv[1]"},
		"donewhen probe": {continuation{argv: []string{"true"}, doneWhen: map[string]any{"path": "{Stdout}/x"}}, ws, "observation probe.exists.path"},
		// A path template bound after the fork to a source path is an
		// external owned path: its resolved target is checked (an earlier revision
		// review round 1, P1).
		"path template": {continuation{argv: []string{"cat", wf.PathTemplate("{Stdout}/x")}}, ws, "command.argv[1]"},
	} {
		t.Run(name, func(t *testing.T) {
			f := continueFork(t, c.k, c.print)
			if f.second != nil {
				t.Fatalf("the second command ran with %v", f.second)
			}
			for _, p := range f.sh.probes {
				if strings.Contains(fmt.Sprint(p), "/x") {
					t.Fatalf("a probe ran with %v", p)
				}
			}
			if !f.refusedSecond(c.field) {
				t.Fatalf("no refusal naming %s", c.field)
			}
		})
	}
}

// The fork's own paths pass: a later command bound to {records} and {workspace}
// (this run's roots, even where they lie below a source root) runs.
func TestForkOwnRootsPass(t *testing.T) {
	f := continueFork(t, continuation{argv: []string{"cat", "{records}/x", "{workspace}/y"}}, func(at, _ string) string { return "plain" })
	if f.second == nil {
		t.Fatal("the second command did not run")
	}
	argv, _ := f.second["argv"].([]any)
	if len(argv) != 3 || !strings.HasPrefix(fmt.Sprint(argv[1]), f.fork.StateDir) || argv[2] != f.fork.Workspace+"/y" {
		t.Fatalf("argv %v", argv)
	}
}

// A command's full output, cut from a long stream, is carried into the fork
// at its relocated path (an earlier revision review round 2, P2: the fork's journal
// named <fork-run>/effects/N.out, which did not exist).
func TestForkCarriesACommandsFullOutput(t *testing.T) {
	long := strings.Repeat("x", wf.OutputTail+1024)
	f := continueFork(t, continuation{argv: []string{"true"}}, func(_, _ string) string { return long })
	var out string
	for _, r := range f.recs {
		if r.Kind == wf.JournalAccepted && r.State == "ccFirst" {
			var a wf.AcceptedData
			var rr wf.RunResult
			if json.Unmarshal(r.Data, &a) == nil && json.Unmarshal(a.Output, &rr) == nil {
				out = rr.Output
			}
		}
	}
	if out == "" || !strings.HasPrefix(out, f.fork.StateDir) {
		t.Fatalf("the fork's full-output path %q", out)
	}
	body, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(body), long) {
		t.Fatalf("the fork's full output: %d bytes, %v", len(body), err)
	}
}

// In a fork, a prompt file whose path is bound from history to a source path
// is refused before it is read (an earlier revision review round 1, P2): the agent would
// be briefed from the source run's file.
func TestForkRefusesAPromptFileUnderTheSource(t *testing.T) {
	f := continueFork(t, continuation{prompt: "{Stdout}/brief.md"}, func(at, _ string) string {
		if err := os.WriteFile(filepath.Join(at, "brief.md"), []byte("SOURCE BRIEF"), 0600); err != nil {
			t.Fatal(err)
		}
		return at
	})
	for _, in := range f.ins {
		if strings.Contains(in.Prompt, "SOURCE BRIEF") {
			t.Fatalf("the fork briefed from the source's file: %+v", in)
		}
	}
	if !f.refusedSecond("prompt file") {
		t.Fatalf("no refusal naming the prompt file; offered %+v", f.ins)
	}
}

// In a fork, a typed path a later step uses is checked at its resolved
// target before it is opened or handed to an agent (an earlier revision review round 1,
// P2): a legacy artifact template bound from history to a source path is
// neither read by a Compute nor given as a prompt input.
func TestForkRefusesAnArtifactUnderTheSource(t *testing.T) {
	write := func(at, _ string) string {
		if err := os.WriteFile(filepath.Join(at, "notes.md"), []byte("SOURCE NOTES"), 0600); err != nil {
			t.Fatal(err)
		}
		return at
	}
	for use, field := range map[string]string{"read": "artifact notes", "prompt": "prompt input notes"} {
		t.Run(use, func(t *testing.T) {
			f := continueFork(t, continuation{artifact: "{Stdout}/notes.md", use: use}, write)
			for _, r := range f.recs {
				if r.Kind == wf.JournalAccepted && strings.Contains(string(r.Data), "SOURCE NOTES") {
					t.Fatalf("the fork read the source's artifact: %s", r.Data)
				}
			}
			for _, in := range f.ins {
				if in.Kind == wf.InstructionAgent && in.State == "ccBrief" {
					t.Fatalf("the fork briefed with the source's artifact: %+v", in)
				}
			}
			if !f.refusedSecond(field) {
				t.Fatalf("no refusal naming %s; offered %+v", field, f.ins)
			}
		})
	}
}

// A path marked in a wf.Bound prompt input, whole or inside prose, keeps its
// path semantics: in a fork its bound target is checked before the prompt is
// presented (an earlier revision final review, P1). An unmarked citation of the same
// value passes: it is text the agent reads. A wf.ShellBound value is an
// argument the agent runs, so it is checked whole, marked or not.
func TestForkChecksMarkedPathsInABoundPromptInput(t *testing.T) {
	ws := func(at, _ string) string { return at }
	for name, c := range map[string]struct {
		bound, use string
		refused    bool
	}{
		"whole":          {wf.PathTemplate("{Stdout}/spec.md"), "bound", true},
		"in prose":       {"the root spec " + wf.PathTemplate("{Stdout}/spec.md") + ", then every nested spec", "bound", true},
		"citation":       {"the source run wrote {Stdout}/spec.md", "bound", false},
		"own path":       {"the ledger at " + wf.PathTemplate("{records}/ledger.md") + " (append only)", "bound", false},
		"shell unmarked": {"{Stdout}/spec.md", "shell-bound", true},
		"shell own path": {"{records}/ledger.md", "shell-bound", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := continueFork(t, continuation{use: c.use, bound: c.bound}, ws)
			var briefed bool
			for _, in := range f.ins {
				briefed = briefed || in.Kind == wf.InstructionAgent && in.State == "ccBrief"
			}
			refused := f.refusedSecond("prompt input SPEC")
			if c.refused && (briefed || !refused) {
				t.Fatalf("briefed %v, refused %v; offered %+v", briefed, refused, f.ins)
			}
			if !c.refused && (!briefed || refused) {
				t.Fatalf("briefed %v, refused %v; offered %+v", briefed, refused, f.ins)
			}
		})
	}
}

// A file the engine opens for a fork's step is checked at its final target:
// a link inside the fork whose target lies in the source is refused, for a
// prompt file and for an artifact a Compute reads (an earlier revision final review, P2).
func TestForkRefusesALinkIntoTheSource(t *testing.T) {
	link := func(name string) func(forkWS, sourceWS string) {
		return func(forkWS, sourceWS string) {
			target := filepath.Join(sourceWS, name)
			if err := os.WriteFile(target, []byte("SOURCE CONTENT"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(forkWS, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	plain := func(string, string) string { return "plain" }
	for name, c := range map[string]struct {
		k     continuation
		field string
	}{
		"prompt file": {continuation{prompt: "{workspace}/brief.md", prepare: link("brief.md")}, "prompt file"},
		"artifact":    {continuation{artifact: "{workspace}/notes.md", use: "read", prepare: link("notes.md")}, "artifact notes"},
	} {
		t.Run(name, func(t *testing.T) {
			f := continueFork(t, c.k, plain)
			for _, in := range f.ins {
				if strings.Contains(in.Prompt, "SOURCE CONTENT") {
					t.Fatalf("the fork briefed through the link: %+v", in)
				}
			}
			for _, r := range f.recs {
				if r.Kind == wf.JournalAccepted && strings.Contains(string(r.Data), "SOURCE CONTENT") {
					t.Fatalf("the fork read through the link: %s", r.Data)
				}
			}
			if !f.refusedSecond(c.field) {
				t.Fatalf("no refusal naming %s; offered %+v", c.field, f.ins)
			}
		})
	}
}
