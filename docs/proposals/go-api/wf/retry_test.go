package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	RtA    struct{}
	RtEnd  struct{}
	RtStop struct{}
	RtWait struct{}
)

// classDef is one agent whose unavailable class takes a pause route; mutate
// builds the class edge.
func classDef(name string, mutate func(r wf.RetryPolicy[RtA], pause wf.Edge[RtA]) wf.RetryPolicy[RtA]) *wf.Def {
	d := wf.New(name).Version(1)
	a := wf.NewAgent[RtA, string](d)
	end := wf.NewTerminal[RtEnd](d, wf.Success)
	stop := wf.NewTerminal[RtStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	wait := wf.NewTerminal[RtWait](d, wf.Stopped)
	ok := a.To(end, "ok")
	r := mutate(wf.Retry(wf.Const(3), a.To(stop, "exhausted")), a.To(wait, "pause"))
	a.Spec(wf.PromptText("go"), r, wf.Checkf("c", func(string, wf.Ctx) error { return nil }))
	a.Route(func(string, wf.Ctx) wf.Edge[RtA] { return ok }, ok)
	return d
}

func backoffDef(delays ...time.Duration) *wf.Def {
	return classDef("backoff", func(r wf.RetryPolicy[RtA], pause wf.Edge[RtA]) wf.RetryPolicy[RtA] {
		return r.OnClassBackoff(wf.Unavailable, pause, delays...)
	})
}

func TestOnClassBackoffRenders(t *testing.T) {
	d := backoffDef(time.Second, 2*time.Second)
	var rv *wf.RetryView
	for _, s := range wf.Inspect(d).States {
		if s.Name == "RtA" {
			rv = s.Retry
		}
	}
	if rv == nil {
		t.Fatal("no retry view on RtA")
	}
	got := rv.Backoff["unavailable"]
	if len(got) != 2 || got[0] != time.Second || got[1] != 2*time.Second {
		t.Fatalf("Backoff[unavailable] = %v, want [1s 2s]", got)
	}
	if rv.ByClass["unavailable"] != "pause" {
		t.Fatalf("ByClass[unavailable] = %q, want pause", rv.ByClass["unavailable"])
	}
	if out := d.Render(); !strings.Contains(out, "pause [on unavailable after retries 1s, 2s]") {
		t.Fatalf("render lacks the schedule annotation:\n%s", out)
	}
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
}

func TestOnClassBackoffHashed(t *testing.T) {
	a, b := backoffDef(time.Second, 2*time.Second).Hash(), backoffDef(time.Second, 3*time.Second).Hash()
	if a == b {
		t.Fatalf("different schedules hash alike: %s", a)
	}
	if a != backoffDef(time.Second, 2*time.Second).Hash() {
		t.Fatal("hash not stable for equal schedules")
	}
}

func TestOnClassStaysImmediate(t *testing.T) {
	d := classDef("plain", func(r wf.RetryPolicy[RtA], pause wf.Edge[RtA]) wf.RetryPolicy[RtA] {
		return r.OnClass(wf.Unavailable, pause)
	})
	for _, s := range wf.Inspect(d).States {
		if s.Name == "RtA" {
			if len(s.Retry.Backoff) != 0 {
				t.Fatalf("plain OnClass has Backoff %v", s.Retry.Backoff)
			}
		}
	}
	out := d.Render()
	if !strings.Contains(out, "pause [on unavailable]") || strings.Contains(out, "after retries") {
		t.Fatalf("plain OnClass render:\n%s", out)
	}
}

// Marker types for the retry tests.
type (
	RrWork  struct{}
	RrGate  struct{}
	RrEnd   struct{}
	RrFan   struct{}
	RrMerge struct{}
	RrPart  struct{}
	RrItem  struct{}
	RrOK    struct{}
	RrBad   struct{}
	RrAfter struct{}
	RrSub   struct{}
)

// retryCompute is one compute with no retry policy that fails with err until
// *fail is cleared.
func retryCompute(fail *bool) *wf.Def {
	d := wf.New("retry-compute").Version(1)
	c := wf.NewCompute[RrWork, int](d, func(wf.Ctx) (int, error) {
		if *fail {
			return 0, errors.New("compute broke")
		}
		return 1, nil
	})
	end := wf.NewTerminal[RrEnd](d, wf.Success)
	done := c.To(end, "done")
	c.Route(func(int, wf.Ctx) wf.Edge[RrWork] { return done }, done)
	return d
}

func TestRetryHaltedCompute(t *testing.T) {
	fail := true
	d := retryCompute(&fail)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.next(run)
	if st := h.status(run); st.Phase != wf.RunBlocked || st.Instances[0].Attempt != 1 {
		t.Fatalf("status = %+v, want blocked at attempt 1", st)
	}
	fail = false
	if err := h.e.Retry(run, "", false); err != nil {
		t.Fatal(err)
	}
	retried := h.kind(run, wf.JournalRetried)
	if len(retried) != 1 || !strings.Contains(string(retried[0].Data), "compute broke") {
		t.Fatalf("retried records = %+v, want one naming the halt reason", retried)
	}
	statusOf(t, h.status(run), wf.RunEnded, "RrEnd")
	// The compute ran again at attempt 2, no attempt spent.
	var attempts []int
	for _, r := range h.kind(run, wf.JournalAccepted) {
		attempts = append(attempts, r.Attempt)
	}
	if len(attempts) != 1 || attempts[0] != 2 {
		t.Fatalf("accepted at attempts %v, want [2]", attempts)
	}
}

func TestRetryHaltedEffect(t *testing.T) {
	d, _ := commandDef(nil)
	w := newWorld()
	w.applyErr = errors.New("exec: permission denied")
	h := newHarness(t, d, w)
	run := h.run(d, map[string]string{"topic": "x"})
	h.next(run)
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %+v, want blocked", st)
	}
	w.applyErr = nil
	w.runs = []wf.RunResult{{Exit: 0}}
	if err := h.e.Retry(run, "", false); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "cOK")
	if w.appliedCount() != 2 {
		t.Fatalf("applied %d times, want the failed attempt and the retry", w.appliedCount())
	}
}

// retryMapDef is a Map of two children, each a compute then an agent. The
// compute of item 1 fails (and so halts, it has no retry policy) while *fail is
// set; item 2 passes it and waits at the agent, so the join stays open.
func retryMapDef(fail *bool) *wf.Def {
	return retryMapPolicyDef(fail, wf.ContinueSiblings)
}

// retryMapPolicyDef is retryMapDef with the Map's sibling policy given: the
// zero policy leaves OnSiblingFailure unset.
func retryMapPolicyDef(fail *bool, policy wf.SiblingPolicy) *wf.Def {
	child := retryItemChild(fail)
	d := wf.New("retry-map")
	m := wf.NewMap[RrFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, child)
	merged := wf.NewTerminal[RrMerge](d, wf.Success)
	partial := wf.NewTerminal[RrPart](d, wf.Stopped)
	m.Parallel(2)
	if policy != 0 {
		m.OnSiblingFailure(policy)
	}
	m.Join(wf.AllOK(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// retryItemChild is one item of retryMapDef: a compute then an agent.
func retryItemChild(fail *bool) wf.Child[chItem, chRes] {
	cd := wf.New("rr-item")
	g := wf.NewCompute[RrGate, int](cd, func(c wf.Ctx) (int, error) {
		if *fail && wf.InputOf[chItem](c).N == 1 {
			return 0, errors.New("item 1 broke")
		}
		return 1, nil
	})
	w := wf.NewAgent[RrWork, chOut](cd)
	ok := wf.NewTerminal[RrOK](cd, wf.Success)
	bad := wf.NewTerminal[RrBad](cd, wf.Stopped)
	cd.OnFailure(wf.Blocked, bad)
	toWork := g.To(w, "gate passed")
	g.Route(func(int, wf.Ctx) wf.Edge[RrGate] { return toWork }, toWork)
	w.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[RrWork](wf.Const(1), w.To(bad, "gave up")))
	toOK := w.To(ok, "ok")
	w.Route(func(chOut, wf.Ctx) wf.Edge[RrWork] { return toOK }, toOK)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	return wf.Typed[chItem, chRes](cd)
}

func TestRetryHaltedMapChild(t *testing.T) {
	fail := true
	d := retryMapDef(&fail)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Child != "RrFan[2]" {
		t.Fatalf("instructions = %+v, want only item 2 at its agent", ins)
	}
	other := ins[0]
	const halted = "RrFan[1]"
	if got := kids(h.status(run))[halted]; got.Phase != "done" || !strings.Contains(got.Reason, "item 1 broke") {
		t.Fatalf("item 1 = %+v, want settled after its halt", got)
	}

	// The Map settled the child but its join is still open: Retry revives it.
	fail = false
	if err := h.e.Retry(run, halted, false); err != nil {
		t.Fatal(err)
	}
	if r := h.kind(run, wf.JournalRetried); len(r) != 1 || r[0].Instance != halted {
		t.Fatalf("retried records = %+v, want one for %s", r, halted)
	}
	var again *wf.Instruction
	for _, in := range h.next(run) {
		if in.Child == halted {
			again = &in
		}
	}
	if again == nil {
		t.Fatal("the retried child did not run again to its agent")
	}
	h.submit(run, *again, `{"OK":true}`)
	h.submit(run, other, `{"OK":true}`)
	statusOf(t, h.status(run), wf.RunEnded, "RrMerge")

	// Once the join is decided, a halted child can no longer be revived; here
	// the run has ended too, which Retry rejects before anything else.
	fail = true
	d = retryMapDef(&fail)
	h = newHarness(t, d, nil)
	run = h.run(d, nil)
	other = h.next(run)[0]
	h.submit(run, other, `{"OK":true}`)
	statusOf(t, h.status(run), wf.RunEnded, "RrPart")
	if err := h.e.Retry(run, halted, false); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "already ended") {
		t.Fatalf("Retry after the join = %v, want ErrRejected naming the ended run", err)
	}
}

// retryLiveJoinDef is retryMapDef with the given join whose met and partial
// edges both go to a further agent, so the run stays live once the join is
// decided.
func retryLiveJoinDef(fail *bool, join wf.JoinPolicy) *wf.Def {
	d := wf.New("retry-live-join")
	m := wf.NewMap[RrFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, retryItemChild(fail))
	after := wf.NewAgent[RrAfter, chOut](d)
	merged := wf.NewTerminal[RrMerge](d, wf.Success)
	partial := wf.NewTerminal[RrPart](d, wf.Stopped)
	m.Parallel(2)
	m.Join(join, m.To(after, "met"), m.To(after, "partial"))
	after.Spec(wf.PromptText("after"), wf.Retry[RrAfter](wf.Const(1), after.To(partial, "gave up")))
	done := after.To(merged, "done")
	after.Route(func(chOut, wf.Ctx) wf.Edge[RrAfter] { return done }, done)
	return d
}

// finishAfter checks the run is live with only the root at RrAfter, then
// answers it and checks the run ends at RrMerge.
func finishAfter(t *testing.T, h *harness, run wf.RunID) {
	t.Helper()
	next := h.next(run)
	if len(next) != 1 || next[0].State != "RrAfter" || next[0].Child != "" {
		t.Fatalf("next = %+v, want only the root at RrAfter", next)
	}
	h.submit(run, next[0], `{"OK":true}`)
	statusOf(t, h.status(run), wf.RunEnded, "RrMerge")
}

// A halted child the Map settled stays halted(); once the join is decided and
// the run goes on, mapUndecided is what refuses to revive it: Retry, retry or
// stop, is rejected because its spawner already decided.
func TestRetryHaltedChildAfterJoinDecidedLive(t *testing.T) {
	fail := true
	d := retryLiveJoinDef(&fail, wf.AllOK())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Child != "RrFan[2]" {
		t.Fatalf("instructions = %+v, want only item 2 at its agent", ins)
	}
	const halted = "RrFan[1]"
	fail = false
	h.submit(run, ins[0], `{"OK":true}`)
	st := h.status(run)
	if st.Phase == wf.RunEnded || st.Phase == wf.RunBlocked || len(st.Fix) != 0 {
		t.Fatalf("status = %s, Fix %q, want the run live at RrAfter with no fix: lines", st.Phase, st.Fix)
	}
	for _, stop := range []bool{false, true} {
		err := h.e.Retry(run, halted, stop)
		if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "already decided") {
			t.Fatalf("Retry(stop=%v) after the join = %v, want ErrRejected naming the decided spawner", stop, err)
		}
	}
	if n := len(h.kind(run, wf.JournalRetried)); n != 0 {
		t.Fatalf("%d retried records journaled by rejected retries", n)
	}
	finishAfter(t, h, run)
}

// A halted child its spawner has not decided yet is a state a crash leaves (the
// process died between the child's halt and the Map's settling it): the run is
// held at that child until the next drive, and Retry revives it.
func TestRetryChildHeldBeforeSpawnerDecides(t *testing.T) {
	fail := true
	d := retryMapPolicyDef(&fail, 0)
	c := newCrashHarness(t, d, newWorld(), wf.JournalChildSettled)
	run := c.run(d, nil)
	c.cj.armed = true
	if _, err := c.e.Next(run, ""); !errors.Is(err, errCrash) {
		t.Fatalf("Next error = %v, want the crash before the Map settles the halted child", err)
	}
	c.resume()
	st := c.status(run)
	if st.Phase != wf.RunBlocked || !strings.HasPrefix(st.NothingReadyReason, "blocked: rr-item/RrGate ") {
		t.Fatalf("status = %s / %q, want blocked at the halted child", st.Phase, st.NothingReadyReason)
	}
	fail = false
	if err := c.e.Retry(run, "RrFan[1]", false); err != nil {
		t.Fatalf("Retry of the child the run is held at: %v", err)
	}
	if st := c.status(run); st.Phase == wf.RunBlocked {
		t.Fatalf("status = %s after Retry, want the run going on", st.Phase)
	}
}

func TestRetryNotHaltedRejected(t *testing.T) {
	d, _ := simple("rr-running", wf.PromptText("go"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.next(run)
	if err := h.e.Retry(run, "", false); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "not halted") {
		t.Fatalf("Retry of a running instance = %v, want ErrRejected", err)
	}
	if err := h.e.Retry(run, "Nowhere", false); err == nil {
		t.Fatal("Retry of an unknown child succeeded")
	}
	if n := len(h.kind(run, wf.JournalRetried)); n != 0 {
		t.Fatalf("%d retried records journaled by rejected retries", n)
	}
}

func TestDefaultUnavailableBackoffBounded(t *testing.T) {
	d := wf.New("rr-down").Version(1)
	obs := wf.Observe[int]("bd", "show", map[string]any{"id": "x"})
	c := wf.NewCompute[RrWork, int](d, func(x wf.Ctx) (int, error) { return wf.Observed(x, obs) })
	end := wf.NewTerminal[RrEnd](d, wf.Success)
	done := c.To(end, "done")
	c.Observes(obs)
	c.Route(func(int, wf.Ctx) wf.Edge[RrWork] { return done }, done)
	w := newWorld()
	w.down["bd.show"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i := 0; i < 12; i++ {
		h.next(run)
		h.now = h.now.Add(9 * time.Minute)
	}
	var got []time.Duration
	for _, r := range h.kind(run, wf.JournalBackoffScheduled) {
		var b wf.BackoffScheduledData
		if err := json.Unmarshal(r.Data, &b); err != nil {
			t.Fatal(err)
		}
		got = append(got, b.Delay)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backoff delays = %v, want %v then a halt", got, want)
	}
	st := h.status(run)
	if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "after 5 retries") {
		t.Fatalf("status = %+v, want halted naming the five retries", st)
	}

	// An operator retry starts the schedule again.
	if err := h.e.Retry(run, "", false); err != nil {
		t.Fatal(err)
	}
	if n := len(h.kind(run, wf.JournalBackoffScheduled)); n != 6 {
		t.Fatalf("backoff records after retry = %d, want a fresh first delay (6)", n)
	}
}

func TestRetryCLI(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	if code, _, er := e.cli("retry", string(run), "--state-dir", e.state); code != 4 || !strings.Contains(er, "not halted") {
		t.Fatalf("retry of a running run = %d %q, want 4", code, er)
	}
	if code, _, _ := e.cli("retry", "nope", "--state-dir", e.state); code != 3 {
		t.Fatalf("retry of an unknown run = %d, want 3", code)
	}
}

// wf retry --child on a halted child its Map settled, while the join is open,
// exits 0 and records the retry.
func TestRetrySettledChildCLI(t *testing.T) {
	fail := true
	cfg := wf.MainConfig{Adapters: wf.NewAdapters(), Defs: []wf.Registration{{Def: retryMapDef(&fail)}}}
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	cli := func(args ...string) (int, string, string) {
		var o, er bytes.Buffer
		code := wf.CLI(cfg, args, &o, &er)
		return code, o.String(), er.String()
	}
	if code, _, er := cli("run", "retry-map", "--state-dir", state, "--workspace", ws, "--run-id", "rr-cli"); code != 0 {
		t.Fatalf("run exited %d: %s", code, er)
	}
	if code, _, er := cli("next", "rr-cli", "all", "--state-dir", state, "--json"); code != 0 {
		t.Fatalf("next exited %d: %s", code, er)
	}
	fail = false
	if code, _, er := cli("retry", "rr-cli", "--state-dir", state, "--child", "RrFan[1]"); code != 0 {
		t.Fatalf("retry of a settled child exited %d: %s", code, er)
	}
}
