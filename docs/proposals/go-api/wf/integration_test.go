package wf_test

// Flow 1 of the integration sweep: a definition that uses dispatch hints and
// PromptFor runs through the workflow CLI as separate processes, per
// COORDINATOR.md: run, next (hints and resolved prompt per item), submit with
// the exact line the instruction carries, status at the terminal.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	itLane  struct{ Name, Lane string }
	itRes   struct{ Done bool }
	itPick  struct{}
	itDone  struct{}
	itDead  struct{}
	itFan   struct{}
	itAll   struct{}
	itSome  struct{}
	itReply struct{ Done bool }
)

func itChild() wf.Child[itLane, itRes] {
	d := wf.New("it-lane")
	pick := wf.NewAgent[itPick, itReply](d)
	done := wf.NewTerminal[itDone](d, wf.Success)
	dead := wf.NewTerminal[itDead](d, wf.Stopped)
	d.OnFailure(wf.Blocked, dead)
	pick.Spec(wf.PromptFor(func(l itLane) string { return "prompts/" + l.Lane + ".md" },
		[]string{"prompts/a.md", "prompts/b.md"}, wf.ChildInput()),
		wf.Retry[itPick](wf.Const(1), pick.To(dead, "lane dead")))
	pick.Dispatch(wf.ModelTier(wf.TierStandard), wf.Effort("low"), wf.AgentType("reviewer"))
	ok := pick.To(done, "answered")
	pick.Route(func(itReply, wf.Ctx) wf.Edge[itPick] { return ok }, ok)
	res := func(wf.Ctx) itRes { return itRes{Done: true} }
	wf.Yield(done, res)
	wf.Yield(dead, res)
	return wf.Typed[itLane, itRes](d)
}

// itDef fans two lanes out; each lane's agent has the same hints and its own prompt file.
func itDef() *wf.Def {
	d := wf.New("hinted-cli")
	fan := wf.NewMap[itFan, itLane, string, itRes](d,
		wf.FromValues(itLane{Name: "one", Lane: "a"}, itLane{Name: "two", Lane: "b"}),
		func(l itLane) string { return l.Name }, itChild())
	all := wf.NewTerminal[itAll](d, wf.Success)
	some := wf.NewTerminal[itSome](d, wf.Stopped)
	fan.Parallel(2).Join(wf.AllSettled(), fan.To(all, "all settled"), fan.To(some, "a lane died"))
	return d
}

func TestHintsAndPromptForThroughBuiltCLI(t *testing.T) {
	e := newCLIEnv(t)
	for name, text := range map[string]string{"a": "Lane A prompt.", "b": "Lane B prompt."} {
		if err := os.WriteFile(filepath.Join(e.ws, "prompts", name+".md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	code, out, er := e.proc(dir, nil, "run", "hinted-cli", "--state-dir", e.state, "--workspace", e.ws, "--run-id", "hinted-cli-1")
	if code != 0 {
		t.Fatalf("run exited %d: %s", code, er)
	}
	run := wf.RunID(strings.TrimSpace(out))

	proc := func(a ...string) (int, string, string) { return e.proc(dir, nil, a...) }
	ins := e.nextBy(proc, run)
	if len(ins) != 2 {
		t.Fatalf("next listed %d instructions, want one per lane: %+v", len(ins), ins)
	}
	want := wf.DispatchHints{Model: "sonnet", Effort: "low", AgentType: "reviewer", Tier: wf.TierStandard}
	byPrompt := map[string]string{}
	for _, in := range ins {
		if in.Dispatch != want {
			t.Errorf("%s: dispatch = %+v, want %+v", in.State, in.Dispatch, want)
		}
		code, prompt, er := proc("prompt", string(run), "--claim", claimToken(t, in), "--state-dir", e.state)
		if code != 0 {
			t.Fatalf("prompt exited %d: %s", code, er)
		}
		prompt = withBody(t, prompt)
		for _, p := range []string{"Lane A prompt.", "Lane B prompt."} {
			if strings.Contains(prompt, p) {
				byPrompt[p] = prompt
			}
		}
	}
	if len(byPrompt) != 2 {
		t.Fatalf("want one instruction per lane prompt file, got %v from %+v", len(byPrompt), ins)
	}
	if a, b := byPrompt["Lane A prompt."], byPrompt["Lane B prompt."]; !strings.Contains(a, `"one"`) || !strings.Contains(b, `"two"`) {
		t.Errorf("each prompt should carry its own item:\nA: %s\nB: %s", a, b)
	}
	if st := e.status(run); st.Phase == wf.RunEnded {
		t.Fatalf("status before any done = %+v", st)
	}

	for _, in := range ins {
		if code, _, er := proc(e.doneArgs(in, `{"Done":true}`)...); code != 0 {
			t.Fatalf("done exited %d: %s", code, er)
		}
	}
	code, out, er = e.proc(dir, nil, "status", string(run), "--state-dir", e.state)
	if code != 0 {
		t.Fatalf("status exited %d: %s", code, er)
	}
	st := e.status(run)
	if st.Phase != wf.RunEnded || st.Terminal != "itAll" || st.Outcome != wf.Success {
		t.Fatalf("status = %+v (%s), want ended at itAll with success", st, out)
	}
}
