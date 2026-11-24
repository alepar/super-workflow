package wf_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	yc1   struct{}
	yc2   struct{}
	yc3   struct{}
	yc4   struct{}
	yc5   struct{}
	ycOut struct{}
)

// countingWorld is slowWorld that counts each shell argv it applies, so a
// test sees every effect ran exactly once whatever the interleaving.
type countingWorld struct {
	slowWorld
	mu    sync.Mutex
	count map[string]int
}

func (w *countingWorld) Apply(op string, x wf.ExternalEffect) (json.RawMessage, error) {
	if argv, _ := x.Args["argv"].([]any); len(argv) > 0 {
		w.mu.Lock()
		w.count[fmt.Sprint(x.Key)+" "+fmt.Sprint(argv...)]++
		w.mu.Unlock()
	}
	return w.slowWorld.Apply(op, x)
}

func (w *countingWorld) counts() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]int{}
	for k, v := range w.count {
		out[k] = v
	}
	return out
}

// chainDef fans out two agent units. A unit's accepted result runs a chain
// of five computed steps, each edge an effect "sleep <step>" (named per unit
// by its item), so one done drives five steps under the run lock. pair adds
// a second effect, "echo <item> pair", to the first step's edge.
func chainDef(name, step string, pair bool) *wf.Def {
	child := wf.New(name + "-item")
	w := wf.NewAgent[chWork, chOut](child)
	ok := wf.NewTerminal[chOK](child, wf.Success)
	bad := wf.NewTerminal[chBad](child, wf.Stopped)
	child.OnFailure(wf.Blocked, bad)
	w.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[chWork](wf.Const(1), w.To(bad, "gave up")))
	c1 := wf.NewCompute[yc1, ycOut](child, func(wf.Ctx) (ycOut, error) { return ycOut{}, nil })
	c2 := wf.NewCompute[yc2, ycOut](child, func(wf.Ctx) (ycOut, error) { return ycOut{}, nil })
	c3 := wf.NewCompute[yc3, ycOut](child, func(wf.Ctx) (ycOut, error) { return ycOut{}, nil })
	c4 := wf.NewCompute[yc4, ycOut](child, func(wf.Ctx) (ycOut, error) { return ycOut{}, nil })
	c5 := wf.NewCompute[yc5, ycOut](child, func(wf.Ctx) (ycOut, error) { return ycOut{}, nil })
	sleep := func(i int) wf.Effect {
		return shell.Effect(wf.KeyOf("step", fmt.Sprint(i)), shell.Run("sleep", step, fmt.Sprint(i)))
	}
	toC1 := w.To(c1, "ok")
	toBad := w.To(bad, "bad")
	w.Route(func(o chOut, _ wf.Ctx) wf.Edge[chWork] {
		if o.OK {
			return toC1
		}
		return toBad
	}, toC1, toBad)
	e1 := c1.To(c2, "step 1").With(sleep(1))
	if pair {
		e1 = c1.To(c2, "step 1").With(sleep(1), shell.Effect(wf.KeyOf("pair"), shell.Run("echo", "pair")))
	}
	c1.Route(func(ycOut, wf.Ctx) wf.Edge[yc1] { return e1 }, e1)
	e2 := c2.To(c3, "step 2").With(sleep(2))
	c2.Route(func(ycOut, wf.Ctx) wf.Edge[yc2] { return e2 }, e2)
	e3 := c3.To(c4, "step 3").With(sleep(3))
	c3.Route(func(ycOut, wf.Ctx) wf.Edge[yc3] { return e3 }, e3)
	e4 := c4.To(c5, "step 4").With(sleep(4))
	c4.Route(func(ycOut, wf.Ctx) wf.Edge[yc4] { return e4 }, e4)
	e5 := c5.To(ok, "step 5").With(sleep(5))
	c5.Route(func(ycOut, wf.Ctx) wf.Edge[yc5] { return e5 }, e5)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)

	d := wf.New(name)
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](child))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// chainRun is a started chainDef run with both units handed out, started,
// fetched and their results written: each is one done away from its chain.
type chainRun struct {
	t     *testing.T
	e     *cliEnv
	cfg   wf.MainConfig
	world *countingWorld
	run   string
	units []wf.Instruction
}

func newChainRun(t *testing.T, step string, pair bool) *chainRun {
	t.Helper()
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "BASH_DEFAULT_TIMEOUT_MS", "HERMES_SESSION_ID", "TERMINAL_TIMEOUT", "CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		t.Setenv(k, "")
	}
	d := chainDef("yield-chain", step, pair)
	world := &countingWorld{count: map[string]int{}}
	reg := wf.NewAdapters()
	if err := reg.Register(world); err != nil {
		t.Fatal(err)
	}
	r := &chainRun{t: t, e: newCLIEnv(t), cfg: wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: d}}}, world: world}
	r.run = strings.TrimSpace(r.must("run", d.Name(), "--run-id", "yield", "--state-dir", r.e.state, "--workspace", r.e.ws))
	if err := json.Unmarshal([]byte(r.must("next", r.run, "2", "--state-dir", r.e.state, "--json")), &r.units); err != nil || len(r.units) != 2 {
		t.Fatalf("next: %v: %+v", err, r.units)
	}
	for _, in := range r.units {
		r.must("dev", "started", r.run, in.Name, "--session", "s", "--agent", in.Name, "--state-dir", r.e.state)
	}
	return r
}

func (r *chainRun) call(args ...string) (int, string, string) {
	var out, er bytes.Buffer
	code := fixtureCLI(r.cfg, args, &out, &er)
	return code, out.String(), er.String()
}

func (r *chainRun) must(args ...string) string {
	r.t.Helper()
	code, out, er := r.call(args...)
	if code != 0 {
		r.t.Fatalf("%v exited %d: %s", args, code, er)
	}
	return out
}

// ready fetches unit i's prompt and writes its result.
func (r *chainRun) ready(i int) {
	r.t.Helper()
	withBody(r.t, r.must("prompt", r.run, r.units[i].Name, "--state-dir", r.e.state))
	writeAllocated(r.t, r.units[i].Output, `{"OK":true}`)
}

// doneAsync runs unit i's done in the background; the returned func waits
// for it and returns its exit code and stderr.
func (r *chainRun) doneAsync(i int) func() (int, string) {
	var code int
	var er string
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		code, _, er = r.call("done", r.run, r.units[i].Name, "--state-dir", r.e.state)
	}()
	return func() (int, string) { <-ch; return code, er }
}

// heldBy waits until the run's lock record names verb.
func (r *chainRun) heldBy(verb string) {
	r.t.Helper()
	lock := r.e.state + "/runs/" + r.run + "/lock"
	for start := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(lock); bytes.Contains(b, []byte(`"verb":"`+verb+`"`)) {
			return
		}
		if time.Since(start) > 10*time.Second {
			r.t.Fatalf("%s never took the run lock", verb)
		}
	}
}

// stepRuns counts, from the journal, each chain step's applied effect by
// instance and state ("chFan[1] yc3"); sleeps is how many sleeps the
// adapter applied in all.
func (r *chainRun) stepRuns() (steps map[string]int, sleeps int) {
	r.t.Helper()
	steps = map[string]int{}
	for _, rec := range r.journal() {
		if rec.Kind == wf.JournalEffectResult && strings.HasPrefix(rec.State, "yc") && strings.Contains(string(rec.Data), `"Index":0`) {
			steps[rec.Instance+" "+rec.State]++
		}
	}
	for k, n := range r.world.counts() {
		if strings.Contains(k, " sleep") {
			sleeps += n
		}
	}
	return steps, sleeps
}

// unitSteps is how many of unit item's chain steps ran.
func unitSteps(steps map[string]int, item int) int {
	n := 0
	for s := 1; s <= 5; s++ {
		n += steps[fmt.Sprintf("chFan[%d] yc%d", item, s)]
	}
	return n
}

// journal is the run's records as the journal view prints them.
func (r *chainRun) journal() []wf.JournalRecord {
	r.t.Helper()
	var recs []wf.JournalRecord
	for _, line := range strings.Split(strings.TrimSpace(r.must("journal", r.run, "--state-dir", r.e.state)), "\n") {
		var rec wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			r.t.Fatalf("journal line %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// finish drives the run to its end with the coordinator's next and checks
// every chain step of both units ran exactly once.
func (r *chainRun) finish(step string) {
	r.t.Helper()
	for i := 0; i < 20; i++ {
		code, _, er := r.call("next", r.run, "1", "--state-dir", r.e.state, "--json")
		if code == 7 {
			break
		}
		if code != 0 {
			r.t.Fatalf("next exited %d: %s", code, er)
		}
	}
	steps, sleeps := r.stepRuns()
	for item := 1; item <= 2; item++ {
		for s := 1; s <= 5; s++ {
			if k := fmt.Sprintf("chFan[%d] yc%d", item, s); steps[k] != 1 {
				r.t.Errorf("chain step %q ran %d times, want once (all: %v)", k, steps[k], steps)
			}
		}
	}
	if sleeps != 10 {
		r.t.Errorf("the adapter applied %d chain effects, want 10", sleeps)
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(r.must("status", r.run, "--state-dir", r.e.state, "--json")), &st); err != nil {
		r.t.Fatal(err)
	}
	if st.Outcome != wf.Success {
		r.t.Errorf("run outcome %q, want success", st.Outcome)
	}
}

// A done's chain lets the run lock go between its steps when another call
// waits: the other unit's prompt gets in after about one step, not
// after the whole chain, and the chain still runs each step once.
func TestYieldLetsAPromptInMidChain(t *testing.T) {
	r := newChainRun(t, "1", false)
	r.ready(0)
	wait := r.doneAsync(0)
	r.heldBy("done")
	start := time.Now()
	code, _, er := r.call("prompt", r.run, r.units[1].Name, "--state-dir", r.e.state)
	waited := time.Since(start)
	if code != 0 {
		t.Fatalf("prompt: exit %d: %s", code, er)
	}
	if code, er := wait(); code != 0 {
		t.Fatalf("done: exit %d: %s", code, er)
	}
	t.Logf("prompt waited %v behind a 5 s chain", waited)
	if waited > 2500*time.Millisecond {
		t.Errorf("prompt waited %v behind a chain of 1 s steps, want about one step", waited)
	}
	writeAllocated(t, r.units[1].Output, `{"OK":true}`)
	r.must("done", r.run, r.units[1].Name, "--state-dir", r.e.state)
	r.finish("1")
}

// A waiter's records change what the drive does next: a stop that gets in
// mid-chain ends the drive at the next step boundary, and the steps after
// it do not run.
func TestYieldFoldsAWaitersStopBeforeTheNextStep(t *testing.T) {
	r := newChainRun(t, "1", false)
	r.ready(0)
	wait := r.doneAsync(0)
	r.heldBy("done")
	if code, _, er := r.call("stop", r.run, "--reason", "test", "--state-dir", r.e.state); code != 0 {
		t.Fatalf("stop: exit %d: %s", code, er)
	}
	if code, er := wait(); code != 0 {
		t.Fatalf("done: exit %d: %s", code, er)
	}
	steps, sleeps := r.stepRuns()
	if ran := unitSteps(steps, 1); ran >= 5 || sleeps != ran {
		t.Errorf("%d chain steps ran (%d effects applied) after a stop got in mid-chain, want fewer than 5", ran, sleeps)
	}
	recs := r.journal()
	var stopAt, lastStep int64
	for _, rec := range recs {
		if rec.Kind == wf.JournalPaused && stopAt == 0 {
			stopAt = rec.Seq
		}
		if rec.Kind == wf.JournalEffectIntent {
			lastStep = rec.Seq
		}
	}
	if stopAt == 0 || lastStep > stopAt {
		t.Errorf("an effect intent (seq %d) follows the stop (seq %d)", lastStep, stopAt)
	}
}

// Two dones at once, each with a chain: they take turns step by step, and
// every step of both chains runs exactly once.
func TestYieldBetweenTwoChainsRunsEachStepOnce(t *testing.T) {
	r := newChainRun(t, "0.3", false)
	r.ready(0)
	r.ready(1)
	wait0 := r.doneAsync(0)
	wait1 := r.doneAsync(1)
	for i, w := range []func() (int, string){wait0, wait1} {
		if code, er := w(); code != 0 {
			t.Fatalf("done %d: exit %d: %s", i, code, er)
		}
	}
	r.finish("0.3")
}

// A step's effects are one step: a waiter gets in before or after both of
// them, never between them.
func TestYieldNeverSplitsAStepsEffects(t *testing.T) {
	r := newChainRun(t, "1", true)
	r.ready(0)
	wait := r.doneAsync(0)
	r.heldBy("done")
	if code, _, er := r.call("prompt", r.run, r.units[1].Name, "--state-dir", r.e.state); code != 0 {
		t.Fatalf("prompt: exit %d: %s", code, er)
	}
	if code, er := wait(); code != 0 {
		t.Fatalf("done: exit %d: %s", code, er)
	}
	var first, last, fetched int64
	for _, rec := range r.journal() {
		switch {
		case rec.Kind == wf.JournalEffectIntent && rec.Instance == "chFan[1]" && rec.State == "yc1" && first == 0:
			first = rec.Seq
		case rec.Kind == wf.JournalEffectResult && rec.Instance == "chFan[1]" && rec.State == "yc1" && strings.Contains(string(rec.Data), `"Index":1`):
			last = rec.Seq
		case rec.Kind == wf.JournalPromptFetched && rec.Instance == "chFan[2]":
			fetched = rec.Seq
		}
	}
	if first == 0 || last == 0 || fetched == 0 {
		t.Fatalf("journal lacks the step (%d..%d) or the fetch (%d)", first, last, fetched)
	}
	if fetched > first && fetched < last {
		t.Errorf("the other unit's prompt (seq %d) got in between one step's effects (seq %d..%d)", fetched, first, last)
	}
}

// A done that dies right after it took the lock back from a waiter leaves a
// journal the run goes on from: the coordinator's next drives the rest of
// the chain, and each step runs once.
func TestYieldCrashLeavesAJournalTheRunGoesOnFrom(t *testing.T) {
	r := newChainRun(t, "1", false)
	r.ready(0)
	type crash struct{}
	defer wf.SetYieldedHook(func(wf.RunID) { panic(crash{}) })()
	done := make(chan any)
	go func() {
		defer func() { done <- recover() }()
		r.call("done", r.run, r.units[0].Name, "--state-dir", r.e.state)
	}()
	r.heldBy("done")
	if code, _, er := r.call("prompt", r.run, r.units[1].Name, "--state-dir", r.e.state); code != 0 {
		t.Fatalf("prompt: exit %d: %s", code, er)
	}
	if p := <-done; p != (crash{}) {
		t.Fatalf("done did not die at the yield: %v", p)
	}
	wf.SetYieldedHook(nil)
	steps, _ := r.stepRuns()
	if ran := unitSteps(steps, 1); ran == 0 || ran == 5 {
		t.Fatalf("the crash came after %d of 5 steps, want mid-chain", ran)
	}
	writeAllocated(t, r.units[1].Output, `{"OK":true}`)
	r.must("done", r.run, r.units[1].Name, "--state-dir", r.e.state)
	r.finish("1")
}

// lastIntentAfter reports whether chFan[1]'s chain applied an effect after
// the first record of kind k: the seq of that record and of the last such
// intent.
func (r *chainRun) intentAfter(k wf.JournalKind) (at, last int64) {
	r.t.Helper()
	for _, rec := range r.journal() {
		if rec.Kind == k && at == 0 {
			at = rec.Seq
		}
		if rec.Kind == wf.JournalEffectIntent && rec.Instance == "chFan[1]" && strings.HasPrefix(rec.State, "yc") {
			last = rec.Seq
		}
	}
	return at, last
}

// A call that binds the run until another unit while a done's chain has let
// the lock go: the done's drive starts again from the top under that
// boundary, and the first unit's chain takes no further step.
func TestYieldRedrivesUnderAnUntilUnitBoundMeanwhile(t *testing.T) {
	r := newChainRun(t, "1", false)
	r.ready(0)
	wait := r.doneAsync(0)
	r.heldBy("done")
	if code, _, er := r.call("next", r.run, "1", "--until-unit", r.units[1].Name, "--state-dir", r.e.state, "--json"); code != 0 {
		t.Fatalf("next --until-unit: exit %d: %s", code, er)
	}
	code, er := wait()
	t.Logf("done: exit %d %s", code, er)
	at, last := r.intentAfter(wf.JournalUntilUnitBound)
	if at == 0 {
		t.Fatal("the until-unit boundary was never journaled")
	}
	if last > at {
		t.Errorf("the done's drive applied chFan[1]'s step (intent seq %d) after the run was bound to %s (seq %d)", last, r.units[1].Name, at)
	}
}

// Another call adopts a changed definition while a done's chain has let the
// lock go: the done's drive reloads, meets the definition it does not run,
// and applies no step of the old one under the new pin.
func TestYieldRedrivesAfterAnAdoptionMeanwhile(t *testing.T) {
	r := newChainRun(t, "1", false)
	r.ready(0)
	wait := r.doneAsync(0)
	r.heldBy("done")
	changed := chainDef("yield-chain", "0.01", false)
	reg := wf.NewAdapters()
	if err := reg.Register(r.world); err != nil {
		t.Fatal(err)
	}
	adopter := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: changed}}}
	var out, er bytes.Buffer
	if code := fixtureCLI(adopter, []string{"next", r.run, "--adopt", "--state-dir", r.e.state}, &out, &er); code != 0 {
		t.Fatalf("next --adopt: exit %d: %s", code, er.String())
	}
	// The child's result was accepted before the adoption: its done
	// succeeds, and the drive's mismatch is the coordinator's to meet.
	if code, derr := wait(); code != 0 {
		t.Errorf("done after its result was accepted: exit %d: %s", code, derr)
	}
	accepted := false
	for _, rec := range r.journal() {
		if rec.Kind == wf.JournalAccepted && rec.Instance == "chFan[1]" && rec.State == "chWork" {
			accepted = true
		}
	}
	if !accepted {
		t.Error("the child's result was not accepted")
	}
	at, last := r.intentAfter(wf.JournalMigrated)
	if at == 0 {
		t.Fatal("the adoption was never journaled")
	}
	if last > at {
		t.Errorf("the done's drive applied a step of the old definition (intent seq %d) after the adoption (seq %d)", last, at)
	}
	// The coordinator's next, on the old binary, reports the mismatch.
	if code, _, er := r.call("next", r.run, "1", "--state-dir", r.e.state, "--json"); code != 6 || !strings.Contains(er, "definition hash differs") {
		t.Errorf("the coordinator's next: exit %d, want 6 naming the mismatch: %s", code, er)
	}
	// A child whose result is not yet accepted meets the mismatch as before.
	writeAllocated(t, r.units[1].Output, `{"OK":true}`)
	if code, _, er := r.call("done", r.run, r.units[1].Name, "--state-dir", r.e.state); code != 6 || !strings.Contains(er, "definition hash differs") {
		t.Errorf("done before acceptance under a changed definition: exit %d, want 6: %s", code, er)
	}
}
