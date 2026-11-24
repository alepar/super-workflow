package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/broaden"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/checkrepair"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/escalation"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/noprogress"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/typedexit"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// world is a scripted adapter world: shell.run applies pop scripted results
// (exit 0 when the queue is empty), every apply is recorded, observations and
// effect checks come from the embedded scriptedRuntime.
type world struct {
	*scriptedRuntime
	runs     []wf.RunResult
	applyErr error
	checks   int
	checkErr []error // popped by successive Checks: each is returned as that Check's error
}

func newWorld() *world { return &world{scriptedRuntime: newFakeRuntime()} }

func (w *world) Read(ws string, d wf.ObservationDesc) (json.RawMessage, error) {
	if d.Adapter == "fsx" {
		return json.RawMessage(`{}`), nil
	}
	return w.scriptedRuntime.Read(ws, d)
}

func (w *world) Apply(ws string, e wf.ExternalEffect) (json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.applied = append(w.applied, e)
	if w.applyErr != nil {
		return nil, w.applyErr
	}
	if e.Adapter == "shell" {
		var r wf.RunResult
		if len(w.runs) > 0 {
			r, w.runs = w.runs[0], w.runs[1:]
		}
		return json.Marshal(r)
	}
	if w.failOp[e.Adapter+"."+e.Op] {
		return nil, errors.New("apply failed")
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (w *world) Check(ws string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	w.mu.Lock()
	w.checks++
	var cerr error
	if len(w.checkErr) > 0 {
		cerr, w.checkErr = w.checkErr[0], w.checkErr[1:]
	}
	w.mu.Unlock()
	if cerr != nil {
		return wf.EffectUnknown, cerr
	}
	return w.scriptedRuntime.Check(ws, e)
}

func (w *world) appliedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.applied)
}

// crashJournal fails the append of the first record of kind once armed,
// standing for a process that died just before writing it.
type crashJournal struct {
	wf.Journal
	kind  wf.JournalKind
	armed bool
}

var errCrash = errors.New("crash")

func (j *crashJournal) Append(r wf.JournalRecord) error {
	if j.armed && r.Kind == j.kind {
		j.armed = false
		return errCrash
	}
	return j.Journal.Append(r)
}

// crashHarness is a harness whose journal can crash. resume() is a fresh Engine
// over the same records.
type crashHarness struct {
	*harness
	cj *crashJournal
	w  *world
	d  *wf.Def
}

func newCrashHarness(t *testing.T, d *wf.Def, w *world, kind wf.JournalKind) *crashHarness {
	t.Helper()
	h := newHarness(t, d, w)
	cj := &crashJournal{Journal: h.j, kind: kind}
	h.e = wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
	return &crashHarness{harness: h, cj: cj, w: w, d: d}
}

// resume swaps in a fresh Engine over the surviving journal.
func (c *crashHarness) resume() {
	c.e = wf.NewEngine(c.j, c.reg, wf.EngineOptions{Defs: []*wf.Def{c.d}, Clock: func() time.Time { return c.now }})
}

// crashAt answers the open instruction and expects the process to die on the
// append armed in the journal. The crash comes in the drive after the
// submission's decision is journaled, which the verb does not report: the
// decision stands and the run's next driver meets the rest.
func (c *crashHarness) crashAt(run wf.RunID, out string) {
	c.t.Helper()
	c.cj.armed = true
	in := c.next(run)[0]
	if _, err := c.e.Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(out)}); err != nil && !errors.Is(err, errCrash) {
		c.t.Fatalf("Submit error = %v, want the crash or none", err)
	}
	if c.cj.armed {
		c.t.Fatal("the armed append never ran: no crash")
	}
	c.resume()
}

// ---------------------------------------------------------------- example scenarios

func TestCheckrepairOnEngine(t *testing.T) {
	testRun := map[string]any{"argv": []string{"go", "test", "./..."}, "dir": ""}
	vetRun := map[string]any{"argv": []string{"go", "vet", "./..."}, "dir": ""}

	t.Run("green first time", func(t *testing.T) {
		d := checkrepair.Definition()
		w := newWorld()
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		if ins := h.next(run); len(ins) != 0 {
			t.Fatalf("instructions = %+v, want none", ins)
		}
		statusOf(t, h.status(run), wf.RunEnded, "Green")
		if w.appliedCount() != 1 || w.applied[0].Adapter != "shell" || w.applied[0].Op != "run" {
			t.Fatalf("applied = %+v, want the one test run", w.applied)
		}
	})

	t.Run("fail, repair, green", func(t *testing.T) {
		d := checkrepair.Definition()
		w := newWorld()
		w.runs = []wf.RunResult{{Exit: 1, Stderr: "boom"}, {Exit: 0}}
		w.set("shell", "run", testRun, wf.RunResult{Exit: 1, Stderr: "boom"})
		w.set("shell", "run", vetRun, wf.RunResult{})
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		ins := h.next(run)
		if len(ins) != 1 || ins[0].State != "Repair" || !strings.Contains(ins[0].Prompt, "boom") {
			t.Fatalf("instructions = %+v, want Repair carrying the captured stderr", ins)
		}
		st := h.drive(run, map[string][]string{"Repair": {`{"Summary":"fixed"}`}})
		statusOf(t, st, wf.RunEnded, "Green")
		if w.appliedCount() != 2 {
			t.Fatalf("test runs = %d, want 2", w.appliedCount())
		}
	})

	t.Run("unreadable run gives up", func(t *testing.T) {
		d := checkrepair.Definition()
		w := newWorld()
		w.runs = []wf.RunResult{{Exit: 1}}
		w.down["shell.run"] = true
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		h.next(run)
		statusOf(t, h.status(run), wf.RunEnded, "GaveUp")
	})

	t.Run("repairs exhausted", func(t *testing.T) {
		d := checkrepair.Definition()
		w := newWorld()
		w.runs = []wf.RunResult{{Exit: 1}, {Exit: 1}, {Exit: 1}, {Exit: 1}}
		w.set("shell", "run", testRun, wf.RunResult{Exit: 1, Stderr: "boom"})
		w.set("shell", "run", vetRun, wf.RunResult{})
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		for i := 0; i < 3; i++ {
			in := h.next(run)[0]
			if _, err := h.e.Fail(run, in.Token, "cannot"); err != nil {
				t.Fatal(err)
			}
		}
		h.next(run)
		statusOf(t, h.status(run), wf.RunEnded, "GaveUp")
	})
}

func TestBroadenOnEngine(t *testing.T) {
	unsure := `{"Text":"a","Confidence":"unsure"}`
	sure := `{"Text":"a","Confidence":"confident"}`
	for name, tc := range map[string]struct {
		answers  map[string][]string
		terminal string
	}{
		"confident at once":           {map[string][]string{"Answer": {sure}}, "Accepted"},
		"broadened answer is sure":    {map[string][]string{"Answer": {unsure}, "AnswerWider": {sure}}, "Accepted"},
		"still unsure after widening": {map[string][]string{"Answer": {unsure}, "AnswerWider": {unsure}}, "Escalated"},
		"first answer dead twice":     {map[string][]string{"Answer": {`"x"`, `"y"`}}, "Escalated"},
	} {
		t.Run(name, func(t *testing.T) {
			d := broaden.Definition()
			h := newHarness(t, d, nil)
			run := h.run(d, map[string]string{"wider_context": "the whole repo"})
			statusOf(t, h.drive(run, tc.answers), wf.RunEnded, tc.terminal)
		})
	}
	d := broaden.Definition()
	h := newHarness(t, d, nil)
	run := h.run(d, map[string]string{"wider_context": "the whole repo"})
	h.submit(run, h.next(run)[0], unsure)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].State != "AnswerWider" || !strings.Contains(ins[0].Prompt, "the whole repo") || strings.Contains(ins[0].Prompt, "{{") {
		t.Fatalf("broadened instruction = %+v", ins)
	}
}

func TestEscalationOnEngine(t *testing.T) {
	fixed := `{"Status":"fixed","Patch":"p"}`
	stuck := `{"Status":"stuck"}`
	red := `{"Status":"fixed","Patch":"p","Tests":["TestA"]}`
	for name, tc := range map[string]struct {
		answers  map[string][]string
		terminal string
	}{
		"cheap fixes":             {map[string][]string{"Cheap": {fixed}}, "Done"},
		"cheap stuck":             {map[string][]string{"Cheap": {stuck}, "Strong": {fixed}}, "Done"},
		"cheap checks fail twice": {map[string][]string{"Cheap": {red, red}, "Strong": {fixed}}, "Done"},
		"strong stuck":            {map[string][]string{"Cheap": {stuck}, "Strong": {stuck}}, "GaveUp"},
	} {
		t.Run(name, func(t *testing.T) {
			d := escalation.Definition()
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			statusOf(t, h.drive(run, tc.answers), wf.RunEnded, tc.terminal)
		})
	}
	d := escalation.Definition()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], stuck)
	if ins := h.next(run); len(ins) != 1 || ins[0].State != "Strong" || !strings.Contains(ins[0].Prompt, `"Status":"stuck"`) {
		t.Fatalf("the strong tier must receive the cheap attempt: %+v", ins)
	}
}

func TestTypedexitOnEngine(t *testing.T) {
	step := func(exit string) string { return `{"Exit":"` + exit + `","Summary":"s"}` }
	many := func(n int, s string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	for name, tc := range map[string]struct {
		answers  []string
		terminal string
	}{
		"submitted":                {[]string{step("submitted")}, "Done"},
		"continue then submitted":  {[]string{step("continue"), step("continue"), step("submitted")}, "Done"},
		"limits exceeded":          {[]string{step("limits_exceeded")}, "LimitsStop"},
		"format error repeated":    {[]string{step("format_error_repeated")}, "FormatStop"},
		"blocked":                  {[]string{step("blocked")}, "BlockedStop"},
		"eight steps then the cap": {many(8, step("continue")), "LimitsStop"},
		"no valid exit twice":      {[]string{`"nope"`, `"nope again"`}, "FormatStop"},
	} {
		t.Run(name, func(t *testing.T) {
			d := typedexit.Definition()
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			// drive fails if the agent is asked more often than answered: the
			// ninth visit is cut off before the step runs.
			statusOf(t, h.drive(run, map[string][]string{"Work": tc.answers}), wf.RunEnded, tc.terminal)
		})
	}
}

func TestNoprogressOnEngine(t *testing.T) {
	good := `{"Summary":"s","FailingIDs":[]}`
	for name, tc := range map[string]struct {
		answers  []string
		terminal string
		outcome  wf.Outcome
	}{
		"fixed":                         {[]string{good}, "Fixed", wf.Success},
		"same failure twice is stalled": {[]string{`"bad"`, `"bad"`}, "StalledAt", wf.Stalled},
		"five different failures":       {[]string{`"a"`, `"b"`, `"c"`, `"d"`, `"e"`}, "GaveUp", wf.Stopped},
		"progress resets the stall":     {[]string{`"a"`, `"a"`, `"b"`}, "StalledAt", wf.Stalled},
	} {
		t.Run(name, func(t *testing.T) {
			d := noprogress.Definition()
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			st := h.drive(run, map[string][]string{"Fix": tc.answers})
			statusOf(t, st, wf.RunEnded, tc.terminal)
			if st.Outcome != tc.outcome {
				t.Fatalf("outcome = %s, want %s", st.Outcome, tc.outcome)
			}
		})
	}
}

func TestCritiqueOnEngine(t *testing.T) {
	draft := `{"Body":"text"}`
	revise := `{"Verdict":"revise","Notes":"n"}`
	approve := `{"Verdict":"approve"}`

	t.Run("approved", func(t *testing.T) {
		d := critique.Definition()
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		st := h.drive(run, map[string][]string{"Draft": {draft, draft}, "Critique": {revise, approve}})
		statusOf(t, st, wf.RunEnded, "Accepted")
		if st.Instances[0].Counters["rounds"] != 1 {
			t.Fatalf("rounds = %d, want 1", st.Instances[0].Counters["rounds"])
		}
	})

	// The rest reaches the human gate.
	gated := func(t *testing.T, answers map[string][]string, decision string, terminal string) {
		t.Helper()
		d := critique.Definition()
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		queue := map[string][]string{}
		for k, v := range answers {
			queue[k] = append([]string(nil), v...)
		}
		for i := 0; i < 100; i++ {
			ins := h.next(run)
			if len(ins) == 0 {
				if h.stopExhausted(run) {
					continue
				}
				break
			}
			in := ins[0]
			if in.Kind == wf.InstructionHumanGate {
				if err := h.e.Answer(run, in.State, decision); err != nil {
					t.Fatal(err)
				}
				continue
			}
			q := queue[in.State]
			if len(q) == 0 {
				t.Fatalf("no answer left for %s", in.State)
			}
			queue[in.State] = q[1:]
			h.submit(run, in, q[0])
		}
		statusOf(t, h.status(run), wf.RunEnded, terminal)
	}
	t.Run("round cap reaches the human, who accepts", func(t *testing.T) {
		gated(t, map[string][]string{
			"Draft":    {draft, draft, draft, draft, draft, draft},
			"Critique": {revise, revise, revise, revise, revise},
		}, "accept", "Accepted")
	})
	t.Run("round cap reaches the human, who drops", func(t *testing.T) {
		gated(t, map[string][]string{
			"Draft":    {draft, draft, draft, draft, draft, draft},
			"Critique": {revise, revise, revise, revise, revise},
		}, "drop", "Dropped")
	})
	t.Run("stalled drafter reaches the human", func(t *testing.T) {
		gated(t, map[string][]string{"Draft": {`"x"`, `"x"`}}, "accept", "Accepted")
	})
}

// ---------------------------------------------------------------- commands

type (
	cRun   struct{}
	cHalf  struct{}
	cOK    struct{}
	cFail  struct{}
	cOther struct{}
)

type cExit int

// commandDef: Run {topic} -> ok on exit 0, fail on 1, other otherwise.
func commandDef(retry func(*wf.Command[cRun], *wf.Terminal[cFail]) wf.RetryPolicy[cRun]) (*wf.Def, *wf.Command[cRun]) {
	d := wf.New("command")
	d.Param("topic")
	exits := wf.NewEnum(d, "c-exit", cExit(0), cExit(1))
	cmd := wf.NewCommand[cRun](d, "make", "{topic}")
	ok := wf.NewTerminal[cOK](d, wf.Success)
	fail := wf.NewTerminal[cFail](d, wf.Stopped)
	cmd.RouteCases(wf.Cases[cRun, wf.RunResult](wf.ExitAs[cExit], exits,
		wf.On(cExit(0), cmd.To(ok, "passes")),
		wf.On(cExit(1), cmd.To(fail, "fails"))))
	if retry != nil {
		cmd.Retry(retry(cmd, fail))
	}
	return d, cmd
}

func TestCommandRunsBoundArgvAndRoutesOnExit(t *testing.T) {
	for exit, terminal := range map[int]string{0: "cOK", 1: "cFail"} {
		d, _ := commandDef(nil)
		w := newWorld()
		w.runs = []wf.RunResult{{Exit: exit, Stdout: "out"}}
		h := newHarness(t, d, w)
		run := h.run(d, map[string]string{"topic": "build"})
		h.next(run)
		statusOf(t, h.status(run), wf.RunEnded, terminal)
		if len(w.applied) != 1 || w.applied[0].Adapter != "shell" || w.applied[0].Op != "run" {
			t.Fatalf("applied = %+v", w.applied)
		}
		argv, _ := w.applied[0].Args["argv"].([]any)
		if len(argv) != 2 || argv[0] != "make" || argv[1] != "build" {
			t.Fatalf("argv = %v, want the bound [make build]", w.applied[0].Args["argv"])
		}
		res := h.kind(run, wf.JournalEffectResult)
		var rd wf.EffectResultData
		_ = json.Unmarshal(res[0].Data, &rd)
		if !rd.OK || rd.Outcome != "applied" || !strings.Contains(string(rd.Result), `"Stdout":"out"`) {
			t.Fatalf("effect_result = %+v: a non-zero exit is data, not a failure", rd)
		}
	}
	// An exit code no case covers is a Blocked step.
	d, _ := commandDef(nil)
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 7}}
	h := newHarness(t, d, w)
	run := h.run(d, map[string]string{"topic": "x"})
	h.next(run)
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %+v, want blocked", st)
	}
}

func TestCommandApplyErrors(t *testing.T) {
	// No Retry: a failed apply is Blocked.
	d, _ := commandDef(nil)
	w := newWorld()
	w.applyErr = errors.New("exec: permission denied")
	h := newHarness(t, d, w)
	run := h.run(d, map[string]string{"topic": "x"})
	h.next(run)
	if st := h.status(run); st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "blocked") {
		t.Fatalf("status = %+v, want blocked", st)
	}

	// With Retry(2): the second failed attempt exhausts the budget; a spent
	// attempt is re-run.
	d, _ = commandDef(func(c *wf.Command[cRun], fail *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
		return wf.Retry[cRun](wf.Const(2), c.To(fail, "gave up"))
	})
	w = newWorld()
	w.applyErr = errors.New("exec: permission denied")
	h = newHarness(t, d, w)
	run = h.run(d, map[string]string{"topic": "x"})
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "cFail")
	if w.appliedCount() != 2 {
		t.Fatalf("attempts = %d, want the 2 of the budget", w.appliedCount())
	}

	// Unavailable takes the class route without spending the budget.
	d, _ = commandDef(func(c *wf.Command[cRun], fail *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
		return wf.Retry[cRun](wf.Const(5), c.To(fail, "gave up")).OnClass(wf.Unavailable, c.To(fail, "tool missing"))
	})
	w = newWorld()
	w.applyErr = wf.ErrUnavailable
	h = newHarness(t, d, w)
	run = h.run(d, map[string]string{"topic": "x"})
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "cFail")
	if w.appliedCount() != 1 {
		t.Fatalf("attempts = %d, want 1: the class route is not a retry", w.appliedCount())
	}
	rd := h.kind(run, wf.JournalEffectResult)
	var res wf.EffectResultData
	_ = json.Unmarshal(rd[0].Data, &res)
	if res.OK || res.Outcome != "unavailable" {
		t.Fatalf("effect_result = %+v, want unavailable", res)
	}
}

// doneWhenCommand: Run with DoneWhen(x.done), exit 0 -> cOK, and an optional
// OnClass(Uncertain) route to cFail.
func doneWhenCommand(uncertainRoute bool) (*wf.Def, wf.Observation[bool]) {
	d, cmd := commandDef(nil)
	obs := wf.Observe[bool]("x", "done", nil)
	wf.DoneWhen(cmd, obs, "the target exists")
	return d, obs
}

func TestCommandCrashRechecksDoneWhen(t *testing.T) {
	setup := func(t *testing.T, met bool) (*crashHarness, wf.RunID) {
		d, _ := doneWhenCommand(false)
		w := newWorld()
		w.set("x", "done", nil, false)
		c := newCrashHarness(t, d, w, wf.JournalEffectResult)
		run := c.run(d, map[string]string{"topic": "t"})
		c.cj.armed = true
		if _, err := c.e.Next(run, ""); !errors.Is(err, errCrash) {
			t.Fatalf("Next error = %v, want the crash", err)
		}
		c.resume()
		w.set("x", "done", nil, met)
		return c, run
	}

	t.Run("postcondition met completes by observation", func(t *testing.T) {
		c, run := setup(t, true)
		if ins := c.next(run); len(ins) != 0 {
			t.Fatalf("instructions = %+v, want none", ins)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cOK")
		if len(c.kind(run, wf.JournalCompletedByObservation)) != 1 || c.w.appliedCount() != 1 {
			t.Fatalf("kinds = %v, applied = %d: the command must not run again", kinds(c.records(run)), c.w.appliedCount())
		}
	})

	t.Run("postcondition unmet is uncertain and held", func(t *testing.T) {
		c, run := setup(t, false)
		before := len(c.records(run))
		ins := c.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || ins[0].Effect == 0 {
			t.Fatalf("instructions = %+v, want one manual verification", ins)
		}
		if again := c.next(run); len(again) != 1 || len(c.records(run)) != before {
			t.Fatalf("a held Uncertain effect must append nothing: %d records after, %d before", len(c.records(run)), before)
		}
		if c.w.appliedCount() != 1 {
			t.Fatalf("applied = %d, want 1: never re-applied", c.w.appliedCount())
		}
		if err := settleRunAs(c.e, run, ins[0].Effect, wf.RunResult{}); err != nil {
			t.Fatal(err)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cOK") // settled applied: exit 0
		if len(c.kind(run, wf.JournalEffectSettled)) != 1 || c.w.appliedCount() != 1 {
			t.Fatalf("kinds = %v, applied = %d", kinds(c.records(run)), c.w.appliedCount())
		}
	})

	t.Run("settled not applied is blocked", func(t *testing.T) {
		c, run := setup(t, false)
		ins := c.next(run)
		if err := settleAs(c.e, run, ins[0].Effect, false); err != nil {
			t.Fatal(err)
		}
		if st := c.status(run); st.Phase != wf.RunBlocked {
			t.Fatalf("status = %+v, want blocked", st)
		}
		if c.w.appliedCount() != 1 {
			t.Fatalf("applied = %d, want 1", c.w.appliedCount())
		}
	})

	t.Run("OnClass(Uncertain) is taken", func(t *testing.T) {
		d, cmd := commandDef(func(c *wf.Command[cRun], fail *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
			return wf.Retry[cRun](wf.Const(2), c.To(fail, "gave up")).OnClass(wf.Uncertain, c.To(fail, "uncertain"))
		})
		wf.DoneWhen(cmd, wf.Observe[bool]("x", "done", nil), "the target exists")
		w := newWorld()
		w.set("x", "done", nil, false)
		c := newCrashHarness(t, d, w, wf.JournalEffectResult)
		run := c.run(d, map[string]string{"topic": "t"})
		c.cj.armed = true
		_, _ = c.e.Next(run, "")
		c.resume()
		if ins := c.next(run); len(ins) != 0 {
			t.Fatalf("instructions = %+v, want none: the declared route is taken", ins)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cFail")
		if c.w.appliedCount() != 1 {
			t.Fatalf("applied = %d, want 1", c.w.appliedCount())
		}
	})
}

// crashedCommand starts commandDef with mod applied to its command, lets the
// process die after the command's intent (the result is never journaled) and
// resumes on a fresh Engine.
func crashedCommand(t *testing.T, w *world, retry func(*wf.Command[cRun], *wf.Terminal[cFail]) wf.RetryPolicy[cRun], mod func(*wf.Command[cRun])) (*crashHarness, wf.RunID) {
	t.Helper()
	d, cmd := commandDef(retry)
	if mod != nil {
		mod(cmd)
	}
	c := newCrashHarness(t, d, w, wf.JournalEffectResult)
	run := c.run(d, map[string]string{"topic": "t1"})
	c.cj.armed = true
	if _, err := c.e.Next(run, ""); !errors.Is(err, errCrash) {
		t.Fatalf("Next error = %v, want the crash", err)
	}
	c.resume()
	return c, run
}

func keyedBuild(c *wf.Command[cRun]) { c.Key("build", "{topic}") }

func TestKeyedCommandRecoversThroughCheck(t *testing.T) {
	t.Run("done continues with exit 0 and no re-run", func(t *testing.T) {
		w := newWorld()
		w.status["build/t1"] = wf.EffectDone
		c, run := crashedCommand(t, w, nil, keyedBuild)
		if ins := c.next(run); len(ins) != 0 {
			t.Fatalf("instructions = %+v, want none: no manual verification", ins)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cOK") // exit 0 routes "passes"
		if w.appliedCount() != 1 || w.checks != 1 {
			t.Fatalf("applied = %d, checks = %d, want 1 and 1", w.appliedCount(), w.checks)
		}
		var rd wf.EffectResultData
		res := c.kind(run, wf.JournalEffectResult)
		_ = json.Unmarshal(res[0].Data, &rd)
		if !rd.OK || rd.Outcome != "done" || !strings.Contains(string(rd.Result), `"Exit":0`) {
			t.Fatalf("effect_result = %+v, want a done exit-0 result", rd)
		}
	})
	t.Run("not done re-applies once", func(t *testing.T) {
		w := newWorld()
		w.status["build/t1"] = wf.EffectNotDone
		c, run := crashedCommand(t, w, nil, keyedBuild)
		w.runs = []wf.RunResult{{Exit: 1}} // the re-run's result
		if ins := c.next(run); len(ins) != 0 {
			t.Fatalf("instructions = %+v, want none", ins)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cFail") // the re-run's exit 1
		if w.appliedCount() != 2 {
			t.Fatalf("applied = %d, want the crashed run and one re-run", w.appliedCount())
		}
		if got := w.applied[1].KeyID(); got != "build/t1" {
			t.Fatalf("re-applied key = %q, want the bound build/t1", got)
		}
	})
	t.Run("unknown is manual verification", func(t *testing.T) {
		w := newWorld()
		c, run := crashedCommand(t, w, nil, keyedBuild)
		if ins := c.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || ins[0].Transient {
			t.Fatalf("instructions = %+v, want one non-transient manual verification", ins)
		}
		if w.appliedCount() != 1 {
			t.Fatalf("applied = %d, want 1", w.appliedCount())
		}
	})
}

func TestIdempotentCommandReruns(t *testing.T) {
	w := newWorld()
	c, run := crashedCommand(t, w, nil, func(c *wf.Command[cRun]) { keyedBuild(c); c.Idempotent() })
	w.runs = []wf.RunResult{{Exit: 1}}
	if ins := c.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none", ins)
	}
	statusOf(t, c.status(run), wf.RunEnded, "cFail")
	if w.appliedCount() != 2 || w.checks != 0 {
		t.Fatalf("applied = %d, checks = %d, want one more apply and no Check", w.appliedCount(), w.checks)
	}
}

func TestSettleWithExitCode(t *testing.T) {
	w := newWorld()
	c, run := crashedCommand(t, w, nil, nil)
	in := c.next(run)[0]
	if err := settleAs(c.e, run, in.Effect, true); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "--exit") {
		t.Fatalf("Settle(applied) on a command = %v, want ErrRejected naming --exit", err)
	}
	if err := settleRunAs(c.e, run, in.Effect, wf.RunResult{Exit: 1, Stdout: "x"}); err != nil {
		t.Fatal(err)
	}
	statusOf(t, c.status(run), wf.RunEnded, "cFail") // exit 1 routes "fails", not the exit-0 default
	var sd wf.EffectSettledData
	_ = json.Unmarshal(c.kind(run, wf.JournalEffectSettled)[0].Data, &sd)
	if sd.Result == nil || sd.Result.Exit != 1 || sd.Result.Stdout != "x" {
		t.Fatalf("effect_settled = %+v, want the observed result", sd)
	}
	if w.appliedCount() != 1 {
		t.Fatalf("applied = %d, want 1", w.appliedCount())
	}
}

// G4: a not-applied keyed command is re-issued once with nothing spent, with
// or without a retry policy; the next not-applied settlement, or an unkeyed
// command's first, is Blocked: it takes OnClass(Blocked), else the exhaustion
// halt answered by retry (and stop when the state has a fallback edge).
func TestSettleNotAppliedReissuesKeyed(t *testing.T) {
	retryOf := func(n int) func(*wf.Command[cRun], *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
		return func(c *wf.Command[cRun], fail *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
			return wf.Retry[cRun](wf.Const(n), c.To(fail, "gave up"))
		}
	}
	blockedRoute := func(c *wf.Command[cRun], fail *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
		return wf.Retry[cRun](wf.Const(1), c.To(fail, "gave up")).OnClass(wf.Blocked, c.To(fail, "not applied"))
	}
	settled := func(c *crashHarness, run wf.RunID, i int) wf.EffectSettledData {
		var sd wf.EffectSettledData
		_ = json.Unmarshal(c.kind(run, wf.JournalEffectSettled)[i].Data, &sd)
		return sd
	}
	// settleCrashing settles the open effect not applied and lets the process
	// die on the re-issued command's result, so a second verification is due.
	settleCrashing := func(t *testing.T, c *crashHarness, run wf.RunID) wf.Instruction {
		t.Helper()
		c.e = wf.NewEngine(c.cj, c.reg, wf.EngineOptions{Defs: []*wf.Def{c.d}, Clock: func() time.Time { return c.now }})
		c.cj.armed = true
		if err := settleAs(c.e, run, c.next(run)[0].Effect, false); !errors.Is(err, errCrash) {
			t.Fatalf("Settle error = %v, want the re-issued command to crash", err)
		}
		c.resume()
		ins := c.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification {
			t.Fatalf("instructions = %+v, want the re-issue's manual verification", ins)
		}
		return ins[0]
	}
	for name, retry := range map[string]func(*wf.Command[cRun], *wf.Terminal[cFail]) wf.RetryPolicy[cRun]{"no retry policy": nil, "a retry policy": retryOf(2)} {
		t.Run("keyed is re-issued once with nothing spent, "+name, func(t *testing.T) {
			w := newWorld()
			c, run := crashedCommand(t, w, retry, keyedBuild)
			first := c.next(run)[0]
			if err := settleAs(c.e, run, first.Effect, false); err != nil {
				t.Fatal(err)
			}
			if ins := c.next(run); len(ins) != 0 {
				t.Fatalf("instructions = %+v, want none: the reissued command ran", ins)
			}
			statusOf(t, c.status(run), wf.RunEnded, "cOK")
			if w.appliedCount() != 2 {
				t.Fatalf("applied = %d, want the crashed run and the reissue", w.appliedCount())
			}
			intents := c.kind(run, wf.JournalEffectIntent)
			if len(intents) != 2 || intents[1].Seq == first.Effect || intents[1].Attempt != 2 {
				t.Fatalf("intents = %+v, want a fresh intent of attempt 2", intents)
			}
			if sd := settled(c, run, 0); !sd.Reissued || sd.Refused == nil || sd.Refused.Spent || sd.Refused.Halt || sd.Refused.Plan != nil {
				t.Fatalf("effect_settled = %+v, want a free re-issue: nothing spent, no halt, no route", sd)
			}
		})
	}
	t.Run("the second not-applied takes OnClass(Blocked)", func(t *testing.T) {
		w := newWorld()
		c, run := crashedCommand(t, w, blockedRoute, keyedBuild)
		second := settleCrashing(t, c, run)
		if err := settleAs(c.e, run, second.Effect, false); err != nil {
			t.Fatal(err)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cFail")
		if sd := settled(c, run, 1); sd.Reissued || sd.Refused == nil || sd.Refused.Plan == nil || sd.Refused.Spent {
			t.Fatalf("effect_settled = %+v, want the Blocked route with nothing spent", sd)
		}
		if w.appliedCount() != 2 {
			t.Fatalf("applied = %d, want the crashed run and one reissue", w.appliedCount())
		}
	})
	t.Run("the second not-applied with no Blocked route takes the exhausted edge", func(t *testing.T) {
		w := newWorld()
		c, run := crashedCommand(t, w, retryOf(2), keyedBuild)
		second := settleCrashing(t, c, run)
		if err := settleAs(c.e, run, second.Effect, false); err != nil {
			t.Fatal(err)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cFail")
		if w.appliedCount() != 2 {
			t.Fatalf("applied = %d, want no third run", w.appliedCount())
		}
	})
	t.Run("no retry policy halts plainly, and retry runs it again", func(t *testing.T) {
		w := newWorld()
		c, run := crashedCommand(t, w, nil, keyedBuild)
		second := settleCrashing(t, c, run)
		if err := settleAs(c.e, run, second.Effect, false); err != nil {
			t.Fatal(err)
		}
		if st := c.status(run); st.Phase != wf.RunBlocked {
			t.Fatalf("status = %s, want blocked: a state with no route and no retry policy halts", st.Phase)
		}
		if err := c.e.Retry(run, "", true); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("Retry --stop = %v, want rejected: not an exhaustion halt", err)
		}
		if err := c.e.Retry(run, "", false); err != nil {
			t.Fatal(err)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cOK")
		if w.appliedCount() != 3 {
			t.Fatalf("applied = %d, want the retry to run the command a third time", w.appliedCount())
		}
	})
	t.Run("unkeyed is blocked at once", func(t *testing.T) {
		w := newWorld()
		c, run := crashedCommand(t, w, retryOf(2), nil)
		if err := settleAs(c.e, run, c.next(run)[0].Effect, false); err != nil {
			t.Fatal(err)
		}
		statusOf(t, c.status(run), wf.RunEnded, "cFail")
		if sd := settled(c, run, 0); sd.Reissued || sd.Refused == nil || sd.Refused.Halt || sd.Refused.Plan == nil || w.appliedCount() != 1 {
			t.Fatalf("effect_settled = %+v applied %d, want the exhausted edge and no reissue", sd, w.appliedCount())
		}
	})
}

func TestTransientCheckRetried(t *testing.T) {
	w := newWorld()
	w.status["build/t1"] = wf.EffectDone
	c, run := crashedCommand(t, w, nil, keyedBuild)
	w.checkErr = []error{errors.New("tool down")}
	before := len(c.records(run))
	ins := c.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || !ins[0].Transient || !strings.Contains(ins[0].Prompt, "Check failed transiently: tool down") {
		t.Fatalf("instructions = %+v, want one transient manual verification naming the error", ins)
	}
	if len(c.records(run)) != before {
		t.Fatalf("a transient Check error journaled %d records", len(c.records(run))-before)
	}
	if ins := c.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none once the Check answers done", ins)
	}
	statusOf(t, c.status(run), wf.RunEnded, "cOK")
	if w.checks != 2 || w.appliedCount() != 1 {
		t.Fatalf("checks = %d, applied = %d, want 2 and 1", w.checks, w.appliedCount())
	}
}

func TestTransientCheckSkipsUncertainRoute(t *testing.T) {
	retry := func(c *wf.Command[cRun], fail *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
		return wf.Retry[cRun](wf.Const(2), c.To(fail, "gave up")).OnClass(wf.Uncertain, c.To(fail, "uncertain"))
	}
	w := newWorld()
	c, run := crashedCommand(t, w, retry, keyedBuild)
	w.checkErr = []error{errors.New("tool down")}
	if ins := c.next(run); len(ins) != 1 || !ins[0].Transient {
		t.Fatalf("instructions = %+v, want a transient manual verification, not the Uncertain route", ins)
	}
	if st := c.status(run); st.Phase == wf.RunEnded {
		t.Fatalf("status = %+v, a transient error must not take OnClass(Uncertain)", st)
	}
}

func TestSettleCLIExit(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("cmd-cli")
	if code, _, _ := e.proc("", []string{"WF_TEST_CRASH=1"}, "next", string(run), "all", "--state-dir", e.state); code != 3 {
		t.Fatalf("crashing next exited %d, want the crash code 3", code)
	}
	effect := itoa(e.next(run)[0].Effect)
	settle := func(extra ...string) (int, string) {
		code, _, er := e.cli(append([]string{"settle", string(run), "--state-dir", e.state, "--effect", effect, "--evidence", "out.txt checked"}, extra...)...)
		return code, er
	}
	// an earlier revision review round 1: a settlement carries what was checked.
	if code, _, er := e.cli("settle", string(run), "--state-dir", e.state, "--effect", effect, "--not-applied"); code != 2 || !strings.Contains(er, "--evidence") {
		t.Fatalf("settle without --evidence = %d %q, want a usage error naming --evidence", code, er)
	}
	if code, _, er := e.cli("settle", string(run), "--state-dir", e.state, "--effect", effect, "--not-applied", "--evidence", "  "); code != 2 || !strings.Contains(er, "--evidence") {
		t.Fatalf("settle with blank --evidence = %d %q, want a usage error naming --evidence", code, er)
	}
	if code, er := settle("--applied"); code != 4 || !strings.Contains(er, "--exit") {
		t.Fatalf("settle --applied on a command = %d %q, want 4 naming --exit", code, er)
	}
	if code, _ := settle("--not-applied", "--exit", "0"); code != 2 {
		t.Fatalf("settle --not-applied --exit = %d, want 2", code)
	}
	if code, _ := settle("--applied", "--stdout-file", "f"); code != 2 {
		t.Fatalf("settle --stdout-file without --exit = %d, want 2", code)
	}
	out := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(out, []byte("seen"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, er := settle("--applied", "--exit", "0", "--stdout-file", out); code != 0 {
		t.Fatalf("settle --applied --exit 0 = %d %q, want 0", code, er)
	}
	_, journal, _ := e.cli("journal", string(run), "--state-dir", e.state)
	if !strings.Contains(journal, `"Stdout":"seen"`) {
		t.Fatalf("journal lacks the settled stdout:\n%s", journal)
	}
	if st := e.status(run); st.Phase != wf.RunEnded {
		t.Fatalf("status = %+v, want ended", st)
	}
}

// ---------------------------------------------------------------- edge effects

// effectDef: A submits and takes an edge carrying the given effects to Done.
// Counter "n" is incremented on that edge. retry may add class routes.
func effectDef(retry func(*wf.Agent[eA, eOut], *wf.Terminal[eBad]) wf.RetryPolicy[eA], effects ...wf.Effect) (*wf.Def, *wf.Agent[eA, eOut]) {
	d := wf.New("effects")
	n := d.Counter("n")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	pol := wf.Retry[eA](wf.Const(2), a.To(bad, "gave up"))
	if retry != nil {
		pol = retry(a, bad)
	}
	a.Spec(wf.PromptText("p"), pol)
	next := a.To(done, "done").With(n.Inc())
	for _, e := range effects {
		next = next.With(e)
	}
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	return d, a
}

func mergeEffect(key wf.Key) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "git", Op: "merge", Args: map[string]any{"branch": "feat"}, Key: key}
}

func TestCrashIntentCheckedByKey(t *testing.T) {
	d, _ := effectDef(nil, mergeEffect(wf.KeyOf("merge", "feat")))

	crashed := func(t *testing.T, status wf.EffectStatus) (*crashHarness, wf.RunID) {
		w := newWorld()
		w.status["merge/feat"] = status
		c := newCrashHarness(t, d, w, wf.JournalEffectResult)
		run := c.run(d, nil)
		c.crashAt(run, `{}`)
		if c.w.appliedCount() != 1 {
			t.Fatalf("applied = %d before the resume, want the one pre-crash apply", c.w.appliedCount())
		}
		return c, run
	}

	t.Run("done is not re-applied", func(t *testing.T) {
		c, run := crashed(t, wf.EffectDone)
		c.next(run)
		statusOf(t, c.status(run), wf.RunEnded, "eDone")
		if c.w.appliedCount() != 1 {
			t.Fatalf("applied = %d, want 1: done by Key means not re-applied", c.w.appliedCount())
		}
		var rd wf.EffectResultData
		_ = json.Unmarshal(c.kind(run, wf.JournalEffectResult)[0].Data, &rd)
		if !rd.OK || rd.Outcome != "done" {
			t.Fatalf("effect_result = %+v, want outcome done", rd)
		}
		if got := c.status(run).Instances[0].Counters["n"]; got != 1 {
			t.Fatalf("counter n = %d, want 1", got)
		}
	})

	t.Run("not done is applied", func(t *testing.T) {
		c, run := crashed(t, wf.EffectNotDone)
		c.next(run)
		statusOf(t, c.status(run), wf.RunEnded, "eDone")
		if c.w.appliedCount() != 2 {
			t.Fatalf("applied = %d, want the apply and its one re-apply", c.w.appliedCount())
		}
	})
}

func TestNonIdempotentYieldsManualVerification(t *testing.T) {
	d, _ := effectDef(nil, mergeEffect(wf.NonIdempotent("pushes upstream")))
	w := newWorld()
	c := newCrashHarness(t, d, w, wf.JournalEffectResult)
	run := c.run(d, nil)
	c.crashAt(run, `{}`)

	intent := c.kind(run, wf.JournalEffectIntent)[0].Seq
	ins := c.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions = %+v, want one", ins)
	}
	in := ins[0]
	if in.Kind != wf.InstructionManualVerification || in.Effect != intent {
		t.Fatalf("instruction = %+v, want manual verification of effect %d", in, intent)
	}
	if want := "workflow settle " + string(run) + " --effect "; !strings.HasPrefix(in.Submit, want) {
		t.Fatalf("Submit = %q, want the settle command %q...", in.Submit, want)
	}
	for _, want := range []string{"git", "merge", "feat", "pushes upstream"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	if w.appliedCount() != 1 || w.checks != 0 {
		t.Fatalf("applied = %d, checks = %d: a NonIdempotent effect is neither re-applied nor checked", w.appliedCount(), w.checks)
	}
}

func TestUnknownCheckIsUncertainAndNeverReapplied(t *testing.T) {
	d, _ := effectDef(nil, mergeEffect(wf.KeyOf("merge", "feat")))
	w := newWorld() // no scripted status: the check answers unknown
	c := newCrashHarness(t, d, w, wf.JournalEffectResult)
	run := c.run(d, nil)
	c.crashAt(run, `{}`)
	before := len(c.records(run))
	for i := 0; i < 3; i++ {
		ins := c.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification {
			t.Fatalf("call %d: instructions = %+v, want a manual verification", i, ins)
		}
	}
	if w.appliedCount() != 1 || len(c.records(run)) != before || w.checks == 0 {
		t.Fatalf("applied = %d, records %d -> %d, checks = %d: an unknown check holds the effect without re-applying", w.appliedCount(), before, len(c.records(run)), w.checks)
	}
	if st := c.status(run); st.Phase == wf.RunEnded {
		t.Fatalf("status = %+v: the run must wait for the settlement", st)
	}
}

func TestUncertainTakesDeclaredRoute(t *testing.T) {
	d, _ := effectDef(func(a *wf.Agent[eA, eOut], bad *wf.Terminal[eBad]) wf.RetryPolicy[eA] {
		return wf.Retry[eA](wf.Const(2), a.To(bad, "gave up")).OnClass(wf.Uncertain, a.To(bad, "uncertain effect"))
	}, mergeEffect(wf.NonIdempotent("pushes")))
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	run := c.run(d, nil)
	c.crashAt(run, `{}`)
	if ins := c.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none", ins)
	}
	statusOf(t, c.status(run), wf.RunEnded, "eBad")
	if c.w.appliedCount() != 1 {
		t.Fatalf("applied = %d, want 1", c.w.appliedCount())
	}
	refused := c.kind(run, wf.JournalRefused)
	if len(refused) != 1 || refused[0].Class != wf.Uncertain {
		t.Fatalf("refused = %+v, want one Uncertain record", refused)
	}
}

func TestUnknownEffectTakesDefinitionDefaultWithoutRetry(t *testing.T) {
	d := wf.New("default-uncertain")
	work := wf.NewCompute[eC, eOut](d, func(wf.Ctx) (eOut, error) { return eOut{}, nil })
	done, bad := twoTerminals(d)
	d.OnFailure(wf.Uncertain, bad)
	next := work.To(done, "done").With(mergeEffect(wf.KeyOf("merge", "feat")))
	work.Route(func(eOut, wf.Ctx) wf.Edge[eC] { return next }, next)
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	run := c.run(d, nil)
	c.cj.armed = true
	if _, err := c.e.Next(run, ""); !errors.Is(err, errCrash) {
		t.Fatalf("Next error = %v, want the crash", err)
	}
	c.resume()
	if ins := c.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want the default route instead of manual verification", ins)
	}
	statusOf(t, c.status(run), wf.RunEnded, "eBad")
	if c.w.appliedCount() != 1 || c.w.checks == 0 {
		t.Fatalf("applied = %d, checks = %d: unknown reconciliation must route without reapplying", c.w.appliedCount(), c.w.checks)
	}
	refused := c.kind(run, wf.JournalRefused)
	if len(refused) != 1 || refused[0].Class != wf.Uncertain {
		t.Fatalf("refused = %+v, want one Uncertain record", refused)
	}
}

func TestSettleAppliedContinues(t *testing.T) {
	d, _ := effectDef(nil, mergeEffect(wf.NonIdempotent("pushes")), wf.ExternalEffect{Adapter: "git", Op: "tag", Args: map[string]any{"t": "v1"}, Key: wf.KeyOf("tag")})
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	run := c.run(d, nil)
	c.crashAt(run, `{}`)
	in := c.next(run)[0]

	if err := settleAs(c.e, run, in.Effect+99, true); err == nil {
		t.Fatal("settling an effect id with no open intent must fail")
	}
	if err := settleAs(c.e, run, in.Effect, true); err != nil {
		t.Fatal(err)
	}
	statusOf(t, c.status(run), wf.RunEnded, "eDone")
	if len(c.kind(run, wf.JournalEffectSettled)) != 1 {
		t.Fatalf("kinds = %v, want one effect_settled", kinds(c.records(run)))
	}
	// The settled effect is not re-applied; the edge's next effect is.
	if c.w.appliedCount() != 2 || c.w.applied[1].Op != "tag" {
		t.Fatalf("applied = %+v, want the merge once and then the tag", c.w.applied)
	}
	if err := settleAs(c.e, run, in.Effect, true); err == nil || !strings.Contains(err.Error(), "already settled") {
		t.Fatalf("second settle = %v, want already settled", err)
	}
}

func TestSettleNotAppliedBlocks(t *testing.T) {
	t.Run("takes the exhausted edge without a Blocked route", func(t *testing.T) {
		// C3: the declared route wins; nothing halts for a human.
		d, _ := effectDef(nil, mergeEffect(wf.NonIdempotent("pushes")))
		c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
		run := c.run(d, nil)
		c.crashAt(run, `{}`)
		if err := settleAs(c.e, run, c.next(run)[0].Effect, false); err != nil {
			t.Fatal(err)
		}
		st := c.status(run)
		statusOf(t, st, wf.RunEnded, "eBad")
		if st.Instances[0].Counters["n"] != 0 || c.w.appliedCount() != 1 {
			t.Fatalf("status = %+v applied %d, want the edge not taken and the merge applied once", st, c.w.appliedCount())
		}
	})
	t.Run("takes OnClass(Blocked)", func(t *testing.T) {
		d, _ := effectDef(func(a *wf.Agent[eA, eOut], bad *wf.Terminal[eBad]) wf.RetryPolicy[eA] {
			return wf.Retry[eA](wf.Const(2), a.To(bad, "gave up")).OnClass(wf.Blocked, a.To(bad, "not applied"))
		}, mergeEffect(wf.NonIdempotent("pushes")))
		c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
		run := c.run(d, nil)
		c.crashAt(run, `{}`)
		if err := settleAs(c.e, run, c.next(run)[0].Effect, false); err != nil {
			t.Fatal(err)
		}
		statusOf(t, c.status(run), wf.RunEnded, "eBad")
	})
}

// A1: a failed effect skips the rest of the edge and stops it: the edge's
// counters do not apply and the source takes its Blocked route.
func TestFailedApplyStopsEdge(t *testing.T) {
	first := wf.ExternalEffect{Adapter: "bd", Op: "create", Args: map[string]any{"title": "a"}, Key: wf.KeyOf("a")}
	second := wf.ExternalEffect{Adapter: "git", Op: "tag", Args: map[string]any{"t": "v1"}, Key: wf.KeyOf("b")}
	d, _ := effectDef(func(a *wf.Agent[eA, eOut], bad *wf.Terminal[eBad]) wf.RetryPolicy[eA] {
		return wf.Retry[eA](wf.Const(2), a.To(bad, "gave up")).OnClass(wf.Blocked, a.To(bad, "edge failed"))
	}, first, second)
	w := newWorld()
	w.failOp["bd.create"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"eA": {`{}`}})
	statusOf(t, st, wf.RunEnded, "eBad")
	if w.appliedCount() != 1 || w.applied[0].Op != "create" {
		t.Fatalf("applied = %+v, want only the failed first effect", w.applied)
	}
	var rd wf.EffectResultData
	_ = json.Unmarshal(h.kind(run, wf.JournalEffectResult)[0].Data, &rd)
	if rd.OK || rd.Outcome != "failed" || rd.Error == "" {
		t.Fatalf("effect_result = %+v, want failed with its error", rd)
	}
	if got := st.Instances[0].Counters["n"]; got != 0 {
		t.Fatalf("counter n = %d, want 0: the edge was not taken", got)
	}
	if r := h.kind(run, wf.JournalRefused); len(r) != 1 || r[0].Class != wf.Blocked || len(h.kind(run, wf.JournalEffectIntent)) != 1 {
		t.Fatalf("kinds = %v, want one Blocked refusal and no intent for the skipped effect", kinds(h.records(run)))
	}
}

// ---------------------------------------------------------------- migrations

type (
	mOld struct{}
	mNew struct{}
	mEnd struct{}
)

func migDef(newName bool) *wf.Def {
	d := wf.New("mig")
	if newName {
		d.Version(2)
		a := wf.NewAgent[mNew, eOut](d)
		end := wf.NewTerminal[mEnd](d, wf.Success)
		a.Spec(wf.PromptText("new"), wf.Retry[mNew](wf.Const(2), a.To(end, "x")))
		next := a.To(end, "done")
		a.Route(func(eOut, wf.Ctx) wf.Edge[mNew] { return next }, next)
		return d
	}
	a := wf.NewAgent[mOld, eOut](d)
	end := wf.NewTerminal[mEnd](d, wf.Success)
	a.Spec(wf.PromptText("old"), wf.Retry[mOld](wf.Const(2), a.To(end, "x")))
	next := a.To(end, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[mOld] { return next }, next)
	return d
}

func TestMigrationMapsOldHash(t *testing.T) {
	v1 := migDef(false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "m1"})
	run := h.run(v1, nil)
	old := h.next(run)
	if len(old) != 1 || old[0].State != "mOld" {
		t.Fatalf("v1 instruction = %+v", old)
	}

	v2 := migDef(true)
	if v1.Hash() == v2.Hash() {
		t.Fatal("test needs two different hashes")
	}
	v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: map[string]string{"mOld": "mNew"}})
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}, RunID: "m1"})
	if id, err := e2.Run(v2, nil, nil); err != nil || id != run {
		t.Fatalf("Run on the new definition = %q, %v: it resumes the old-hash run", id, err)
	}
	ins, err := e2.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].State != "mNew" || ins[0].Token != old[0].Token {
		t.Fatalf("v2 instruction = %+v, %v: the open attempt moves onto the new state", ins, err)
	}
	if _, err := e2.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(h.kind(run, wf.JournalMigrated)); n != 1 {
		t.Fatalf("migrated records = %d, want exactly 1", n)
	}
	var md wf.MigratedData
	_ = json.Unmarshal(h.kind(run, wf.JournalMigrated)[0].Data, &md)
	if md.FromHash != v1.Hash() || md.ToHash != v2.Hash() {
		t.Fatalf("migrated = %+v", md)
	}
	res, err := e2.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{}`)})
	if err != nil || res.Class != wf.Accepted {
		t.Fatalf("Submit = %+v, %v", res, err)
	}
	st, _ := e2.Status(run)
	statusOf(t, st, wf.RunEnded, "mEnd")
}

type (
	mRenamed struct{}
	mNow     struct{}
	mBad     struct{}
	mSub     struct{}
)

// migChain is a two-agent chain: its first agent (mOld, or mRenamed in v2) and
// mNow. mNow's route reads the first agent's output and ends at mEnd only when
// it sees "seen" (v1 ends regardless), else at mBad, so a missed rename shows
// as the wrong terminal. yield gives the terminals a result for use as a child.
func migChain(name string, v2, yield bool) *wf.Def {
	d := wf.New(name)
	var wire func(now *wf.Agent[mNow, eOut], bad *wf.Terminal[mBad]) func(wf.Ctx) bool
	if v2 {
		d.Version(2)
		first := wf.NewAgent[mRenamed, eOut](d) // declared first: the entry state
		wire = func(now *wf.Agent[mNow, eOut], bad *wf.Terminal[mBad]) func(wf.Ctx) bool {
			next := first.To(now, "next")
			first.Spec(wf.PromptText("old"), wf.Retry[mRenamed](wf.Const(2), first.To(bad, "x")))
			first.Route(func(eOut, wf.Ctx) wf.Edge[mRenamed] { return next }, next)
			return func(c wf.Ctx) bool { return wf.OutputOf(c, first).V == "seen" }
		}
	} else {
		first := wf.NewAgent[mOld, eOut](d)
		wire = func(now *wf.Agent[mNow, eOut], bad *wf.Terminal[mBad]) func(wf.Ctx) bool {
			next := first.To(now, "next")
			first.Spec(wf.PromptText("old"), wf.Retry[mOld](wf.Const(2), first.To(bad, "x")))
			first.Route(func(eOut, wf.Ctx) wf.Edge[mOld] { return next }, next)
			return func(wf.Ctx) bool { return true }
		}
	}
	now := wf.NewAgent[mNow, eOut](d)
	end := wf.NewTerminal[mEnd](d, wf.Success)
	bad := wf.NewTerminal[mBad](d, wf.Stopped)
	if yield {
		res := func(wf.Ctx) chRes { return chRes{} }
		wf.Yield(end, res)
		wf.Yield(bad, res)
	}
	seen := wire(now, bad)
	toEnd, toBad := now.To(end, "done"), now.To(bad, "bad")
	now.Spec(wf.PromptText("now"), wf.Retry[mNow](wf.Const(2), now.To(bad, "x")))
	now.Route(func(_ eOut, c wf.Ctx) wf.Edge[mNow] {
		if seen(c) {
			return toEnd
		}
		return toBad
	}, toEnd, toBad)
	return d
}

func TestMigrationRenamesVisitedNonCurrentState(t *testing.T) {
	v1 := migChain("mig2", false, false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "m2"})
	run := h.run(v1, nil)
	first := h.next(run)
	h.submit(run, first[0], `{"V":"seen"}`)
	at := h.next(run)
	if len(at) != 1 || at[0].State != "mNow" {
		t.Fatalf("v1 instruction = %+v, want the run sitting at mNow", at)
	}

	v2 := migChain("mig2", true, false)
	v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: map[string]string{"mOld": "mRenamed"}})
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}, RunID: "m2"})
	if _, err := e2.Run(v2, nil, nil); err != nil {
		t.Fatal(err)
	}
	ins, err := e2.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].State != "mNow" {
		t.Fatalf("v2 instruction = %+v, %v: the current state is not a rename key and stays", ins, err)
	}
	res, err := e2.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{}`)})
	if err != nil || res.Class != wf.Accepted {
		t.Fatalf("Submit = %+v, %v", res, err)
	}
	st, _ := e2.Status(run)
	statusOf(t, st, wf.RunEnded, "mEnd") // mBad if OutputOf(mRenamed) lost the v1 output
}

func TestMigrationRenamesChildInstance(t *testing.T) {
	build := func(v2 bool) *wf.Def {
		d := wf.New("mig3")
		if v2 {
			d.Version(2)
		}
		sub := wf.NewSub[mSub, chItem, chRes](d, wf.Typed[chItem, chRes](migChain("child", v2, true)), func(wf.Ctx) chItem { return chItem{} })
		fin := wf.NewTerminal[mEnd](d, wf.Success)
		stop := wf.NewTerminal[mBad](d, wf.Stopped)
		sub.Exits(wf.OnExit[mEnd](sub.To(fin, "ok")), wf.OnExit[mBad](sub.To(stop, "bad")))
		return d
	}
	v1 := build(false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "m3"})
	run := h.run(v1, nil)
	first := h.next(run)
	if len(first) != 1 || first[0].State != "child/mOld" {
		t.Fatalf("v1 instruction = %+v", first)
	}
	h.submit(run, first[0], `{"V":"seen"}`)
	at := h.next(run)
	if len(at) != 1 || at[0].State != "child/mNow" {
		t.Fatalf("v1 instruction = %+v, want the child sitting at child/mNow", at)
	}

	v2 := build(true)
	v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: map[string]string{"child/mOld": "child/mRenamed"}})
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}, RunID: "m3"})
	if _, err := e2.Run(v2, nil, nil); err != nil {
		t.Fatal(err)
	}
	ins, err := e2.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].State != "child/mNow" {
		t.Fatalf("v2 instruction = %+v, %v", ins, err)
	}
	if res, err := e2.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{}`)}); err != nil || res.Class != wf.Accepted {
		t.Fatalf("Submit = %+v, %v", res, err)
	}
	if _, err := e2.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	st, _ := e2.Status(run)
	statusOf(t, st, wf.RunEnded, "mEnd") // mBad if the child's OutputOf(mRenamed) lost the v1 output
}

func TestHashChangeWithoutMigrationErrors(t *testing.T) {
	v1 := migDef(false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "m2"})
	run := h.run(v1, nil)
	v2 := migDef(true)
	// A Migration from some other hash does not cover this run.
	v2.Migrations(wf.Migration{FromHash: "sha256:other", ToHash: v2.Hash(), States: map[string]string{"mOld": "mNew"}})
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}, RunID: "m2"})
	_, err := e2.Next(run, "")
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), v1.Hash()) || !strings.Contains(err.Error(), v2.Hash()) {
		t.Fatalf("Next = %v, want ErrDefChanged naming both hashes", err)
	}
	if n := len(h.kind(run, wf.JournalMigrated)); n != 0 {
		t.Fatalf("migrated records = %d, want none", n)
	}
}

// ---------------------------------------------------------------- model map

type (
	pOpus   struct{}
	pSonnet struct{}
	pFable  struct{}
	pPlain  struct{}
)

// modelDef chains four agents hinted opus, sonnet, fable and nothing.
func modelDef() *wf.Def {
	d := wf.New("models")
	a := wf.NewAgent[pOpus, eOut](d).Dispatch(wf.Model("opus"))
	b := wf.NewAgent[pSonnet, eOut](d).Dispatch(wf.Model("sonnet"))
	c := wf.NewAgent[pFable, eOut](d).Dispatch(wf.Model("fable"))
	p := wf.NewAgent[pPlain, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText("a"), wf.Retry[pOpus](wf.Const(2), a.To(bad, "x")))
	b.Spec(wf.PromptText("b"), wf.Retry[pSonnet](wf.Const(2), b.To(bad, "x")))
	c.Spec(wf.PromptText("c"), wf.Retry[pFable](wf.Const(2), c.To(bad, "x")))
	p.Spec(wf.PromptText("p"), wf.Retry[pPlain](wf.Const(2), p.To(bad, "x")))
	ab, bc, cp, pd := a.To(b, "n"), b.To(c, "n"), c.To(p, "n"), p.To(done, "n")
	a.Route(func(eOut, wf.Ctx) wf.Edge[pOpus] { return ab }, ab)
	b.Route(func(eOut, wf.Ctx) wf.Edge[pSonnet] { return bc }, bc)
	c.Route(func(eOut, wf.Ctx) wf.Edge[pFable] { return cp }, cp)
	p.Route(func(eOut, wf.Ctx) wf.Edge[pPlain] { return pd }, pd)
	return d
}

func TestModelMapRewritesJournaledUnhashed(t *testing.T) {
	d := modelDef()
	hashBefore, renderBefore := d.Hash(), d.Render()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{ModelMap: map[string]string{"opus": "sonnet"}})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Dispatch.Model != "sonnet" {
		t.Fatalf("instruction = %+v, want the opus hint rewritten to sonnet", ins)
	}
	var started wf.RunStartedData
	_ = json.Unmarshal(h.kind(run, wf.JournalRunStarted)[0].Data, &started)
	if started.ModelMap["opus"] != "sonnet" {
		t.Fatalf("run_started = %+v, want the model map journaled", started)
	}
	var issued wf.InstructionIssuedData
	_ = json.Unmarshal(h.kind(run, wf.JournalInstructionIssued)[0].Data, &issued)
	if issued.Dispatch.Model != "sonnet" {
		t.Fatalf("instruction_issued = %+v, want the rewritten model", issued.Dispatch)
	}
	if d.Hash() != hashBefore || d.Render() != renderBefore || !strings.Contains(d.Render(), "model=opus") {
		t.Fatal("the model map must not touch the definition: hash and render still show opus")
	}
}

func TestModelMapSurvivesFreshEngine(t *testing.T) {
	d := modelDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{ModelMap: map[string]string{"opus": "haiku"}})
	run := h.run(d, nil)
	// A process that was given no model map issues the first instruction.
	fresh := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
	ins, err := fresh.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Dispatch.Model != "haiku" {
		t.Fatalf("fresh engine instruction = %+v, %v, want the journaled map re-applied", ins, err)
	}
}

func TestModelMapSinglePass(t *testing.T) {
	d := modelDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{ModelMap: map[string]string{"opus": "sonnet", "sonnet": "haiku"}})
	run := h.run(d, nil)
	for _, want := range []struct{ state, model string }{
		{"pOpus", "sonnet"}, // opus -> sonnet, and not on to haiku
		{"pSonnet", "haiku"},
		{"pFable", "fable"}, // unmapped passes through
		{"pPlain", ""},      // no hint stays empty
	} {
		ins := h.next(run)
		if len(ins) != 1 || ins[0].State != want.state || ins[0].Dispatch.Model != want.model {
			t.Fatalf("instruction = %+v, want %s with model %q", ins, want.state, want.model)
		}
		h.submit(run, ins[0], `{}`)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eDone")
}

// A child's result is accepted and the drive after it fails (here an append
// that fails, as on ENOSPC): the verb still reports the acceptance, and the
// error is printed with the run and the unit and noted in the journal for
// the coordinator.
func TestDriveErrorAfterAcceptanceIsReportedNotReturned(t *testing.T) {
	d, _ := effectDef(nil, mergeEffect(wf.KeyOf("merge", "feat")))
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	var progress bytes.Buffer
	c.e = wf.NewEngine(c.cj, c.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return c.now }, Progress: &progress})
	run := c.run(d, nil)
	c.cj.armed = true
	in := c.next(run)[0]
	res, err := c.e.Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(`{}`)})
	if err != nil || res.Class != wf.Accepted {
		t.Fatalf("Submit = %+v, %v; want the acceptance, no error", res, err)
	}
	if c.cj.armed {
		t.Fatal("the armed append never ran")
	}
	want := "your result was recorded; the run hit an error afterwards that the coordinator must see: crash"
	if !strings.Contains(progress.String(), want) || !strings.Contains(progress.String(), "run "+string(run)+", unit ") {
		t.Errorf("stderr lacks the run, the unit and %q: %q", want, progress.String())
	}
	noted := false
	for _, r := range c.kind(run, wf.JournalNote) {
		if strings.Contains(string(r.Data), "the coordinator must see") {
			noted = true
		}
	}
	if !noted {
		t.Error("no journal note for the coordinator")
	}
}
