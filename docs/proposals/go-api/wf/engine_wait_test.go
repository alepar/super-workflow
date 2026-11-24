package wf_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	wtA   struct{}
	wtB   struct{}
	wtEnd struct{}
)

// wtChainDef is two agents in a row, then a terminal: finishing the first
// leaves the second pending.
func wtChainDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[wtA, clAnswer](d)
	b := wf.NewAgent[wtB, clAnswer](d)
	end := wf.NewTerminal[wtEnd](d, wf.Success)
	a.Spec(wf.PromptText("First."), wf.RetryPolicy[wtA]{})
	b.Spec(wf.PromptText("Second."), wf.RetryPolicy[wtB]{})
	toB := a.To(b, "next")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[wtA] { return toB }, toB)
	toEnd := b.To(end, "ok")
	b.Route(func(clAnswer, wf.Ctx) wf.Edge[wtB] { return toEnd }, toEnd)
	return d
}

// waiter is an Engine over MemJournal on the real clock: Wait sleeps on real
// timers, so the tests use millisecond windows.
type waiter struct {
	t *testing.T
	e wf.Executor
	j wf.Journal
}

func newWaiter(t *testing.T, d *wf.Def, opts wf.EngineOptions) *waiter {
	t.Helper()
	opts.Defs = append(opts.Defs, d)
	j := wf.MemJournal()
	return &waiter{t: t, e: wf.NewEngine(j, nil, opts), j: j}
}

func (w *waiter) start(d *wf.Def) wf.RunID {
	w.t.Helper()
	run, err := w.e.Run(d, nil, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	return run
}

// started hands out one unit and claims it.
func (w *waiter) started(run wf.RunID) wf.Instruction {
	w.t.Helper()
	ins, err := w.e.HandOut(run, 1)
	if err != nil || len(ins) != 1 || ins[0].Name == "" {
		w.t.Fatalf("HandOut(1) = %+v, %v, want one unit", ins, err)
	}
	if err := w.e.Started(run, ins[0].Name, claim("a")); err != nil {
		w.t.Fatal(err)
	}
	return ins[0]
}

// wait calls Wait and fails the test when it returns later than limit.
func (w *waiter) wait(run wf.RunID, since int64, timeout time.Duration) wf.WaitResult {
	w.t.Helper()
	begin := time.Now()
	res, err := w.e.Wait(run, since, timeout)
	if err != nil {
		w.t.Fatal(err)
	}
	if took := time.Since(begin); took > timeout+2*time.Second {
		w.t.Fatalf("Wait took %v with timeout %v", took, timeout)
	}
	return res
}

func (w *waiter) after(d time.Duration, f func() error) {
	go func() {
		time.Sleep(d)
		if err := f(); err != nil {
			w.t.Error(err)
		}
	}()
}

func oneEvent(t *testing.T, res wf.WaitResult, kind wf.EventKind) wf.Event {
	t.Helper()
	if len(res.Events) != 1 || res.Events[0].Kind != kind {
		t.Fatalf("events = %+v, want exactly one %q", res.Events, kind)
	}
	return res.Events[0]
}

// eventsOf are the events of the given kind.
func eventsOf(res wf.WaitResult, kind wf.EventKind) []wf.Event {
	var out []wf.Event
	for _, ev := range res.Events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestWaitReturnsOnDone(t *testing.T) {
	d := wtChainDef("wt-done")
	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d)
	u := w.started(run)
	w.after(50*time.Millisecond, func() error {
		_, err := w.e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
		return err
	})

	res := w.wait(run, -1, 10*time.Second)
	ev := oneEvent(t, res, wf.EventDone)
	if ev.Name != u.Name || ev.State != "wtA" || ev.Seq == 0 {
		t.Fatalf("event = %+v, want done of %s at wtA", ev, u.Name)
	}
	if want := (wf.UnitCounts{Ready: 1}); res.UnitCounts != want {
		t.Fatalf("counts = %+v, want ready 1, handed 0, running 0", res.UnitCounts)
	}
}

func TestWaitReturnsOnFailed(t *testing.T) {
	d := lsDef("wt-failed", false)
	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d)
	u := w.started(run)
	w.after(50*time.Millisecond, func() error {
		_, err := w.e.Failed(run, u.Name, "gave up")
		return err
	})

	ev := oneEvent(t, w.wait(run, -1, 10*time.Second), wf.EventFailed)
	if ev.Name != u.Name {
		t.Fatalf("event = %+v, want failed of %s", ev, u.Name)
	}
}

// An expired hand-out wakes Wait with a re-offerable window event: the unit is
// not lost, it counts as pending, and the next HandOut offers it again under
// the same name, which closes the event.
func TestWaitReturnsOnHandOutWindowExpiry(t *testing.T) {
	d := lsDef("wt-window", true)
	w := newWaiter(t, d, wf.EngineOptions{HandOutWindow: 150 * time.Millisecond})
	run := w.start(d)
	ins, err := w.e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOut = %+v, %v", ins, err)
	}

	begin := time.Now()
	res := w.wait(run, -1, 10*time.Second)
	ev := oneEvent(t, res, wf.EventWindow)
	if ev.Name != ins[0].Name || ev.Detail != wf.DetailReofferable {
		t.Fatalf("event = %+v, want %s re-offerable", ev, ins[0].Name)
	}
	if took := time.Since(begin); took < 100*time.Millisecond {
		t.Fatalf("Wait returned after %v, before the window passed", took)
	}
	if want := (wf.UnitCounts{Ready: 1}); res.UnitCounts != want {
		t.Fatalf("counts = %+v, want the expired hand-out counted ready, not handed", res.UnitCounts)
	}
	again, err := w.e.HandOut(run, 1)
	if err != nil || len(again) != 1 || again[0].Name != ins[0].Name {
		t.Fatalf("re-offer = %+v, %v, want %s again", again, err, ins[0].Name)
	}
	// The re-offer opened a fresh window: nothing is re-offerable until it passes.
	ev = oneEvent(t, w.wait(run, -1, 50*time.Millisecond), wf.EventTimeout)
	if ev.Name != "" {
		t.Fatalf("event = %+v, want a plain timeout inside the new window", ev)
	}
}

type (
	wtPause struct{}
	wtGone  struct{}
	wtOut   struct{ Down bool }
)

func TestWaitReturnsOnBackoffDue(t *testing.T) {
	d := wf.New("wt-backoff")
	a := wf.NewAgent[wtA, wtOut](d)
	done := wf.NewTerminal[wtEnd](d, wf.Success)
	pause := wf.NewTerminal[wtPause](d, wf.Stopped)
	gone := wf.NewTerminal[wtGone](d, wf.Stopped)
	a.Spec(wf.PromptText("Work."),
		wf.Retry(wf.Const(5), a.To(gone, "exhausted")).OnClassBackoff(wf.Unavailable, a.To(pause, "pause"), 200*time.Millisecond),
		wf.Checkf("up", func(o wtOut, _ wf.Ctx) error {
			if o.Down {
				return fmt.Errorf("%w: tool", wf.ErrUnavailable)
			}
			return nil
		}))
	ok := a.To(done, "ok")
	a.Route(func(wtOut, wf.Ctx) wf.Edge[wtA] { return ok }, ok)

	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d)
	ins, err := w.e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("Next = %+v, %v", ins, err)
	}
	if _, err := w.e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{"Down":true}`)}); err != nil {
		t.Fatal(err)
	}

	begin := time.Now()
	res := w.wait(run, -1, 10*time.Second)
	ev := oneEvent(t, res, wf.EventDeadline)
	if ev.Detail != "backoff" || ev.State != "wtA" {
		t.Fatalf("event = %+v, want the backoff of wtA", ev)
	}
	if took := time.Since(begin); took < 150*time.Millisecond {
		t.Fatalf("Wait returned after %v, before the backoff was due", took)
	}
}

type (
	wtAsk  struct{}
	wtStop struct{}
)

type wtAns string

func wtGateDef(name string, timeout time.Duration) *wf.Def {
	d := wf.New(name)
	ans := wf.NewEnum(d, name+"-answer", wtAns("go"))
	g := wf.NewGate[wtAsk, wtAns](d, "Go?", ans, wf.PolicyAnswerable)
	stop := wf.NewTerminal[wtStop](d, wf.Stopped)
	g.Answers(wf.On(wtAns("go"), g.To(stop, "go")))
	if timeout > 0 {
		g.Timeout(timeout, g.To(stop, "timeout"))
	}
	return d
}

func TestWaitReturnsOnGateTimeout(t *testing.T) {
	d := wtGateDef("wt-gate-timeout", 150*time.Millisecond)
	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d) // no Next call: the gate is open from the start

	// The timeout edge reaches a terminal, so the run ends with it.
	res := w.wait(run, -1, 10*time.Second)
	got := eventsOf(res, wf.EventDeadline)
	if len(got) != 1 || got[0].Detail != "gate timeout" || got[0].State != "wtAsk" || len(eventsOf(res, wf.EventRunEnded)) != 1 {
		t.Fatalf("events = %+v, want the gate timeout of wtAsk and the run end", res.Events)
	}
}

func TestWaitReturnsOnGateAnswered(t *testing.T) {
	d := wtGateDef("wt-gate-answer", 0)
	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d)
	if _, err := w.e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	w.after(50*time.Millisecond, func() error { return w.e.Answer(run, "wtAsk", "go") })

	res := w.wait(run, -1, 10*time.Second)
	got := eventsOf(res, wf.EventGateAnswered)
	if len(got) != 1 || got[0].State != "wtAsk" || got[0].Detail != "go" || len(eventsOf(res, wf.EventDeadline)) != 0 {
		t.Fatalf("events = %+v, want wtAsk answered go and no deadline", res.Events)
	}
}

func TestWaitReturnsOnRunEnd(t *testing.T) {
	d := lsDef("wt-end", false)
	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d)
	u := w.started(run)
	w.after(50*time.Millisecond, func() error {
		_, err := w.e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
		return err
	})

	res := w.wait(run, -1, 10*time.Second)
	if len(res.Events) != 2 || res.Events[0].Kind != wf.EventDone || res.Events[1].Kind != wf.EventRunEnded {
		t.Fatalf("events = %+v, want done then run ended", res.Events)
	}
	// Once the run has ended, a Wait past its last record still returns at once.
	again := w.wait(run, res.Seq, 10*time.Second)
	oneEvent(t, again, wf.EventRunEnded)
}

func TestWaitTimesOut(t *testing.T) {
	d := lsDef("wt-timeout", false)
	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d)
	w.started(run)

	begin := time.Now()
	res := w.wait(run, -1, 100*time.Millisecond)
	oneEvent(t, res, wf.EventTimeout)
	if took := time.Since(begin); took < 100*time.Millisecond {
		t.Fatalf("Wait returned after %v, before its timeout", took)
	}
	if want := (wf.UnitCounts{Running: 1}); res.UnitCounts != want {
		t.Fatalf("counts = %+v, want running 1", res.UnitCounts)
	}
}

// busyJournal wraps a journal so that Watch is always ready and every Records
// call takes delay (30 ms when zero), which it logs from the moment it starts.
type busyJournal struct {
	wf.Journal
	delay  time.Duration
	mu     sync.Mutex
	on     bool
	starts []time.Time
}

func (b *busyJournal) Records(run wf.RunID) ([]wf.JournalRecord, error) {
	b.mu.Lock()
	if b.on {
		b.starts = append(b.starts, time.Now())
	}
	slow := b.on
	b.mu.Unlock()
	if slow {
		d := b.delay
		if d == 0 {
			d = 30 * time.Millisecond
		}
		time.Sleep(d)
	}
	return b.Journal.Records(run)
}

func (b *busyJournal) Watch(run wf.RunID, stop <-chan struct{}) <-chan struct{} {
	// Buffered and pre-filled, so the signal is ready the moment Wait selects
	// on it, not only once a goroutine has been scheduled.
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	go func() {
		for {
			select {
			case ch <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()
	return ch
}

// A Wait whose change signal is always ready must still stop at its timeout:
// the cycle in progress may finish, but none starts after the timer fired.
func TestWaitChecksTimeoutBeforeEachCycle(t *testing.T) {
	d := lsDef("wt-precheck", false)
	// Each cycle takes 100 ms and the timeout is 125 ms: with the check before
	// each cycle, cycles start at ~0 and ~100 ms; without it a third starts at
	// ~200 ms. The bound sits between, 40 ms of slack over the legitimate start
	// and 35 ms short of the wrong one.
	bj := &busyJournal{Journal: wf.MemJournal(), delay: 100 * time.Millisecond}
	e := wf.NewEngine(bj, nil, wf.EngineOptions{Defs: []*wf.Def{d}})
	w := &waiter{t: t, e: e, j: bj}
	run := w.start(d)
	w.started(run)

	bj.mu.Lock()
	bj.on = true
	bj.mu.Unlock()
	begin := time.Now()
	res := w.wait(run, -1, 125*time.Millisecond)
	oneEvent(t, res, wf.EventTimeout)

	bj.mu.Lock()
	defer bj.mu.Unlock()
	if len(bj.starts) < 2 {
		t.Fatalf("Wait ran %d cycles, want at least 2", len(bj.starts))
	}
	for i, s := range bj.starts {
		if s.After(begin.Add(165 * time.Millisecond)) {
			t.Fatalf("cycle %d started %v after begin, want before 165ms (125ms timeout, 40ms slack): a cycle began after the timeout", i, s.Sub(begin))
		}
	}
}

func TestWaitSinceBaseline(t *testing.T) {
	d := wtChainDef("wt-since")
	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d)
	u := w.started(run)
	if _, err := w.e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}

	// The done landed after the last hand-out but before the call.
	res := w.wait(run, -1, 10*time.Second)
	if ev := oneEvent(t, res, wf.EventDone); ev.Name != u.Name {
		t.Fatalf("event = %+v, want done of %s", ev, u.Name)
	}
	// Passing the returned Seq skips it: only the timeout is left.
	oneEvent(t, w.wait(run, res.Seq, 100*time.Millisecond), wf.EventTimeout)
}

func TestEventLine(t *testing.T) {
	for _, tc := range []struct {
		ev   wf.Event
		want string
	}{
		{wf.Event{Kind: wf.EventDone, Name: "A#1", State: "A"}, "done A#1"},
		{wf.Event{Kind: wf.EventDeadline, State: "Ask", Detail: "gate timeout"}, "deadline due Ask gate timeout"},
		{wf.Event{Kind: wf.EventTimeout}, "timeout"},
	} {
		if got := tc.ev.Line(); got != tc.want {
			t.Errorf("Line(%+v) = %q, want %q", tc.ev, got, tc.want)
		}
	}
}

func TestNextWaitCLIPrintsSpawnLine(t *testing.T) {
	d := wtChainDef("wt-cli")
	dir := t.TempDir()
	e := wf.NewEngine(wf.FileJournal(dir), nil, wf.EngineOptions{Defs: []*wf.Def{d}})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOut = %+v, %v", ins, err)
	}
	if err := e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}

	// next --wait hands out the unit the done made ready and prints its spawn
	// line, which carries the unit's brief.
	var o, er bytes.Buffer
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}}
	if code := wf.CLI(cfg, []string{"next", string(run), "1", "--state-dir", dir, "--timeout", "5s"}, &o, &er); code != 0 {
		t.Fatalf("next --wait = %d, stderr %q", code, er.String())
	}
	lines := strings.Split(strings.TrimRight(o.String(), "\n"), "\n")
	if len(lines) < 5 || !strings.HasPrefix(lines[0], "start a child agent for ") || strings.HasPrefix(lines[0], "start a child agent for "+ins[0].Name+" ") ||
		!strings.Contains(lines[2], " prompt "+string(run)+" --claim ") {
		t.Fatalf("stdout = %q, want one spawn line for the next unit carrying its brief", o.String())
	}

	o.Reset()
	if code := wf.CLI(cfg, []string{"next", "no-such-run", "1", "--state-dir", dir}, &o, &er); code != 3 {
		t.Fatalf("next --wait on an unknown run = %d, want 3", code)
	}
}

// countJournal counts the Records calls: one per Wait cycle.
type countJournal struct {
	wf.Journal
	mu sync.Mutex
	n  int
}

func (c *countJournal) Records(run wf.RunID) ([]wf.JournalRecord, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Journal.Records(run)
}

func (c *countJournal) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A transcript that is always fresh must neither flood the journal nor keep
// Wait spinning on its own appends.
//
// Bounds: the heartbeat debounce is a quarter of the stall window (no lease
// here), so 100 ms; a 1s wait journals at most 1s/100ms = 10 heartbeats, plus 2
// for the edges. The old test allowed 30 cycles in 5s, 6 per second, so a 1s
// wait allows 6 plus 2.
func TestWaitBusyTranscriptBoundedRecords(t *testing.T) {
	const (
		stall    = 400 * time.Millisecond
		debounce = stall / 4
		wait     = time.Second
		maxRecs  = int(wait/debounce) + 2
		maxCycle = 6*int(wait/time.Second) + 2
	)
	d := lsDef("wt-busy", false)
	cj := &countJournal{Journal: wf.MemJournal()}
	e := wf.NewEngine(cj, nil, wf.EngineOptions{
		Defs:            []*wf.Def{d},
		StallDefault:    stall,
		TranscriptMtime: func(string) (time.Time, error) { return time.Now(), nil },
	})
	w := &waiter{t: t, e: e, j: cj}
	run := w.start(d)
	ins, err := e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOut = %+v, %v", ins, err)
	}
	if err := e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Prompt(run, ins[0].Name); err != nil {
		t.Fatal(err)
	}
	size := func() int {
		unlock, err := cj.Lock(run)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		recs, err := cj.Journal.Records(run) // not counted
		if err != nil {
			t.Fatal(err)
		}
		return len(recs)
	}
	before, calls := size(), cj.calls()

	res := w.wait(run, -1, wait)
	oneEvent(t, res, wf.EventTimeout)
	after, cycles := size(), cj.calls()-calls
	if grew := after - before; grew > maxRecs {
		t.Fatalf("the journal grew by %d records over a %v wait, want at most %d", grew, wait, maxRecs)
	}
	if cycles > maxCycle {
		t.Fatalf("Wait ran %d cycles in %v, want at most %d: it woke on its own appends", cycles, wait, maxCycle)
	}
}

// What Wait's own drive cycle journals is returned in that call; only the wake
// signal is blind to it.
func TestWaitReturnsOwnCycleEvents(t *testing.T) {
	t.Run("stall", func(t *testing.T) {
		d := lsDef("wt-own-stall", true)
		m := newTranscripts()
		w := newWaiter(t, d, wf.EngineOptions{StallDefault: 100 * time.Millisecond, TranscriptMtime: m.mtime})
		run := w.start(d)
		ins, err := w.e.HandOut(run, 1)
		if err != nil || len(ins) != 1 {
			t.Fatalf("HandOut = %+v, %v", ins, err)
		}
		m.set("/t/a.jsonl", time.Now().Add(-time.Hour))
		if err := w.e.Started(run, ins[0].Name, claim("a")); err != nil {
			t.Fatal(err)
		}
		if _, err := w.e.Prompt(run, ins[0].Name); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond) // the stall is already due at the call
		begin := time.Now()
		oneEvent(t, w.wait(run, -1, 5*time.Second), wf.EventStall)
		if took := time.Since(begin); took > time.Second {
			t.Fatalf("Wait took %v, want the stall returned by the first cycle", took)
		}
	})
	t.Run("re-offerable hand-out", func(t *testing.T) {
		d := lsDef("wt-own-window", true)
		w := newWaiter(t, d, wf.EngineOptions{HandOutWindow: 50 * time.Millisecond})
		run := w.start(d)
		ins, err := w.e.HandOut(run, 1)
		if err != nil || len(ins) != 1 {
			t.Fatalf("HandOut = %+v, %v", ins, err)
		}
		time.Sleep(100 * time.Millisecond) // the window is already past
		begin := time.Now()
		ev := oneEvent(t, w.wait(run, -1, 5*time.Second), wf.EventWindow)
		if ev.Name != ins[0].Name || ev.Detail != wf.DetailReofferable {
			t.Fatalf("event = %+v, want %s re-offerable", ev, ins[0].Name)
		}
		if took := time.Since(begin); took > time.Second {
			t.Fatalf("Wait took %v, want the re-offer returned by the first cycle", took)
		}
	})
	t.Run("detached result", func(t *testing.T) {
		fr := newDetRunner()
		d := detDef(idempotentDetached(time.Minute))
		opts := detOpts(t, fr)
		opts.EffectPoll = 50 * time.Millisecond
		w := newWaiter(t, d, opts)
		run, err := w.e.Run(d, map[string]string{"topic": "build"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		unlock, err := w.j.Lock(run)
		if err != nil {
			t.Fatal(err)
		}
		recs, err := w.j.Records(run)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		var intent int64
		for _, r := range recs {
			if r.Kind == wf.JournalEffectIntent {
				intent = r.Seq
			}
		}
		fr.set(intent, wf.DetachedPresent, wf.RunResult{Exit: 0}) // lands before the call
		begin := time.Now()
		res := w.wait(run, 0, 10*time.Second)
		if evs := eventsOf(res, wf.EventEffect); len(evs) != 1 || evs[0].Detail != "applied" {
			t.Fatalf("events = %+v, want one effect applied", res.Events)
		}
		if took := time.Since(begin); took > time.Second {
			t.Fatalf("Wait took %v, want the result returned by the first cycle", took)
		}
	})
	t.Run("blocked halt", func(t *testing.T) {
		h, run := eiHaltWorld(t, nil)
		begin := time.Now()
		res, err := h.e.Wait(run, -1, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		oneEvent(t, res, wf.EventBlocked)
		if took := time.Since(begin); took > time.Second {
			t.Fatalf("Wait took %v, want the halt returned by the first cycle", took)
		}
	})
}

// Marker types for the held-run predicate tests.
type (
	PhSub   struct{}
	PhFin   struct{}
	PhFan   struct{}
	PhMerge struct{}
	PhPart  struct{}
)

// heldSubInMapDef is a Map (no sibling policy) of two items, each a Sub over
// retryItemChild: item 1's inner compute halts (depth 2), item 2's inner agent
// stays live.
func heldSubInMapDef(fail *bool) *wf.Def {
	md := wf.New("ph-mid")
	sub := wf.NewSub[PhSub, chItem, chRes](md, retryItemChild(fail), func(c wf.Ctx) chItem { return wf.InputOf[chItem](c) })
	fin := wf.NewTerminal[PhFin](md, wf.Success)
	sub.Exits(wf.OnExit[RrOK](sub.To(fin, "ok")))
	wf.Yield(fin, func(wf.Ctx) chRes { return chRes{} })
	d := wf.New("ph-sub-in-map")
	m := wf.NewMap[PhFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](md))
	merged := wf.NewTerminal[PhMerge](d, wf.Success)
	partial := wf.NewTerminal[PhPart](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllOK(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// heldEffectMapDef is a Map (no sibling policy) of two items over a child whose
// A-to-B edge carries a non-idempotent effect.
func heldEffectMapDef() *wf.Def {
	d := wf.New("ph-effect-map")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, twoStepChild(mergeEffect(wf.NonIdempotent("pushes"))))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// heldMapCase halts item 1 of retryMapPolicyDef under the given policy.
func heldMapCase(policy wf.SiblingPolicy) func(*testing.T) (*harness, wf.RunID) {
	return func(t *testing.T) (*harness, wf.RunID) {
		fail := true
		d := retryMapPolicyDef(&fail, policy)
		h := newHarness(t, d, nil)
		return h, h.run(d, nil)
	}
}

// heldEffectSettledMapCase halts chFan[1] by settling its effect not-applied.
func heldEffectSettledMapCase(t *testing.T) (*harness, wf.RunID) {
	d := heldEffectMapDef()
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	run := c.run(d, nil)
	c.crashAt(run, `{}`)
	for _, in := range c.next(run) {
		if in.Kind == wf.InstructionManualVerification {
			if err := settleAs(c.e, run, in.Effect, false); err != nil {
				t.Fatal(err)
			}
			return c.harness, run
		}
	}
	t.Fatal("no manual verification to settle")
	return nil, ""
}

// heldSubHaltCase halts the root through a Sub whose child halts.
func heldSubHaltCase(t *testing.T) (*harness, wf.RunID) {
	fail := true
	c, run := bareHaltOf(t, haltSubDef(&fail))
	return c.harness, run
}

// bareHaltOf starts d, whose leaf compute fails at once: a bare Blocked halt
// (no retry or stop answer) that a Sub settles and propagates to its parent.
func bareHaltOf(t *testing.T, d *wf.Def) (*crashHarness, wf.RunID) {
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	run := c.run(d, nil)
	if ins := c.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none: the leaf halts at once", ins)
	}
	return c, run
}

// haltSubDef is a Sub over retryItemChild for item 1: while *fail holds the
// child's compute fails, a bare halt the Sub propagates to the root.
func haltSubDef(fail *bool) *wf.Def {
	d := wf.New("ph-sub-halt")
	sub := wf.NewSub[chSubRun, chItem, chRes](d, retryItemChild(fail), func(wf.Ctx) chItem { return chItem{N: 1} })
	fin := wf.NewTerminal[chFin](d, wf.Success)
	sub.Exits(wf.OnExit[RrOK](sub.To(fin, "ok")))
	return d
}

// heldHaltOf runs d up to its non-idempotent effect, crashes there and settles
// the effect not applied, which halts the instance that carried it with the
// exhaustion halt (G4): its spawner waits for the answer.
func heldHaltOf(t *testing.T, d *wf.Def) (*crashHarness, wf.RunID) {
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	run := c.run(d, nil)
	c.crashAt(run, `{}`)
	if err := settleAs(c.e, run, c.next(run)[0].Effect, false); err != nil {
		t.Fatal(err)
	}
	return c, run
}

// subInSubDef is a Sub over a Sub over retryItemChild for item 1: while *fail
// holds, the leaf's halt blocks the middle Sub and through it the root.
func subInSubDef(fail *bool) *wf.Def {
	md := wf.New("ph-sub-mid")
	inner := wf.NewSub[PhSub, chItem, chRes](md, retryItemChild(fail), func(wf.Ctx) chItem { return chItem{N: 1} })
	mfin := wf.NewTerminal[PhFin](md, wf.Success)
	inner.Exits(wf.OnExit[RrOK](inner.To(mfin, "ok")))
	wf.Yield(mfin, func(wf.Ctx) chRes { return chRes{} })
	d := wf.New("ph-sub-in-sub")
	outer := wf.NewSub[chSubRun, chItem, chRes](d, wf.Typed[chItem, chRes](md), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[chFin](d, wf.Success)
	outer.Exits(wf.OnExit[PhFin](outer.To(fin, "ok")))
	return d
}

var heldCases = []struct {
	name  string
	held  bool
	state string // the qualified state of the blocked event
	build func(*testing.T) (*harness, wf.RunID)
}{
	{"map child halt, unset policy", false, "", heldMapCase(0)},
	{"map child halt, ContinueSiblings", false, "", heldMapCase(wf.ContinueSiblings)},
	{"map child halt, CancelSiblings", false, "", heldMapCase(wf.CancelSiblings)},
	{"sub inside a map child halts", false, "", func(t *testing.T) (*harness, wf.RunID) {
		fail := true
		d := heldSubInMapDef(&fail)
		h := newHarness(t, d, nil)
		return h, h.run(d, nil)
	}},
	{"root command halt", true, "cRun", func(t *testing.T) (*harness, wf.RunID) { return eiHaltWorld(t, nil) }},
	{"sub halt propagates to the root", true, "chSubRun", heldSubHaltCase},
}

// The run is held only where nothing above a halt will settle it: a halt in a
// Map child or a Sub inside one is absorbed (the Map goes on or the run ends on
// its own), while a root halt, and a halt that propagates to the root, block.
func TestWaitBlockedOnlyWhenRunHeld(t *testing.T) {
	for _, tc := range heldCases {
		t.Run(tc.name, func(t *testing.T) {
			h, run := tc.build(t)
			res, err := h.e.Wait(run, 0, 200*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			evs := eventsOf(res, wf.EventBlocked)
			if tc.held {
				if len(evs) != 1 || evs[0].State != tc.state {
					t.Fatalf("blocked events = %+v, want exactly one at %s", evs, tc.state)
				}
				return
			}
			if len(evs) != 0 {
				t.Fatalf("blocked events = %+v, want none for an absorbed halt", evs)
			}
			if st := h.status(run); st.Phase == wf.RunBlocked {
				t.Fatalf("status = %s: the run is not held", st.Phase)
			}
			if st := h.status(run); st.Phase != wf.RunEnded && len(h.next(run)) == 0 {
				t.Fatalf("run %s hands out nothing and has not ended", st.Phase)
			}
		})
	}
}

// A Wait started on a run that is already held (the halt journaled before it,
// by an earlier Next) reports blocked on its first cycle.
func TestWaitBlockedWhenAlreadyHeld(t *testing.T) {
	h, run := eiHaltWorld(t, nil)
	h.next(run)
	begin := time.Now()
	res, err := h.e.Wait(run, int64(len(h.records(run))), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("Wait took %v, want a return well before the 5s timeout", took)
	}
	if ev := oneEvent(t, res, wf.EventBlocked); ev.State != "cRun" || !strings.Contains(ev.Detail, "permission denied") {
		t.Fatalf("event = %+v, want blocked at cRun with the halt reason", ev)
	}
}

// Wait and Status share one predicate: Status reports the run blocked, with a
// blocked: reason, exactly when Wait reported a blocked event.
func TestWaitAndStatusAgreeOnHeld(t *testing.T) {
	for _, tc := range heldCases {
		t.Run(tc.name, func(t *testing.T) {
			h, run := tc.build(t)
			res, err := h.e.Wait(run, 0, 100*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			waitBlocked := len(eventsOf(res, wf.EventBlocked)) > 0
			st := h.status(run)
			statusBlocked := st.Phase == wf.RunBlocked
			reason := strings.HasPrefix(st.NothingReadyReason, "blocked:")
			if waitBlocked != tc.held || statusBlocked != waitBlocked || reason != waitBlocked {
				t.Fatalf("held=%v: wait blocked=%v, status phase=%s reason=%q", tc.held, waitBlocked, st.Phase, st.NothingReadyReason)
			}
		})
	}
}

// A Sub halt that propagated to the root is recovered by retrying the settled
// child and then the root: the child is handed out again and the run is no
// longer held.
func TestRetryRecoversSubHaltPropagatedToRoot(t *testing.T) {
	fail := true
	c, run := bareHaltOf(t, haltSubDef(&fail))
	h := c.harness
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %s, want blocked before the retry", st.Phase)
	}
	fail = false // the human cleared the cause
	if err := h.e.Retry(run, "chSubRun#1", false); err != nil {
		t.Fatalf("Retry of the settled child: %v", err)
	}
	if err := h.e.Retry(run, "", false); err != nil {
		t.Fatalf("Retry of the root: %v", err)
	}
	if st := h.status(run); st.Phase == wf.RunBlocked {
		t.Fatalf("status = %s after Retry, want the run going on: %+v", st.Phase, st.Instances)
	}
	if ins := h.next(run); len(ins) == 0 {
		t.Fatal("the revived child was not handed out again")
	}
	if n := len(h.kind(run, wf.JournalRetried)); n != 2 {
		t.Fatalf("retried records = %d, want 2", n)
	}
}

// A parent halted on a child's reason clears when that child ends: retrying
// only the child is enough, the run needs no top-level Retry.
func TestParentBlockedClearsWhenChildEnds(t *testing.T) {
	fail := true
	c, run := bareHaltOf(t, haltSubDef(&fail))
	fail = false
	retryLeafToEnd(t, c, run, "chSubRun#1")
}

// A Sub inside a Sub: retrying only the leaf revives the middle Sub its parent
// already settled Blocked, which ends and clears the root in turn.
func TestParentBlockedClearsWhenNestedChildEnds(t *testing.T) {
	fail := true
	c, run := bareHaltOf(t, subInSubDef(&fail))
	fail = false
	retryLeafToEnd(t, c, run, "chSubRun#1/PhSub#1")
	mid := 0
	for _, r := range c.kind(run, wf.JournalRetried) {
		if r.Instance == "chSubRun#1" {
			mid++
		}
	}
	if mid != 1 {
		t.Fatalf("middle retried records = %d, want the engine's one", mid)
	}
}

// retryLeafToEnd retries only the halted leaf of a blocked run and drives it to
// its end, which needs no retry above the leaf. Right after the engine journals
// its own retried on the root, a fresh engine over the same journal reports the
// same status and appends and applies nothing more.
func retryLeafToEnd(t *testing.T, c *crashHarness, run wf.RunID, leaf string) {
	t.Helper()
	if st := c.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %s, want blocked before the retry", st.Phase)
	}
	if err := c.e.Retry(run, leaf, false); err != nil {
		t.Fatalf("Retry of the settled leaf: %v", err)
	}
	if st := c.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %s: the leaf has not ended, the root stays blocked", st.Phase)
	}
	rootRetried := func() int {
		n := 0
		for _, r := range c.kind(run, wf.JournalRetried) {
			if r.Instance == "" {
				n++
			}
		}
		return n
	}
	replayed := false
	for i := 0; i < 10 && c.status(run).Phase != wf.RunEnded; i++ {
		for _, in := range c.next(run) {
			c.submit(run, in, `{}`)
		}
		if replayed || rootRetried() == 0 {
			continue
		}
		replayed = true
		before, recs, applied := c.status(run), len(c.records(run)), c.w.appliedCount()
		c.resume()
		if after := c.status(run); !reflect.DeepEqual(after, before) {
			t.Fatalf("status after reopen = %+v, want %+v", after, before)
		}
		if n := len(c.records(run)); n != recs {
			t.Fatalf("records after reopen = %d, want %d: the replay appended", n, recs)
		}
		if n := c.w.appliedCount(); n != applied {
			t.Fatalf("applied after reopen = %d, want %d", n, applied)
		}
	}
	if !replayed {
		t.Fatal("the root was never retried by the engine")
	}
	if st := c.status(run); st.Phase != wf.RunEnded {
		t.Fatalf("status = %s, want ended without a retry above the leaf: %q", st.Phase, st.NothingReadyReason)
	}
	// The test retried only the leaf; the one retried record on the root is
	// the engine's own, journaled when its child ended.
	if n := rootRetried(); n != 1 {
		t.Fatalf("root retried records = %d, want the engine's one", n)
	}
}
