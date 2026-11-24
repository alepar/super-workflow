package wf_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// States of the workdir definitions.
type (
	wdBind  struct{}
	wdAgent struct{}
	wdPlain struct{}
	wdBase  struct{}
	wdOwn   struct{}
	wdDone  struct{}
	wdBad   struct{}
)

type wdBound struct {
	Workdir string `json:"workdir"`
}

type wdOut struct{}

// wdDef runs an agent, a plain command, a command with In("."), and a command
// with In("{sub}"), in that order. With bind it starts at a compute that binds
// {workdir} to ".worktrees/run"; without it the definition binds nothing.
func wdDef(name string, bind bool) *wf.Def {
	d := wf.New(name)
	var b *wf.Compute[wdBind, wdBound]
	if bind {
		b = wf.NewCompute[wdBind, wdBound](d, func(wf.Ctx) (wdBound, error) {
			return wdBound{Workdir: ".worktrees/run"}, nil
		})
	}
	a := wf.NewAgent[wdAgent, wdOut](d)
	plain := wf.NewCommand[wdPlain](d, "echo", "{workspace}")
	base := wf.NewCommand[wdBase](d, "echo", "base").In(".")
	own := wf.NewCommand[wdOwn](d, "echo", "own").In("{sub}")
	done := wf.NewTerminal[wdDone](d, wf.Success)
	bad := wf.NewTerminal[wdBad](d, wf.Stopped)

	a.Spec(wf.PromptText("Work."), wf.Retry[wdAgent](wf.Const(2), a.To(bad, "gave up")))
	toPlain := a.To(plain, "agent done")
	a.Route(func(wdOut, wf.Ctx) wf.Edge[wdAgent] { return toPlain }, toPlain)
	toBase := plain.To(base, "plain done")
	plain.Route(func(wf.RunResult, wf.Ctx) wf.Edge[wdPlain] { return toBase }, toBase)
	toOwn := base.To(own, "base done")
	base.Route(func(wf.RunResult, wf.Ctx) wf.Edge[wdBase] { return toOwn }, toOwn)
	toDone := own.To(done, "own done")
	own.Route(func(wf.RunResult, wf.Ctx) wf.Edge[wdOwn] { return toDone }, toDone)
	if bind {
		toA := b.To(a, "bound")
		b.Route(func(wdBound, wf.Ctx) wf.Edge[wdBind] { return toA }, toA)
	}
	return d
}

// shellDirs are the dir args of the shell effects the run applied, by echoed word.
func shellDirs(t *testing.T, w *world) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, e := range w.applied {
		if e.Adapter != "shell" {
			continue
		}
		argv, _ := e.Args["argv"].([]any)
		dir, _ := e.Args["dir"].(string)
		got[argv[1].(string)] = dir
	}
	return got
}

// intentDirs reads the dir of each journaled shell intent, in order.
func intentDirs(t *testing.T, h *harness, run wf.RunID) []string {
	t.Helper()
	var dirs []string
	for _, r := range h.kind(run, wf.JournalEffectIntent) {
		var d wf.EffectIntentData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		dir, _ := d.Effect.Args["dir"].(string)
		dirs = append(dirs, dir)
	}
	return dirs
}

func TestWorkdirSteersAgentsAndCommands(t *testing.T) {
	ws := t.TempDir()
	w := newWorld()
	d := wdDef("wd-steer", true)
	h := newHarnessOpts(t, d, w, wf.EngineOptions{Workspace: ws})
	run := h.run(d, map[string]string{"sub": "elsewhere"})
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
		t.Fatalf("instructions = %+v", ins)
	}
	want := filepath.Join(ws, ".worktrees/run")
	if ins[0].Workspace != want {
		t.Errorf("instruction Workspace = %q, want %q", ins[0].Workspace, want)
	}
	if !strings.Contains(ins[0].Prompt, "Work in "+want+".") {
		t.Errorf("Submission block does not name %q:\n%s", want, ins[0].Prompt)
	}
	h.submit(run, ins[0], `{}`)
	if st := h.status(run); st.Phase != wf.RunEnded {
		t.Fatalf("status = %+v", st)
	}
	// The plain command inherits the bound {workdir}; the journal records it.
	dirs := intentDirs(t, h, run)
	if len(dirs) != 3 || dirs[0] != ".worktrees/run" {
		t.Errorf("journaled dirs = %q, want the bound .worktrees/run first", dirs)
	}
	if got := shellDirs(t, w)[ws]; got != ".worktrees/run" {
		t.Errorf("plain command dir = %q (argv echoes {workspace}=%s; applied %v)", got, ws, shellDirs(t, w))
	}
}

func TestCommandInOverridesWorkdir(t *testing.T) {
	ws := t.TempDir()
	w := newWorld()
	d := wdDef("wd-in", true)
	h := newHarnessOpts(t, d, w, wf.EngineOptions{Workspace: ws})
	run := h.run(d, map[string]string{"sub": "elsewhere"})
	h.submit(run, h.next(run)[0], `{}`)
	got := shellDirs(t, w)
	if got["base"] != "." {
		t.Errorf("In(\".\") ran in %q, want the base checkout \".\"", got["base"])
	}
	if got["own"] != "elsewhere" {
		t.Errorf("In(\"{sub}\") ran in %q, want the bound sub param", got["own"])
	}
}

func TestUnboundWorkdirIsRoot(t *testing.T) {
	ws := t.TempDir()
	w := newWorld()
	d := wdDef("wd-unbound", false)
	h := newHarnessOpts(t, d, w, wf.EngineOptions{Workspace: ws})
	run := h.run(d, map[string]string{"sub": "elsewhere"})
	ins := h.next(run)
	if ins[0].Workspace != ws || !strings.Contains(ins[0].Prompt, "Work in "+ws+".") {
		t.Errorf("an unbound definition must keep the workspace root %q: %+v", ws, ins[0])
	}
	h.submit(run, ins[0], `{}`)
	if dirs := intentDirs(t, h, run); len(dirs) != 3 || dirs[0] != "" {
		t.Errorf("journaled dirs = %q, want the empty root dir for an unbound plain command", dirs)
	}
}

func TestWorkspaceBuiltin(t *testing.T) {
	ws := t.TempDir()
	w := newWorld()
	d := wdDef("wd-builtin", false)
	h := newHarnessOpts(t, d, w, wf.EngineOptions{Workspace: ws})
	run := h.run(d, map[string]string{"sub": "elsewhere"})
	h.submit(run, h.next(run)[0], `{}`)
	var argv []any
	for _, e := range w.applied {
		if e.Adapter == "shell" {
			argv, _ = e.Args["argv"].([]any)
			break
		}
	}
	if len(argv) != 2 || argv[1] != ws {
		t.Errorf("{workspace} bound to %v, want the absolute workspace root %s", argv, ws)
	}
}

func TestWorkdirViewAndRender(t *testing.T) {
	d := wdDef("wd-view", false)
	text := d.Render()
	if !strings.Contains(text, "command echo base (in .)") || !strings.Contains(text, "command echo own (in {sub})") {
		t.Errorf("render does not show the command directories:\n%s", text)
	}
	if strings.Contains(text, "command echo {workspace} (in") {
		t.Errorf("a command without In must render without a directory:\n%s", text)
	}
}
