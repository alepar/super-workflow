package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/approval"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/park"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/resumable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/stale"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/unavailable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// ---------------------------------------------------------------- examples

func TestApprovalOnEngine(t *testing.T) {
	plan := `{"State":"ready","Path":"p/plan.md"}`
	start := func(t *testing.T) (*harness, wf.RunID) {
		d := approval.Definition()
		h := newHarness(t, d, newWorld())
		run := h.run(d, map[string]string{"worktree": "/wt"}) // {worktree} binds from run params
		ins := h.next(run)
		if len(ins) != 1 || ins[0].State != "Draft" {
			t.Fatalf("instructions = %+v, want Draft", ins)
		}
		h.submit(run, ins[0], plan)
		return h, run
	}

	t.Run("approved executes", func(t *testing.T) {
		h, run := start(t)
		ins := h.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate || ins[0].State != "Approve" {
			t.Fatalf("instructions = %+v, want the Approve human gate", ins)
		}
		g := ins[0]
		if g.Token != "" || g.Question != "Run the committed plan?" || strings.Join(g.Answers, ",") != "approve,decline" {
			t.Errorf("gate instruction = %+v", g)
		}
		if !strings.Contains(g.Prompt, "Presents plan: "+string(run)+"/plan.md") {
			t.Errorf("prompt does not present the plan artifact: %q", g.Prompt)
		}
		if want := "workflow answer " + string(run) + " --gate Approve --value <value>"; g.Submit != want {
			t.Errorf("submit = %q, want %q", g.Submit, want)
		}
		if again := h.next(run); len(again) != 1 || again[0].Prompt != g.Prompt || len(h.kind(run, wf.JournalGateAnswered)) != 0 {
			t.Fatalf("a repeat Next must show the same gate and journal nothing: %+v", again)
		}
		if err := h.e.Answer(run, "Approve", "approve"); err != nil {
			t.Fatal(err)
		}
		st := h.drive(run, map[string][]string{"Execute": {`{"Summary":"done"}`}})
		statusOf(t, st, wf.RunEnded, "Done")
		if got := h.kind(run, wf.JournalGateAnswered); len(got) != 1 {
			t.Fatalf("gate_answered records = %d, want 1", len(got))
		}
	})

	t.Run("declined stops", func(t *testing.T) {
		h, run := start(t)
		h.next(run)
		if err := h.e.Answer(run, "Approve", "decline"); err != nil {
			t.Fatal(err)
		}
		statusOf(t, h.status(run), wf.RunEnded, "Rejected")
	})

	t.Run("no answer expires on the journaled clock", func(t *testing.T) {
		h, run := start(t)
		h.next(run)
		h.now = h.now.Add(48*time.Hour - time.Second)
		if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate {
			t.Fatalf("one second before the timeout: instructions = %+v, want the gate still open", ins)
		}
		h.now = h.now.Add(time.Second)
		if ins := h.next(run); len(ins) != 0 {
			t.Fatalf("instructions = %+v, want none after the timeout", ins)
		}
		statusOf(t, h.status(run), wf.RunEnded, "Expired")
		auto := h.kind(run, wf.JournalAutoAnswered)
		if len(auto) != 1 || !strings.Contains(string(auto[0].Data), "timeout") {
			t.Fatalf("auto_answered = %+v, want the timeout record", auto)
		}
	})

	t.Run("answer without an open gate is an error", func(t *testing.T) {
		d := approval.Definition()
		h := newHarness(t, d, newWorld())
		run := h.run(d, nil)
		if err := h.e.Answer(run, "Approve", "approve"); err == nil {
			t.Fatal("Answer before the gate opened returned nil")
		}
	})
}

func TestParkOnEngine(t *testing.T) {
	t.Run("not autonomous waits for the human", func(t *testing.T) {
		d := park.Definition()
		h := newHarness(t, d, nil)
		run, err := h.e.Run(d, nil, park.Options{})
		if err != nil {
			t.Fatal(err)
		}
		ins := h.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate || ins[0].State != "Release" {
			t.Fatalf("instructions = %+v, want the Release gate", ins)
		}
		if err := h.e.Answer(run, "Release", "ship"); err != nil {
			t.Fatal(err)
		}
		statusOf(t, h.status(run), wf.RunEnded, "Shipped")
		if len(h.kind(run, wf.JournalAutoAnswered)) != 0 {
			t.Fatal("a human answer must not journal auto_answered")
		}
	})
	t.Run("autonomous is parked by the policy", func(t *testing.T) {
		d := park.Definition()
		h := newHarness(t, d, nil)
		run, err := h.e.Run(d, nil, park.Options{Autonomous: true})
		if err != nil {
			t.Fatal(err)
		}
		if ins := h.next(run); len(ins) != 0 {
			t.Fatalf("instructions = %+v, want none", ins)
		}
		statusOf(t, h.status(run), wf.RunEnded, "Held")
	})
}

func TestResumableOnEngine(t *testing.T) {
	params := map[string]string{"dataset": "ds"}
	argvs := func(w *world) []string {
		var out []string
		for _, e := range w.applied {
			out = append(out, fmt.Sprint(e.Args["argv"]))
		}
		return out
	}

	t.Run("all stages run", func(t *testing.T) {
		d := resumable.Definition()
		w := newWorld()
		h := newHarness(t, d, &fsWorld{world: w})
		run := h.run(d, params)
		if ins := h.next(run); len(ins) != 0 {
			t.Fatalf("instructions = %+v, want none", ins)
		}
		statusOf(t, h.status(run), wf.RunEnded, "Done")
		want := "[[scripts/fetch ds] [scripts/transform ds] [scripts/publish ds] [scripts/stamp ds]]"
		if got := fmt.Sprint(argvs(w)); got != want {
			t.Fatalf("applied argv = %s, want %s", got, want)
		}
	})

	t.Run("stages with a met postcondition are skipped", func(t *testing.T) {
		d := resumable.Definition()
		w := newWorld()
		h := newHarness(t, d, &fsWorld{world: w, exists: map[string]bool{"work/ds/raw.json": true, "work/ds/shaped.json": true}})
		run := h.run(d, params)
		h.next(run)
		statusOf(t, h.status(run), wf.RunEnded, "Done")
		if got, want := fmt.Sprint(argvs(w)), "[[scripts/publish ds] [scripts/stamp ds]]"; got != want {
			t.Fatalf("applied argv = %s, want %s", got, want)
		}
		if len(h.kind(run, wf.JournalCompletedByObservation)) != 2 {
			t.Fatalf("kinds = %v, want two completed_by_observation", kinds(h.records(run)))
		}
	})

	t.Run("crash in the publish window verifies instead of repeating", func(t *testing.T) {
		d := resumable.Definition()
		w := newWorld()
		fw := &fsWorld{world: w, exists: map[string]bool{"work/ds/raw.json": true, "work/ds/shaped.json": true}}
		h := newHarness(t, d, fw)
		cj := &crashJournal{Journal: h.j, kind: wf.JournalEffectResult, armed: true}
		h.e = wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
		c := &crashHarness{harness: h, cj: cj, w: w, d: d}
		run := c.run(d, params)
		if _, err := c.e.Next(run, ""); err == nil {
			t.Fatal("Next survived the armed crash")
		}
		c.resume()
		ins := c.next(run)
		if len(ins) != 1 || ins[0].State != "Verify" {
			t.Fatalf("instructions = %+v, want the Verify agent", ins)
		}
		if got := fmt.Sprint(argvs(w)); got != "[[scripts/publish ds] [scripts/note-uncertain ds]]" {
			t.Fatalf("applied argv = %s: publish must run once, the note after it", got)
		}
		c.submit(run, ins[0], `{"Published":true}`)
		statusOf(t, c.status(run), wf.RunEnded, "Done")
	})
}

// fsWorld is a world whose fsx reads answer from a set of existing paths.
type fsWorld struct {
	*world
	exists map[string]bool
}

func (f *fsWorld) Read(ws string, d wf.ObservationDesc) (json.RawMessage, error) {
	if d.Adapter == "fsx" && d.Op == "exists" {
		return json.Marshal(f.exists[fmt.Sprint(d.Args["path"])])
	}
	if d.Adapter == "fsx" {
		return json.RawMessage(`""`), nil
	}
	return f.world.Read(ws, d)
}

func TestStaleOnEngine(t *testing.T) {
	head := map[string]any{"worktree": "."}
	setup := func(t *testing.T) (*harness, *world, wf.RunID) {
		d := stale.Definition()
		w := newWorld()
		w.set("git", "head", head, "")
		h := newHarness(t, d, w)
		return h, w, h.run(d, nil)
	}

	t.Run("waits for a head then reviews", func(t *testing.T) {
		h, w, run := setup(t)
		ins := h.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionWait || ins[0].State != "AwaitHead" || ins[0].RetryAfterSeconds != 30 {
			t.Fatalf("instructions = %+v, want one wait with retry-after 30", ins)
		}
		w.set("git", "head", head, "abc")
		ins = h.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent || ins[0].State != "Review" {
			t.Fatalf("instructions = %+v, want the Review agent", ins)
		}
		h.submit(run, ins[0], `{"Approve":true}`)
		statusOf(t, h.status(run), wf.RunEnded, "Reviewed")
	})

	t.Run("head moving under the review is stale and goes back to waiting", func(t *testing.T) {
		h, w, run := setup(t)
		w.set("git", "head", head, "abc")
		in := h.next(run)[0]
		w.set("git", "head", head, "def")
		res := h.submit(run, in, `{"Approve":true}`)
		if res.Class != wf.Stale {
			t.Fatalf("result = %+v, want stale", res)
		}
		// The stale edge led back to AwaitHead, which saw the new head at once.
		var back bool
		for _, r := range h.kind(run, wf.JournalTransition) {
			back = back || (r.State == "Review" && strings.Contains(string(r.Data), `"To":"AwaitHead"`))
		}
		if !back {
			t.Fatalf("kinds = %v, want a Review to AwaitHead transition", kinds(h.records(run)))
		}
		ins := h.next(run)
		if len(ins) != 1 || ins[0].State != "Review" || ins[0].Token == in.Token {
			t.Fatalf("instructions = %+v, want a fresh Review", ins)
		}
		h.submit(run, ins[0], `{"Approve":true}`)
		statusOf(t, h.status(run), wf.RunEnded, "Reviewed")
	})
}

func TestWaitTimesOutOnInjectedClock(t *testing.T) {
	d := stale.Definition()
	w := newWorld()
	w.set("git", "head", map[string]any{"worktree": "."}, "")
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.next(run)

	h.now = h.now.Add(24*time.Hour - 10*time.Second)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionWait || ins[0].RetryAfterSeconds != 10 {
		t.Fatalf("instructions = %+v, want a wait capped at the 10s left", ins)
	}
	h.now = h.now.Add(10 * time.Second)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none after the timeout", ins)
	}
	statusOf(t, h.status(run), wf.RunEnded, "TimedOut")
}

func TestWaitUnavailableReadKeepsWaiting(t *testing.T) {
	d := stale.Definition()
	w := newWorld()
	w.down["git.head"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionWait {
		t.Fatalf("instructions = %+v, want the wait to continue on an unavailable read", ins)
	}
	h.now = h.now.Add(24 * time.Hour)
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "TimedOut")
}

func TestUnavailableOnEngine(t *testing.T) {
	// The example's check never reports a tool as down, so only the semantic
	// route is reachable from a submission; the unavailable route is exercised
	// below on a definition of the same shape whose check can.
	t.Run("three semantic failures end elsewhere", func(t *testing.T) {
		d := unavailable.Definition()
		w := newWorld()
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		for i := 0; i < 3; i++ {
			in := h.next(run)[0]
			if _, err := h.e.Fail(run, in.Token, "no verdict"); err != nil {
				t.Fatal(err)
			}
		}
		statusOf(t, h.status(run), wf.RunEnded, "NotConverged")
		if w.appliedCount() != 0 {
			t.Fatalf("applied = %+v: the semantic route files no blocker", w.applied)
		}
	})

	t.Run("accepted verdict", func(t *testing.T) {
		d := unavailable.Definition()
		h := newHarness(t, d, newWorld())
		run := h.run(d, nil)
		statusOf(t, h.drive(run, map[string][]string{"Triage": {`{"Summary":"ok"}`}}), wf.RunEnded, "TriageDone")
	})

	t.Run("tool down takes the unavailable route and files the blocker", func(t *testing.T) {
		d, w := unavailableShape()
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		in := h.next(run)[0]
		res := h.submit(run, in, `{"Down":true}`)
		if res.Class != wf.Unavailable {
			t.Fatalf("class = %s, want unavailable", res.Class)
		}
		statusOf(t, h.status(run), wf.RunEnded, "uBlocked")
		if w.appliedCount() != 1 || w.applied[0].Adapter != "bd" {
			t.Fatalf("applied = %+v, want the one blocker effect", w.applied)
		}
	})
}

type (
	uTriage  struct{}
	uDone    struct{}
	uBlocked struct{}
	uFailed  struct{}
	uOut     struct{ Down bool }
)

func unavailableShape() (*wf.Def, *world) {
	d := wf.New("ushape")
	a := wf.NewAgent[uTriage, uOut](d)
	done := wf.NewTerminal[uDone](d, wf.Success)
	blocked := wf.NewTerminal[uBlocked](d, wf.Stopped)
	failed := wf.NewTerminal[uFailed](d, wf.Stopped)
	blocker := a.To(blocked, "tool unavailable").With(wf.ExternalEffect{
		Adapter: "bd", Op: "create", Args: map[string]any{"title": "blocker"}, Key: wf.KeyOf("bd", "blocker"),
	})
	a.Spec(wf.PromptText("Triage."),
		wf.Retry(wf.Const(3), a.To(failed, "not converged")).OnClass(wf.Unavailable, blocker),
		wf.Checkf("tool-up", func(o uOut, _ wf.Ctx) error {
			if o.Down {
				return fmt.Errorf("%w: triage tool", wf.ErrUnavailable)
			}
			return nil
		}))
	ok := a.To(done, "triaged")
	a.Route(func(uOut, wf.Ctx) wf.Edge[uTriage] { return ok }, ok)
	return d, newWorld()
}

// ---------------------------------------------------------------- policy

type pOpts struct {
	Autonomous bool
	Mode       string
}

type pAns string

type (
	pAsk  struct{}
	pGo   struct{}
	pStop struct{}
)

// policyDef: a gate with two rules; the first needs Mode=fast and Autonomous.
func policyDef(res wf.Reservation) *wf.Def {
	d := wf.New("policy")
	opts := wf.Options[pOpts](d)
	ans := wf.NewEnum(d, "p-answer", pAns("go"), pAns("stop"))
	g := wf.NewGate[pAsk, pAns](d, "Go?", ans, res)
	goT := wf.NewTerminal[pGo](d, wf.Success)
	stopT := wf.NewTerminal[pStop](d, wf.Stopped)
	g.Answers(wf.On(pAns("go"), g.To(goT, "go")), wf.On(pAns("stop"), g.To(stopT, "stop")))
	auto := wf.Field(opts, func(o *pOpts) *bool { return &o.Autonomous })
	mode := wf.Field(opts, func(o *pOpts) *string { return &o.Mode })
	d.SetPolicy(wf.Policy(
		wf.PolicyAnswer(g, pAns("go"), mode.Is("fast").And(wf.Not(auto.Is(false)))).Park("went fast"),
		wf.PolicyAnswer(g, pAns("stop"), auto.Is(true)).Park("stopped by default"),
	))
	return d
}

func TestPolicyAnswersGateWithParkNote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     pOpts
		terminal string
		answer   string
		park     string
	}{
		{"first matching rule", pOpts{Autonomous: true, Mode: "fast"}, "pGo", "go", "went fast"},
		{"later rule when the first does not match", pOpts{Autonomous: true, Mode: "slow"}, "pStop", "stop", "stopped by default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := policyDef(wf.PolicyAnswerable)
			h := newHarness(t, d, nil)
			run, err := h.e.Run(d, nil, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if ins := h.next(run); len(ins) != 0 {
				t.Fatalf("instructions = %+v, want none", ins)
			}
			statusOf(t, h.status(run), wf.RunEnded, tc.terminal)
			auto := h.kind(run, wf.JournalAutoAnswered)
			if len(auto) != 1 {
				t.Fatalf("auto_answered records = %d, want 1", len(auto))
			}
			var data wf.AutoAnsweredData
			if err := json.Unmarshal(auto[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Answer != tc.answer || data.Park != tc.park || data.Plan == nil {
				t.Fatalf("auto_answered = %+v, want answer %q park %q with a plan", data, tc.answer, tc.park)
			}
		})
	}

	t.Run("no matching rule waits for a human", func(t *testing.T) {
		d := policyDef(wf.PolicyAnswerable)
		h := newHarness(t, d, nil)
		run, err := h.e.Run(d, nil, pOpts{Autonomous: false, Mode: "fast"})
		if err != nil {
			t.Fatal(err)
		}
		if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate {
			t.Fatalf("instructions = %+v, want the human gate", ins)
		}
		if len(h.kind(run, wf.JournalAutoAnswered)) != 0 {
			t.Fatal("no rule matched, yet auto_answered was journaled")
		}
	})
}

func TestReservedGateWaitsForHuman(t *testing.T) {
	d := policyDef(wf.Reserved) // a rule on a reserved gate is a lint error; the engine must not use it
	h := newHarness(t, d, nil)
	run, err := h.e.Run(d, nil, pOpts{Autonomous: true, Mode: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate {
			t.Fatalf("instructions = %+v, want the human gate", ins)
		}
	}
	if len(h.kind(run, wf.JournalAutoAnswered)) != 0 {
		t.Fatal("a reserved gate was auto answered")
	}
	if err := h.e.Answer(run, "pAsk", "stop"); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "pStop")
}

func TestAnswerOutsideEnumRejected(t *testing.T) {
	d := policyDef(wf.Reserved)
	h := newHarness(t, d, nil)
	run, err := h.e.Run(d, nil, pOpts{})
	if err != nil {
		t.Fatal(err)
	}
	h.next(run)
	before := len(h.records(run))
	err = h.e.Answer(run, "pAsk", "maybe")
	if err == nil || !strings.Contains(err.Error(), "p-answer") || !strings.Contains(err.Error(), "go") || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("error = %v, want one naming the answer set", err)
	}
	if got := len(h.records(run)); got != before {
		t.Fatalf("records %d -> %d: a rejected answer must journal nothing", before, got)
	}
	if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate {
		t.Fatalf("instructions = %+v, want the gate still open", ins)
	}
}

// ---------------------------------------------------------------- backoff and lease

type (
	bWork  struct{}
	bDone  struct{}
	bPause struct{}
	bFail  struct{}
	bOut   struct{ Down bool }
)

func engBackoffDef() *wf.Def {
	d := wf.New("backoff")
	a := wf.NewAgent[bWork, bOut](d)
	done := wf.NewTerminal[bDone](d, wf.Success)
	pause := wf.NewTerminal[bPause](d, wf.Stopped)
	fail := wf.NewTerminal[bFail](d, wf.Stopped)
	d.OnFailure(wf.Blocked, fail)
	a.Spec(wf.PromptText("Work."),
		wf.Retry(wf.Const(5), a.To(fail, "exhausted")).
			OnClassBackoff(wf.Unavailable, a.To(pause, "pause"), 10*time.Second, 20*time.Second),
		wf.Checkf("up", func(o bOut, _ wf.Ctx) error {
			if o.Down {
				return fmt.Errorf("%w: tool", wf.ErrUnavailable)
			}
			return nil
		}))
	ok := a.To(done, "ok")
	a.Route(func(bOut, wf.Ctx) wf.Edge[bWork] { return ok }, ok)
	return d
}

func TestBackoffSurfacesAsWait(t *testing.T) {
	d := engBackoffDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in := h.next(run)[0]
	start := h.now

	h.submit(run, in, `{"Down":true}`)
	sched := h.kind(run, wf.JournalBackoffScheduled)
	if len(sched) != 1 {
		t.Fatalf("backoff_scheduled records = %d, want 1", len(sched))
	}
	var data wf.BackoffScheduledData
	if err := json.Unmarshal(sched[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Index != 0 || data.Delay != 10*time.Second || !data.Due.Equal(start.Add(10*time.Second)) || data.Class != wf.Unavailable {
		t.Fatalf("backoff_scheduled = %+v, want index 0, 10s, due start+10s", data)
	}

	ins := h.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionWait || ins[0].RetryAfterSeconds != 10 || ins[0].State != "bWork" {
		t.Fatalf("instructions = %+v, want a wait with retry-after 10", ins)
	}
	h.now = start.Add(4 * time.Second)
	if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionWait || ins[0].RetryAfterSeconds != 6 {
		t.Fatalf("instructions = %+v, want the wait to count down to 6", ins)
	}
	if got := len(h.kind(run, wf.JournalBackoffScheduled)); got != 1 {
		t.Fatalf("backoff_scheduled records = %d, want 1: asking again must not reschedule", got)
	}

	// Due: the state is re-issued on the same attempt, a fresh token, no attempt spent.
	h.now = start.Add(10 * time.Second)
	ins = h.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent || ins[0].Attempt != 1 || ins[0].Token == in.Token {
		t.Fatalf("instructions = %+v, want attempt 1 re-issued with a fresh token", ins)
	}
	h.submit(run, ins[0], `{"Down":true}`)
	second := h.kind(run, wf.JournalBackoffScheduled)
	if len(second) != 2 {
		t.Fatalf("backoff_scheduled records = %d, want 2", len(second))
	}
	if ins := h.next(run); len(ins) != 1 || ins[0].RetryAfterSeconds != 20 {
		t.Fatalf("instructions = %+v, want the second delay of 20s", ins)
	}

	// The schedule is spent: the next unavailable takes the pause edge.
	h.now = h.now.Add(20 * time.Second)
	ins = h.next(run)
	h.submit(run, ins[0], `{"Down":true}`)
	statusOf(t, h.status(run), wf.RunEnded, "bPause")
	if got := len(h.kind(run, wf.JournalBackoffScheduled)); got != 2 {
		t.Fatalf("backoff_scheduled records = %d, want 2: the then edge is not scheduled", got)
	}
}

type (
	lItem struct{ ID string }
	lRes  struct{}
	lFan  struct{}
	lEnd  struct{}
	lWork struct{}
	lKDon struct{}
	lOut  struct{}
)

func TestLeaseExpiryReissues(t *testing.T) {
	kid := wf.New("kid")
	child := wf.Typed[lItem, lRes](kid)
	work := wf.NewAgent[lWork, lOut](kid)
	kdone := wf.NewTerminal[lKDon](kid, wf.Success)
	work.Spec(wf.PromptText("Work."), wf.Retry(wf.Const(2), work.To(kdone, "gave up")))
	next := work.To(kdone, "done")
	work.Route(func(lOut, wf.Ctx) wf.Edge[lWork] { return next }, next)

	d := wf.New("lease")
	src := wf.FromObservation[lItem](wf.Observe[[]lItem]("x", "items", nil))
	wf.NewMap[lFan, lItem, string, lRes](d, src, func(i lItem) string { return i.ID }, child).Lease(time.Minute)
	wf.NewTerminal[lEnd](d, wf.Success)

	h := newHarness(t, d, newWorld())
	run := h.run(d, nil)
	// The Map's own execution lands with another task: spawn its child by hand.
	recs := h.records(run)
	spawn := wf.JournalRecord{
		Kind: wf.JournalChildSpawned, Run: run, DefHash: recs[0].DefHash, At: h.now,
		Data: mustJSON(t, wf.ChildSpawnedData{ID: "kid-1", Def: "kid", State: "lFan"}),
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(spawn)
	unlock()
	if err != nil {
		t.Fatal(err)
	}

	issue := func() wf.Instruction {
		t.Helper()
		ins, err := h.e.Next(run, "kid-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent || ins[0].State != "kid/lWork" {
			t.Fatalf("instructions = %+v, want the child's work agent", ins)
		}
		return ins[0]
	}
	first := issue()
	start := h.now
	// The wave Next claims the unit, so the lease rides on attempt_started.
	claims := attemptStarted(t, h, run)
	if len(claims) != 1 {
		t.Fatalf("attempt_started records = %d, want 1 at issue", len(claims))
	}
	if claims[0].Session != "wave" || !claims[0].Expires.Equal(start.Add(time.Minute)) {
		t.Fatalf("attempt_started = %+v, want a wave claim held until start+1m", claims[0])
	}
	if n := len(h.kind(run, wf.JournalLeaseClaimed)); n != 0 {
		t.Fatalf("lease_claimed records = %d, want 0: the lease rides on attempt_started", n)
	}

	h.now = start.Add(59 * time.Second)
	if again := issue(); again.Token != first.Token || len(lostRecords(t, h, run)) != 0 {
		t.Fatal("before expiry the same attempt must stand")
	}

	h.now = start.Add(time.Minute)
	second := issue()
	if second.Token == first.Token || second.Attempt != 2 {
		t.Fatalf("after expiry: token %q attempt %d, want a fresh token on attempt 2", second.Token, second.Attempt)
	}
	if lost := lostRecords(t, h, run); len(lost) != 1 || lost[0].Cause != wf.LostCauseLease {
		t.Fatalf("attempt_lost records = %+v, want one for the lease", lost)
	}
	if got := len(attemptStarted(t, h, run)); got != 2 {
		t.Fatalf("attempt_started records = %d, want a new claim for the re-issue", got)
	}
	res, err := h.e.Submit(run, wf.Submission{Token: first.Token, Child: "kid-1", Output: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != wf.Rejected || !strings.Contains(strings.Join(res.Refusal, " "), "superseded") {
		t.Fatalf("old token result = %+v, want Rejected as superseded", res)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------- visit tokens, delegations, deadlines

type (
	lgAsk  struct{}
	lgDone struct{}
	lgAns  string
)

// loopGateDef is one reserved gate whose "again" answer returns to itself.
func loopGateDef() *wf.Def {
	d := wf.New("loop-gate")
	ans := wf.NewEnum(d, "lg-ans", lgAns("again"), lgAns("done"))
	g := wf.NewGate[lgAsk, lgAns](d, "Again?", ans, wf.Reserved)
	end := wf.NewTerminal[lgDone](d, wf.Success)
	g.Answers(wf.On(lgAns("again"), g.To(g, "again")), wf.On(lgAns("done"), g.To(end, "done")))
	return d
}

func TestAnswerStaleVisitRejected(t *testing.T) {
	d := loopGateDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first := h.next(run)
	if len(first) != 1 || first[0].Visit != 1 {
		t.Fatalf("first arrival = %+v, want one gate instruction at visit 1", first)
	}
	g := wf.GateAnswer{Gate: first[0].State, Child: first[0].Child, Visit: first[0].Visit, Value: "again"}
	if err := h.e.AnswerVisit(run, g); err != nil {
		t.Fatal(err)
	}
	second := h.next(run)
	if len(second) != 1 || second[0].Visit != 2 {
		t.Fatalf("second arrival = %+v, want the gate again at visit 2", second)
	}
	answered := len(h.kind(run, wf.JournalGateAnswered))
	if err := h.e.AnswerVisit(run, g); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "stale visit") {
		t.Fatalf("a retried answer at visit 1 = %v, want ErrRejected naming a stale visit", err)
	}
	if n := len(h.kind(run, wf.JournalGateAnswered)); n != answered {
		t.Fatalf("a rejected answer journaled %d record(s)", n-answered)
	}
	g.Visit, g.Value = 2, "done"
	if err := h.e.AnswerVisit(run, g); err != nil {
		t.Fatalf("the current visit's answer = %v, want accepted", err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "lgDone")
}

func TestAnswerWrongMapChildRejected(t *testing.T) {
	d := hoGateFan(2)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	for _, u := range h.handOut(run, 2) {
		if err := h.e.Started(run, u.Name, claim("a")); err != nil {
			t.Fatal(err)
		}
		if _, err := h.e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
			t.Fatal(err)
		}
	}
	gates := h.next(run)
	if len(gates) != 2 || gates[0].Child == gates[1].Child || gates[0].Visit != 1 {
		t.Fatalf("gates = %+v, want two children's gates at visit 1", gates)
	}
	a, b := gates[0], gates[1]
	for _, wrong := range []string{"", "no-such-child"} {
		err := h.e.AnswerVisit(run, wf.GateAnswer{Gate: a.State, Child: wrong, Visit: a.Visit, Value: "yes"})
		if !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("answer naming child %q = %v, want ErrRejected", wrong, err)
		}
	}
	if err := h.e.AnswerVisit(run, wf.GateAnswer{Gate: b.State, Child: b.Child, Visit: b.Visit, Value: "yes"}); err != nil {
		t.Fatal(err)
	}
	if open := h.next(run); len(open) != 1 || open[0].Child != a.Child {
		t.Fatalf("after answering %s the open gates = %+v, want only %s", b.Child, open, a.Child)
	}
	if err := h.e.AnswerVisit(run, wf.GateAnswer{Gate: b.State, Child: b.Child, Visit: b.Visit, Value: "yes"}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("a retried answer for the answered child = %v, want ErrRejected", err)
	}
}

func TestTimedGateCarriesDeadline(t *testing.T) {
	d := approval.Definition()
	h := newHarness(t, d, newWorld())
	run := h.run(d, map[string]string{"worktree": "/wt"})
	h.submit(run, h.next(run)[0], `{"State":"ready","Path":"p/plan.md"}`)
	g := h.next(run)[0]
	want := h.now.Add(48 * time.Hour)
	if !g.Deadline.Equal(want) || g.RetryAfterSeconds != 48*3600 {
		t.Fatalf("gate deadline %v retry-after %ds, want %v and %d", g.Deadline, g.RetryAfterSeconds, want, 48*3600)
	}
	h.now = h.now.Add(time.Hour)
	if g := h.next(run)[0]; !g.Deadline.Equal(want) || g.RetryAfterSeconds != 47*3600 {
		t.Fatalf("an hour later: deadline %v retry-after %ds, want the same deadline and %d", g.Deadline, g.RetryAfterSeconds, 47*3600)
	}
	d2 := loopGateDef()
	h2 := newHarness(t, d2, nil)
	if g := h2.next(h2.run(d2, nil))[0]; !g.Deadline.IsZero() || g.RetryAfterSeconds != 0 {
		t.Fatalf("an untimed gate carries deadline %v retry-after %d", g.Deadline, g.RetryAfterSeconds)
	}
}

func TestDelegationShownOnGate(t *testing.T) {
	d := loopGateDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Delegations: map[string]string{"lgAsk": "done"}})
	run := h.run(d, nil)
	g := h.next(run)
	if len(g) != 1 || !g[0].Delegated || g[0].DelegatedAnswer != "done" {
		t.Fatalf("gate = %+v, want delegated with answer done", g)
	}
	if len(h.kind(run, wf.JournalGateAnswered))+len(h.kind(run, wf.JournalAutoAnswered)) != 0 {
		t.Fatal("the engine answered a delegated gate")
	}
	if st := h.status(run); st.Delegations["lgAsk"] != "done" {
		t.Fatalf("status delegations = %v, want lgAsk=done", st.Delegations)
	}
	// A fresh engine over the same journal, built with no delegations, still sees them.
	fresh := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
	if st, err := fresh.Status(run); err != nil || st.Delegations["lgAsk"] != "done" {
		t.Fatalf("fresh status = %v, %v; want the journaled delegation", st.Delegations, err)
	}
	// An undelegated run shows none.
	h2 := newHarness(t, d, nil)
	if g := h2.next(h2.run(d, nil))[0]; g.Delegated || g.DelegatedAnswer != "" {
		t.Fatalf("an undelegated gate shows %+v", g)
	}
}

func TestResumeKeepsJournaledOptionsAndDelegations(t *testing.T) {
	d := policyDef(wf.PolicyAnswerable)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunID: "p1", Delegations: map[string]string{"pAsk": "go"}})
	if _, err := h.e.Run(d, nil, pOpts{Mode: "slow"}); err != nil {
		t.Fatal(err)
	}
	resume := func(delegations map[string]string, opts any) (wf.RunID, error) {
		e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, RunID: "p1", Delegations: delegations})
		return e.Run(d, nil, opts)
	}
	if _, err := resume(nil, nil); err != nil {
		t.Fatalf("resume with nothing passed = %v, want the journaled options kept", err)
	}
	if _, err := resume(nil, pOpts{Mode: "slow"}); err != nil {
		t.Fatalf("resume with equal options = %v", err)
	}
	if _, err := resume(nil, pOpts{Mode: "fast"}); err == nil || !strings.Contains(err.Error(), "different options") {
		t.Fatalf("resume with other options = %v, want a different-options error", err)
	}
	if _, err := resume(map[string]string{"pAsk": "stop"}, nil); err == nil || !strings.Contains(err.Error(), "different delegations") {
		t.Fatalf("resume with other delegations = %v, want a different-delegations error", err)
	}
	st := h.status("p1")
	if !strings.Contains(string(st.Options), `"slow"`) || st.Delegations["pAsk"] != "go" {
		t.Fatalf("status options %s delegations %v, want the journaled ones", st.Options, st.Delegations)
	}
}
