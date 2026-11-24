package wf_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	sbAsk  struct{}
	sbDone struct{}
	sbStop struct{}
)

// sandboxWorld records, per run, whether each effect it applied was told
// the run is sandboxed.
type sandboxWorld struct {
	clWorld
	mu  sync.Mutex
	got map[wf.RunID][]bool
}

func (w *sandboxWorld) ApplyAt(ws string, at wf.EffectContext, x wf.ExternalEffect) (json.RawMessage, error) {
	w.mu.Lock()
	w.got[at.Run] = append(w.got[at.Run], at.Sandboxed)
	w.mu.Unlock()
	return w.clWorld.Apply(ws, x)
}

// A run made by a rewind or a snapshot restore runs every adapter call
// sandboxed (bd --sandbox), so it never reaches a real remote; its source
// does not, and the fork stays sandboxed on resume (bd config review).
func TestForkedRunsAdaptersAreSandboxed(t *testing.T) {
	d := wf.New("sandbox-fork")
	a := wf.NewAgent[sbAsk, clAnswer](d)
	done := wf.NewTerminal[sbDone](d, wf.Success)
	stop := wf.NewTerminal[sbStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[sbAsk](wf.Const(2), a.To(stop, "gave up")))
	next := a.To(done, "ok").With(wf.ExternalEffect{Adapter: "bd", Op: "close", Args: map[string]any{"id": "x-1", "reason": "done"}, Key: wf.KeyOf("close")})
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[sbAsk] { return next }, next)
	world := &sandboxWorld{got: map[wf.RunID][]bool{}}
	reg := wf.NewAdapters()
	if err := reg.Register(world); err != nil {
		t.Fatal(err)
	}
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: d}}}
	e := newCLIEnv(t)
	must := func(args ...string) string {
		t.Helper()
		var o, er bytes.Buffer
		if code := fixtureCLI(cfg, args, &o, &er); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er.String())
		}
		return o.String()
	}
	finish := func(run string) {
		t.Helper()
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(must("next", run, "1", "--state-dir", e.state, "--json")), &ins); err != nil || len(ins) != 1 {
			t.Fatalf("next %s: %v", run, err)
		}
		must("dev", "started", run, ins[0].Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
		must("done", run, ins[0].Name, "--output", writeAllocated(t, ins[0].Output, `{"OK":true}`), "--state-dir", e.state)
	}
	must("run", d.Name(), "--run-id", "src", "--state-dir", e.state, "--workspace", e.ws)
	finish("src")
	fork := strings.TrimSpace(must("dev", "rewind", "src", "--to", "died", "--state-dir", e.state))
	finish(fork)
	if got := world.got["src"]; len(got) != 1 || got[0] {
		t.Errorf("the source's effects sandboxed: %v, want [false]", got)
	}
	if got := world.got[wf.RunID(fork)]; len(got) != 1 || !got[0] {
		t.Errorf("the fork's effects sandboxed: %v, want [true]", got)
	}
}
