package wftest_test

import (
	"encoding/json"
	"errors"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"path/filepath"
	"strings"
	"testing"
)

type fuzzAgent struct{}
type fuzzDone struct{}
type fuzzStop struct{}
type fuzzSpin struct{}

func fuzzSimpleFactory(stop bool) func() *wftest.Sim {
	return func() *wftest.Sim {
		d := wf.New("simple")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		bad := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(2), a.To(bad, "attempt budget exhausted")))
		edge := a.To(done, "accepted")
		if stop {
			edge = a.To(bad, "unexpected rejection")
		}
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return edge }, edge)
		return wftest.New(d)
	}
}

// A bound checked only between agent turns never stops a pure-compute loop.
func TestFuzzBoundsSynchronousComputeCycle(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("cycle")
		c := wf.NewCompute[fuzzSpin, bool](d, func(wf.Ctx) (bool, error) { return false, nil })
		edge := c.To(c, "again")
		c.Route(func(bool, wf.Ctx) wf.Edge[fuzzSpin] { return edge }, edge)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 11, MaxSteps: 8})
	if got.Finding == nil || got.Finding.Rule != "loops-unbounded" {
		t.Fatalf("unbounded cycle: %s", got)
	}
	if got.Trace == nil || !strings.Contains(got.Trace.String(), "transition") {
		t.Fatalf("missing named trace: %s", got)
	}
}

// A graph reaching Stopped through an ordinary accepted route is the rung3
// failure shape, even though the run technically ended.
func TestFuzzRejectsSilentAcceptedStop(t *testing.T) {
	got := wftest.Fuzz(fuzzSimpleFactory(true), wftest.FuzzConfig{Seed: 7, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}}})
	if got.Finding == nil || got.Finding.Rule != "ends-silent" || got.Finding.State != "simple/fuzzAgent" || got.Finding.Edge != "unexpected rejection" {
		t.Fatalf("silent stop: %s", got)
	}
	if !strings.Contains(got.String(), "fuzz: ends-silent simple/fuzzAgent") || !strings.Contains(got.String(), "seed 7") {
		t.Fatalf("finding lacks portable identity: %s", got)
	}
}

func TestFuzzSuccessfulRunReplaysWithoutLiveUnits(t *testing.T) {
	got := wftest.Fuzz(fuzzSimpleFactory(false), wftest.FuzzConfig{Seed: 0, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}}})
	if got.Finding != nil || got.Status.Outcome != wf.Success || got.Status.Running != 0 || !got.ReplayEqual {
		t.Fatalf("successful replay: %s", got)
	}
	again := got.Replay(fuzzSimpleFactory(false))
	if again.Finding != nil || again.Status.Terminal != "fuzzDone" {
		t.Fatalf("script replay: %s", again)
	}
}

func TestFuzzEndedChildIsRecoveredBeforeStall(t *testing.T) {
	got := wftest.Fuzz(fuzzSimpleFactory(false), wftest.FuzzConfig{Seed: 19, MaxSteps: 8, Prefix: []wftest.FuzzAction{
		{State: "fuzzAgent", Kind: wftest.FuzzEndedWithoutDone},
		{State: "fuzzAgent", Kind: wftest.FuzzRecover, Output: json.RawMessage(`"restart"`)},
		{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}}})
	if got.Finding != nil || got.Status.Outcome != wf.Success || got.Agents != 2 {
		t.Fatalf("ended child recovery: %s", got)
	}
	ended := 0
	for _, r := range got.Trace.Journal() {
		if r.Kind == wf.JournalDegraded {
			var d wf.DegradedData
			json.Unmarshal(r.Data, &d)
			// F1: the ended child is offered for recovery; the restart is recorded.
			if d.Kind == wf.DegradedRecovery && d.Action == "restart: offered again" && d.Cause != "" {
				ended++
			}
		}
	}
	if ended != 1 {
		t.Fatalf("ended recovery was not observed exactly once: %s", got)
	}
}

func TestFuzzRefusalReportHasRepairAndNamedBoundary(t *testing.T) {
	var actions []wftest.FuzzAction
	for range 2 { // each refusal spends one of the two attempts (C5)
		actions = append(actions, wftest.FuzzAction{State: "fuzzAgent", Kind: wftest.FuzzRefuse})
	}
	got := wftest.Fuzz(fuzzSimpleFactory(false), wftest.FuzzConfig{Seed: 23, MaxSteps: 16, Prefix: actions})
	if got.Finding != nil || len(got.Refusals) != 2 || !strings.Contains(got.String(), "fix: ") {
		t.Fatalf("unnamed refusal or missing repair: %s", got)
	}
	for _, r := range got.Refusals {
		if len(r.Reasons) == 0 || r.Fix == "" {
			t.Fatalf("refusal has no repair: %+v", r)
		}
	}
}

func TestFuzzMinimizesRepairedRefusalBeforeSilentStop(t *testing.T) {
	got := wftest.Fuzz(fuzzSimpleFactory(true), wftest.FuzzConfig{Seed: 31, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzRefuse}, {State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}}})
	if got.Finding == nil || got.Finding.Rule != "ends-silent" {
		t.Fatalf("old repaired refusal excused a later silent stop: %s", got)
	}
	small := got.Minimize(fuzzSimpleFactory(true))
	if !small.ShrinkComplete || len(small.Script) != 1 || small.Script[0].Kind != wftest.FuzzSubmit || small.Finding == nil || small.Finding.Rule != "ends-silent" {
		t.Fatalf("non-minimal script: %s", small)
	}
	again := small.Replay(fuzzSimpleFactory(true))
	if again.Finding == nil || again.Finding.Rule != "ends-silent" {
		t.Fatalf("minimized script is not replayable: %s", again)
	}
}

// A panic in a user route must still report its seed and input journal.
func TestFuzzReportsRoutePanicWithSeed(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("panic")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		edge := a.To(done, "normal")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), edge))
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { panic("bad output assumption") }, edge)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 47, MaxSteps: 4, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`false`)}}})
	if got.Finding == nil || got.Finding.Rule != "definition-panic" || !strings.Contains(got.String(), "seed 47") || !strings.Contains(got.Trace.String(), "submitted") {
		t.Fatalf("panic lost its reproducer: %s", got)
	}
}

type fuzzVerdict string

func TestFuzzDrawsEveryEnumValueOnRealAgent(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("enum")
		a := wf.NewAgent[fuzzAgent, fuzzVerdict](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		edge := a.To(done, "done")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(2), edge))
		values := wf.NewEnum(d, "verdict", fuzzVerdict("pass"), fuzzVerdict("reject"))
		a.RouteCases(wf.Cases[fuzzAgent, fuzzVerdict](func(v fuzzVerdict) fuzzVerdict { return v }, values, wf.On(fuzzVerdict("pass"), edge), wf.On(fuzzVerdict("reject"), edge)))
		return wftest.New(d)
	}
	seen := map[string]bool{}
	for seed := int64(0); seed < 21; seed++ {
		got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: seed, MaxSteps: 16})
		if got.Finding != nil {
			t.Fatalf("seed %d: %s", seed, got)
		}
		for _, action := range got.Script {
			if action.Kind == wftest.FuzzSubmit {
				var value string
				if err := json.Unmarshal(action.Output, &value); err != nil {
					t.Fatal(err)
				}
				seen[value] = true
			}
		}
	}
	if !seen["pass"] || !seen["reject"] || len(seen) != 2 {
		t.Fatalf("enum exploration omitted branches: %v", seen)
	}
}

// A parent propagates a verified child failure, but must never hide an
// unexplained accepted child stop behind its own non-success exit.
func TestFuzzChildStopKeepsItsCausalFailure(t *testing.T) {
	for _, named := range []bool{true, false} {
		factory := func() *wftest.Sim {
			child := wf.New("simple")
			a := wf.NewAgent[fuzzAgent, bool](child)
			stop := wf.NewTerminal[fuzzStop](child, wf.Stopped)
			a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(2), a.To(stop, "attempt budget exhausted")))
			edge := a.To(stop, "unexpected rejection")
			a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return edge }, edge)
			wf.Yield(stop, func(wf.Ctx) struct{} { return struct{}{} })
			d := wf.New("parent")
			sub := wf.NewSub[fuzzSpin](d, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
			stopped := wf.NewTerminal[fuzzStop](d, wf.Success)
			sub.Exits(wf.OnExit[fuzzStop](sub.To(stopped, "child stopped")))
			return wftest.New(d)
		}
		kind := wftest.FuzzSubmit
		if named {
			kind = wftest.FuzzTransient
		}
		actions := []wftest.FuzzAction{{State: "fuzzAgent", Kind: kind, Output: json.RawMessage(`true`)}, {State: "fuzzAgent", Kind: kind, Output: json.RawMessage(`true`)}}
		if named {
			// The spent budget is offered for recovery (B2): the coordinator gives up.
			actions = append(actions, wftest.FuzzAction{State: "fuzzAgent", Kind: wftest.FuzzRecover, Output: json.RawMessage(`"give-up"`)})
		}
		result := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 0, MaxSteps: 12, Prefix: actions})
		if named && result.Finding != nil {
			t.Fatalf("named child failure: %s", result)
		}
		if !named && (result.Finding == nil || result.Finding.State != "simple/fuzzAgent") {
			t.Fatalf("silent child hidden: %s", result)
		}
	}
}

func TestFuzzWritesAllocatedArtifactsBeforeSubmitting(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("artifacts")
		a := wf.NewAgent[fuzzAgent, bool](d)
		b := wf.NewAgent[fuzzSpin, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		wf.NewArtifact[string](a, "report.md")
		wf.NewArtifact[string](b, "report.md")
		a.Spec(wf.PromptText("write first"), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "failed")))
		next := a.To(b, "next")
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return next }, next)
		b.Spec(wf.PromptText("write second"), wf.Retry[fuzzSpin](wf.Const(1), b.To(stop, "failed")))
		end := b.To(done, "done")
		b.Route(func(bool, wf.Ctx) wf.Edge[fuzzSpin] { return end }, end)
		return wftest.New(d)
	}
	result := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 0, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}, {State: "fuzzSpin", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}}})
	if result.Finding != nil || result.Status.Outcome != wf.Success || !result.ReplayEqual {
		t.Fatalf("allocated artifacts: %s", result)
	}
	paths := map[string]bool{}
	for _, record := range result.Trace.Journal() {
		if record.Kind != wf.JournalSubmitted {
			continue
		}
		var data wf.SubmittedData
		if err := json.Unmarshal(record.Data, &data); err != nil {
			t.Fatal(err)
		}
		ref, ok := data.Artifacts["report.md"]
		if !ok || ref.Path == "" || ref.Hash == "" || paths[ref.Path] {
			t.Fatalf("missing or shared producer artifact: %+v", data.Artifacts)
		}
		paths[ref.Path] = true
	}
	if len(paths) != 2 {
		t.Fatalf("got %d artifact paths, want 2", len(paths))
	}
}

func TestFuzzRejectsChildThatStartsStopped(t *testing.T) {
	factory := func() *wftest.Sim {
		child := wf.New("initial-child")
		stop := wf.NewTerminal[fuzzStop](child, wf.Stopped)
		wf.Yield(stop, func(wf.Ctx) struct{} { return struct{}{} })
		d := wf.New("parent")
		sub := wf.NewSub[fuzzSpin](d, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		sub.Exits(wf.OnExit[fuzzStop](sub.To(done, "recovered")))
		return wftest.New(d)
	}
	result := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 5, MaxSteps: 8})
	if result.Finding == nil || result.Finding.Rule != "ends-silent" || result.Finding.State != "initial-child/fuzzStop" {
		t.Fatalf("initial child stop hidden: %s", result)
	}
}

func TestFuzzArtifactRetriesShareProducerAndRefreshContent(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("retry-artifact")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		wf.NewArtifact[string](a, "report.md")
		a.Spec(wf.PromptText("write"), wf.Retry[fuzzAgent](wf.Const(2), a.To(stop, "failed")))
		end := a.To(done, "done")
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return end }, end)
		return wftest.New(d)
	}
	// The refusal spends attempt 1 (C5); attempt 2 writes its own artifact.
	actions := []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzRefuse}, {State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage("true")}}
	result := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 0, MaxSteps: 12, Prefix: actions})
	if result.Finding != nil || result.Status.Outcome != wf.Success {
		t.Fatalf("artifact retry: %s", result)
	}
}

// A semantic check a schema cannot express keeps schema-only seeds at the
// first state; known-good outputs carry seeds past it, and coverage reports it.
func fuzzHeadingFactory() *wftest.Sim {
	d := wf.New("heading")
	a := wf.NewAgent[fuzzAgent, string](d)
	b := wf.NewAgent[fuzzSpin, bool](d)
	done := wf.NewTerminal[fuzzDone](d, wf.Success)
	stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
	a.Spec(wf.PromptText("write a section"), wf.Retry[fuzzAgent](wf.Const(2), a.To(stop, "attempt budget exhausted")),
		wf.Checkf("heading", func(out string, _ wf.Ctx) error {
			if !strings.HasPrefix(out, "## ") {
				return errors.New("the answer does not start with a heading")
			}
			return nil
		}))
	next := a.To(b, "written")
	a.Route(func(string, wf.Ctx) wf.Edge[fuzzAgent] { return next }, next)
	b.Spec(wf.PromptText("confirm"), wf.Retry[fuzzSpin](wf.Const(2), b.To(stop, "attempt budget exhausted")))
	end := b.To(done, "confirmed")
	b.Route(func(bool, wf.Ctx) wf.Edge[fuzzSpin] { return end }, end)
	return wftest.New(d)
}

func TestFuzzValidCorpusReachesDepthAndCoverageReportsIt(t *testing.T) {
	run := func(valid map[string][]json.RawMessage) *wftest.FuzzCoverage {
		c := wftest.NewFuzzCoverage(fuzzHeadingFactory, nil)
		for seed := int64(0); seed < 24; seed++ {
			got := wftest.Fuzz(fuzzHeadingFactory, wftest.FuzzConfig{Seed: seed, MaxSteps: 32, Valid: valid})
			if got.Finding != nil {
				t.Fatalf("seed %d: %s", seed, got)
			}
			c.Add(got)
		}
		return c
	}
	bare := run(nil)
	if bare.Reached["fuzzSpin"] != 0 || len(bare.Problems()) != 2 {
		t.Fatalf("schema-only seeds passed a heading check:\n%s%v", bare, bare.Problems())
	}
	fed := run(map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`"## Section"`)}})
	if problems := fed.Problems(); len(problems) != 0 {
		t.Fatalf("valid corpus did not cover the definition:\n%s%v", fed, problems)
	}
	if fed.Mix["valid"] == 0 || fed.Mix["schema"] == 0 {
		t.Fatalf("the mix must keep schema-only submits beside valid ones: %v", fed.Mix)
	}
	if !strings.Contains(fed.String(), "fuzzSpin") || !strings.Contains(fed.String(), "ends ") {
		t.Fatalf("coverage report: %s", fed)
	}
}

func TestFuzzCoverageExcusesOnlyWithAReason(t *testing.T) {
	c := wftest.NewFuzzCoverage(fuzzHeadingFactory, map[string]string{"fuzzSpin": "behind a heading", "gone": "renamed"})
	c.Add(&wftest.FuzzResult{End: "success", Reached: []string{"fuzzAgent", "fuzzSpin"}})
	got := strings.Join(c.Problems(), "\n")
	if !strings.Contains(got, "heading/fuzzSpin: marked unreachable") || !strings.Contains(got, "heading/gone: marked unreachable but the definition has no such") {
		t.Fatalf("stale excuses not reported: %s", got)
	}
}

// A read the test world cannot answer is a gap in the fixture, never a named
// failure of the definition.
func TestFuzzMissingWorldValueIsFixtureGap(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("gap")
		a := wf.NewAgent[fuzzAgent, bool](d)
		obs := shell.Observe(shell.Run("cat", "missing.txt"))
		c := wf.NewCompute[fuzzSpin, string](d, func(ctx wf.Ctx) (string, error) {
			r, err := wf.Observed(ctx, obs)
			return r.Stdout, err
		}).Observes(obs)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "attempt budget exhausted")))
		next := a.To(c, "read")
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return next }, next)
		end := c.To(done, "read")
		c.Route(func(string, wf.Ctx) wf.Edge[fuzzSpin] { return end }, end)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 3, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}}})
	if got.Finding == nil || got.Finding.Rule != "fixture-gap" || !strings.Contains(got.Finding.Why, "missing.txt") || got.End != "finding" {
		t.Fatalf("missing world value passed: %s", got)
	}
}

// A stop decided by a computed state takes its cause from upstream: a named
// agent failure routed into it explains it, an accepted agent output does not.
func TestFuzzComputedStopTakesUpstreamCause(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("upstream")
		a := wf.NewAgent[fuzzAgent, bool](d)
		record := wf.NewCompute[fuzzSpin, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(record, "attempt failed")))
		ok, bad := a.To(done, "accepted"), a.To(record, "unexpected rejection")
		a.Route(func(v bool, _ wf.Ctx) wf.Edge[fuzzAgent] {
			if v {
				return ok
			}
			return bad
		}, ok, bad)
		end := record.To(stop, "recorded")
		record.Route(func(bool, wf.Ctx) wf.Edge[fuzzSpin] { return end }, end)
		return wftest.New(d)
	}
	failed := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 1, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzTransient}}})
	if failed.Finding != nil || failed.Status.Terminal != "fuzzStop" {
		t.Fatalf("exhausted agent did not explain the computed stop: %s", failed)
	}
	silent := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 1, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`false`)}}})
	if silent.Finding == nil || silent.Finding.Site() != `ends-silent upstream/fuzzSpin edge "recorded" -> fuzzStop` {
		t.Fatalf("accepted output excused a computed stop: %s", silent)
	}
}

// An accepted output that reaches an unresolved placeholder is a definition
// bug the agent exposed, never a named stop.
func TestFuzzDefinitionErrorIsAFinding(t *testing.T) {
	type out struct {
		ID string `json:"id"`
	}
	factory := func() *wftest.Sim {
		d := wf.New("placeholder")
		a := wf.NewAgent[fuzzAgent, out](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(done, "failed")))
		edge := a.To(done, "named").With(git.WorktreeAdd(wf.KeyOf("p", "{nosuch}"), "{nosuch}", "b", "c"))
		a.Route(func(out, wf.Ctx) wf.Edge[fuzzAgent] { return edge }, edge)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 2, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`{"id":""}`)}}})
	if got.Finding == nil || got.Finding.Rule != "definition-error" || got.Finding.State != "placeholder/fuzzAgent" {
		t.Fatalf("definition error passed: %s", got)
	}
}

// A child halted on a named failure, routed by its parent to a stop, is a
// causal named failure, not a silent one. The child's state opts out of the
// recovery offer (NoRecovery), so its blocked failure with no route halts
// it at once instead of going to the coordinator first.
func TestFuzzHaltedChildExplainsParentStop(t *testing.T) {
	factory := func() *wftest.Sim {
		child := wf.New("halting")
		a := wf.NewAgent[fuzzAgent, bool](child)
		done := wf.NewTerminal[fuzzDone](child, wf.Success)
		end := a.To(done, "done")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), end).NoRecovery())
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return end }, end)
		wf.Yield(done, func(wf.Ctx) struct{} { return struct{}{} })
		d := wf.New("parent")
		sub := wf.NewSub[fuzzSpin](d, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
		ok := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		sub.Exits(wf.OnExit[fuzzDone](sub.To(ok, "done")))
		d.OnFailure(wf.Blocked, stop)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 4, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzBlocked}}})
	if got.Finding != nil || got.Status.Terminal != "fuzzStop" {
		t.Fatalf("halted child: %s", got)
	}
}

// The driver answers human gates from their declared answers, so states
// behind a gate are reached, and a recorded answer replays.
func TestFuzzAnswersGates(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("gated")
		g := wf.NewGate[fuzzStop, fuzzVerdict](d, "go on?", wf.NewEnum(d, "answer", fuzzVerdict("yes"), fuzzVerdict("no")), wf.Reserved)
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		kept := wf.NewTerminal[fuzzSpin](d, wf.Stopped)
		g.Answers(wf.On(fuzzVerdict("yes"), g.To(a, "go on")), wf.On(fuzzVerdict("no"), g.To(kept, "declined")))
		end := a.To(done, "done")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(2), end))
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return end }, end)
		return wftest.New(d)
	}
	coverage := wftest.NewFuzzCoverage(factory, nil)
	for seed := int64(0); seed < 16; seed++ {
		got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: seed, MaxSteps: 16})
		if got.Finding != nil {
			t.Fatalf("seed %d: %s", seed, got)
		}
		if again := got.Replay(factory); again.Finding != nil || again.Status.Terminal != got.Status.Terminal {
			t.Fatalf("seed %d replay: %s", seed, again)
		}
		coverage.Add(got)
	}
	if coverage.Reached["fuzzAgent"] == 0 || coverage.Mix["answer"] == 0 || coverage.Ends["human gate"] == 0 {
		t.Fatalf("gates not answered:\n%s", coverage)
	}
}

// A read the world declares as failing for unknown values is the adapter's
// own failure, not a fixture gap.
func TestFuzzUnknownFailsIsNotAGap(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("unknown")
		obs := shell.Observe(shell.Run("cat", "missing.txt"))
		c := wf.NewCompute[fuzzSpin, string](d, func(ctx wf.Ctx) (string, error) {
			r, err := wf.Observed(ctx, obs)
			return r.Stdout, err
		}).Observes(obs)
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		next := c.To(a, "read")
		c.Route(func(string, wf.Ctx) wf.Edge[fuzzSpin] { return next }, next)
		end := a.To(done, "done")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), end))
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return end }, end)
		w := wftest.NewWorld()
		wftest.UnknownFails(w, "shell", "run", "no such file")
		return wftest.New(d, wftest.WithWorld(w))
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 1, MaxSteps: 8})
	if got.Finding != nil && got.Finding.Rule == "fixture-gap" {
		t.Fatalf("declared adapter failure counted as a gap: %s", got)
	}
}

// A prompt file the test's skills root lacks is a fixture gap, not a named
// failure of the definition.
func TestFuzzMissingPromptFileIsFixtureGap(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("noprompt")
		a := wf.NewAgent[fuzzAgent, bool](d)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		a.Spec(wf.PromptFile(filepath.Join(t.TempDir(), "absent.md")), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "attempt failed")).OnClass(wf.Blocked, a.To(stop, "blocked")))
		end := a.To(done, "done")
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return end }, end)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 1, MaxSteps: 8})
	if got.Finding == nil || got.Finding.Rule != "fixture-gap" {
		t.Fatalf("missing prompt passed: %s", got)
	}
}

// A stop reached through a spent MaxVisits budget is an exhausted declared
// budget, a cause a human accepts.
func TestFuzzSpentVisitBudgetExplainsStop(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("capped")
		a := wf.NewAgent[fuzzAgent, bool](d)
		b := wf.NewAgent[fuzzSpin, bool](d)
		report := wf.NewCompute[fuzzVerdict, bool](d, func(wf.Ctx) (bool, error) { return false, nil })
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		again, ok := a.To(b, "again"), a.To(done, "ok")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(2), ok))
		a.Route(func(v bool, _ wf.Ctx) wf.Edge[fuzzAgent] {
			if v {
				return ok
			}
			return again
		}, again, ok)
		back := b.To(a, "back")
		b.Spec(wf.PromptText("fix"), wf.Retry[fuzzSpin](wf.Const(2), back))
		b.Route(func(bool, wf.Ctx) wf.Edge[fuzzSpin] { return back }, back)
		wf.MaxVisits(wf.Const(1), b.To(report, "cap"))
		end := report.To(stop, "not ready")
		report.Route(func(bool, wf.Ctx) wf.Edge[fuzzVerdict] { return end }, end)
		return wftest.New(d)
	}
	no := json.RawMessage(`false`)
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 1, MaxSteps: 12, Prefix: []wftest.FuzzAction{
		{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: no}, {State: "fuzzSpin", Kind: wftest.FuzzSubmit, Output: no},
		{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: no}}})
	if got.Finding != nil || got.Status.Terminal != "fuzzStop" {
		t.Fatalf("spent budget: %s", got)
	}
}

// A compute refusal its retry repaired is not the cause of a later stop: the
// accepted retry, not the failure, chose the ordinary edge to Stopped.
func TestFuzzRepairedComputeRefusalDoesNotExplainStop(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("repaired")
		a := wf.NewAgent[fuzzAgent, bool](d)
		obs := shell.Observe(shell.Run("cat", "flaky.txt"))
		c := wf.NewCompute[fuzzSpin, string](d, func(ctx wf.Ctx) (string, error) {
			r, err := wf.Observed(ctx, obs)
			return r.Stdout, err
		}).Observes(obs)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		next := a.To(c, "read")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), next))
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return next }, next)
		c.Retry(wf.Retry[fuzzSpin](wf.Const(2), c.To(stop, "read failed twice")))
		end := c.To(stop, "read")
		c.Route(func(string, wf.Ctx) wf.Edge[fuzzSpin] { return end }, end)
		w := wftest.NewWorld()
		reads := 0
		wftest.OnRead(w, func(d wf.ObservationDesc) (json.RawMessage, bool) {
			if reads++; reads == 1 {
				return nil, false
			}
			raw, _ := json.Marshal(wf.RunResult{Stdout: "ok"})
			return raw, true
		})
		wftest.UnknownFails(w, "shell", "run", "transient read failure")
		return wftest.New(d, wftest.WithWorld(w))
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 1, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}}})
	refused := false
	for _, r := range got.Trace.Journal() {
		refused = refused || (r.Kind == wf.JournalRefused && r.State == "fuzzSpin")
	}
	if !refused {
		t.Fatalf("the compute never refused, so the repro does not hold: %s", got)
	}
	if got.Finding == nil || got.Finding.Site() != `ends-silent repaired/fuzzSpin edge "read" -> fuzzStop` {
		t.Fatalf("repaired refusal excused the stop: %s", got)
	}
}

type fuzzWork struct{}
type fuzzFan struct{}
type fuzzAfter struct{}

// A recovery answer can retire a unit handed out in the same call: give-up
// on one ended Map item cancels its sibling, whose hand-out the fuzz
// coordinator held, and the run moves on to After. The held hand-out is
// not acted on; the next step is After.
func TestFuzzRecoveryRetiresAHeldSibling(t *testing.T) {
	factory := func() *wftest.Sim {
		child := wf.New("item")
		work := wf.NewAgent[fuzzWork, bool](child)
		ok := wf.NewTerminal[fuzzDone](child, wf.Success)
		bad := wf.NewTerminal[fuzzStop](child, wf.Stopped)
		work.Spec(wf.PromptText("work"), wf.Retry[fuzzWork](wf.Const(2), work.To(bad, "attempt budget exhausted")))
		edge := work.To(ok, "accepted")
		work.Route(func(bool, wf.Ctx) wf.Edge[fuzzWork] { return edge }, edge)
		child.OnFailure(wf.Blocked, bad)
		wf.Yield(ok, func(wf.Ctx) bool { return true })
		wf.Yield(bad, func(wf.Ctx) bool { return false })

		d := wf.New("fan")
		m := wf.NewMap[fuzzFan, int, int, bool](d, wf.FromValues(1, 2), func(i int) int { return i }, wf.Typed[int, bool](child))
		after := wf.NewAgent[fuzzAfter, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		m.Parallel(2).OnSiblingFailure(wf.CancelSiblings).Join(wf.AllOK(), m.To(after, "met"), m.To(after, "partial"))
		after.Spec(wf.PromptText("after"), wf.Retry[fuzzAfter](wf.Const(2), after.To(done, "attempt budget exhausted")))
		fin := after.To(done, "accepted")
		after.Route(func(bool, wf.Ctx) wf.Edge[fuzzAfter] { return fin }, fin)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 7, MaxSteps: 12, Prefix: []wftest.FuzzAction{
		{State: "fuzzWork", Kind: wftest.FuzzEndedWithoutDone},
		{State: "fuzzWork", Kind: wftest.FuzzRecover, Output: json.RawMessage(`"give-up"`)},
		{State: "fuzzAfter", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)},
	}})
	if got.Finding != nil || got.Status.Outcome != wf.Success {
		t.Fatalf("held sibling: %s", got)
	}
}

// validate must give done's verdict on the same world: a check that keeps
// hidden state between calls (refusing every odd call) is reported once the
// child's fixes are spent and validate's refusal meets done's acceptance.
func TestFuzzReportsValidateThatDiffersFromDone(t *testing.T) {
	factory := func() *wftest.Sim {
		calls := 0
		d := wf.New("differs")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(2), a.To(done, "gave up")),
			wf.Checkf("stateful", func(bool, wf.Ctx) error {
				calls++
				if calls%2 == 1 {
					return errors.New("odd calls refuse")
				}
				return nil
			}))
		a.Then(a.To(done, "accepted"))
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 1, MaxSteps: 8, Prefix: []wftest.FuzzAction{
		{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)},
		{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)},
		{State: "fuzzAgent", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`true`)}}})
	if got.Finding == nil || got.Finding.Rule != "validate-differs" {
		t.Fatalf("a stateful check was not reported: %s", got)
	}
}
