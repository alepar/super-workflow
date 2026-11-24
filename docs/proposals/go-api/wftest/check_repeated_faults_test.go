package wftest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type repeatedPrepare struct{}
type repeatedWork struct{}
type repeatedDone struct{}
type repeatedStop struct{}
type repeatedRepair struct{}

func repeatedCommandFactory() *Sim {
	d := wf.New("repeat-command")
	prepare := wf.NewCommand[repeatedPrepare](d, "prepare")
	work := wf.NewCommand[repeatedWork](d, "work")
	done := wf.NewTerminal[repeatedDone](d, wf.Success)
	stop := wf.NewTerminal[repeatedStop](d, wf.Stopped)
	prepare.Then(prepare.To(work, "prepared"))
	prepare.Retry(wf.Retry[repeatedPrepare](wf.Const(2), prepare.To(stop, "prepare exhausted").Ends("prepare failed")))
	work.Retry(wf.Retry[repeatedWork](wf.Const(2), work.To(stop, "work exhausted").Ends("work failed")))
	work.Then(work.To(done, "done"))
	return New(d)
}

func TestRepeatedEffectErrorsExhaustCommandRetry(t *testing.T) {
	result := Fuzz(repeatedCommandFactory, FuzzConfig{MaxSteps: 8, RepeatPrelude: true, PreludeFault: &FuzzFault{State: "repeatedWork", Class: "effect-error", Adapter: "shell", Op: "run"}})
	if !result.PreludeFaultInjected || result.FaultInjected || result.Finding != nil || result.Status.Terminal != "repeatedStop" {
		t.Fatal(result)
	}
	failed, prepared := 0, 0
	for _, record := range result.Trace.Journal() {
		if record.Kind != wf.JournalEffectResult {
			continue
		}
		var data wf.EffectResultData
		if err := json.Unmarshal(record.Data, &data); err != nil {
			t.Fatal(err)
		}
		if record.State == "repeatedPrepare" && data.OK {
			prepared++
		}
		if record.State == "repeatedWork" && !data.OK {
			failed++
		}
	}
	if failed != 2 || prepared != 1 {
		t.Fatalf("failed=%d prepared=%d", failed, prepared)
	}
	replay := result.Replay(repeatedCommandFactory)
	if replay.Status.Terminal != result.Status.Terminal || replay.Finding != nil {
		t.Fatal(replay)
	}
}

func TestRepeatedEdgeAdverseReachesVisitCap(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("repeat-edge")
		work := wf.NewCompute[repeatedWork, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		repair := wf.NewCompute[repeatedRepair, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		done := wf.NewTerminal[repeatedDone](d, wf.Success)
		stop := wf.NewTerminal[repeatedStop](d, wf.Stopped)
		merge := work.To(done, "merged").With(wf.ExternalEffect{Adapter: "git", Op: "merge", Key: wf.KeyOf("merge")})
		work.Then(merge)
		d.OnFailure(wf.Blocked, repair)
		repair.Then(repair.To(work, "retry merge"))
		wf.MaxVisits(wf.Const(2), work.To(stop, "resolution cap").Ends("conflicts remain"))
		return New(d)
	}
	result := Fuzz(factory, FuzzConfig{MaxSteps: 8, RepeatPrelude: true, PreludeFault: &FuzzFault{State: "repeatedWork", Class: "effect-adverse", Adapter: "git", Op: "merge"}})
	if result.Finding != nil || result.Status.Terminal != "repeatedStop" {
		t.Fatal(result)
	}
	failed := 0
	for _, record := range result.Trace.Journal() {
		if record.Kind == wf.JournalEffectResult {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("edge failure attempts=%d: %s", failed, result)
	}
}

func TestRepeatedEffectHelperEnumeratesEachStateOperation(t *testing.T) {
	var results []*FuzzResult
	runs := checkRepeatedFaultPaths(repeatedCommandFactory, CheckConfig{CalibrationConfig: CalibrationConfig{MaxSteps: 8}}, func(result *FuzzResult) { results = append(results, result) })
	expected := []string{"repeatedPrepare/effect-error", "repeatedPrepare/effect-adverse", "repeatedPrepare/effect-unavailable", "repeatedWork/effect-error", "repeatedWork/effect-adverse", "repeatedWork/effect-unavailable"}
	if runs != len(expected) || len(results) != len(expected) {
		t.Fatalf("runs=%d results=%d", runs, len(results))
	}
	seen := map[string]bool{}
	for _, result := range results {
		fault := result.config.PreludeFault
		seen[fault.State+"/"+fault.Class] = true
		if result.config.Liveness || !result.config.RepeatPrelude {
			t.Fatal("coverage run enabled recovery phase or failed to repeat")
		}
	}
	for _, key := range expected {
		if !seen[key] {
			t.Fatalf("missing %s", key)
		}
	}
}

func TestEffectErrorIsAnOrdinaryExecutionError(t *testing.T) {
	for _, effect := range []wf.ExternalEffect{{Adapter: "shell", Op: "run"}, {Adapter: "git", Op: "merge"}} {
		result, err := effectFaultResult("effect-error", effect)
		if len(result) != 0 || err == nil || errors.Is(err, wf.ErrUnavailable) || strings.Contains(err.Error(), "unknown fuzz") {
			t.Fatalf("effect-error result=%s err=%v", result, err)
		}
	}
}

func TestRepeatedEffectWithoutCapReportsRealBound(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("repeat-unbounded")
		work := wf.NewCommand[repeatedWork](d, "work")
		done := wf.NewTerminal[repeatedDone](d, wf.Success)
		good, bad := work.To(done, "done"), work.To(work, "retry")
		work.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[repeatedWork] {
			if result.Exit != 0 {
				return bad
			}
			return good
		}, good, bad)
		return New(d)
	}
	result := Fuzz(factory, FuzzConfig{MaxSteps: 2, RepeatPrelude: true, PreludeFault: &FuzzFault{State: "repeatedWork", Class: "effect-adverse", Adapter: "shell", Op: "run"}})
	if !result.PreludeFaultInjected || result.Finding == nil || result.Finding.Rule != "loops-unbounded" || result.Status.Terminal != "" {
		t.Fatal(result)
	}
}

func repeatedObservationFactory() *Sim {
	d := wf.New("repeat-observation")
	obs := wf.Observe[bool]("test", "read", nil)
	prepare := wf.NewCompute[repeatedPrepare, bool](d, func(c wf.Ctx) (bool, error) { return wf.Observed(c, obs) }).Observes(obs)
	work := wf.NewCompute[repeatedWork, bool](d, func(c wf.Ctx) (bool, error) { return wf.Observed(c, obs) }).Observes(obs)
	done := wf.NewTerminal[repeatedDone](d, wf.Success)
	stop := wf.NewTerminal[repeatedStop](d, wf.Success)
	prepare.Then(prepare.To(work, "prepared"))
	work.Then(work.To(done, "done"))
	d.OnFailure(wf.Unavailable, stop)
	d.OnFailure(wf.Blocked, stop)
	world := NewWorld()
	Set(world, obs, true)
	return New(d, WithWorld(world))
}

func TestRepeatedObservationUnavailableExhaustsBackoffToDefault(t *testing.T) {
	result := Fuzz(repeatedObservationFactory, FuzzConfig{MaxSteps: 12, RepeatPrelude: true, PreludeFault: &FuzzFault{State: "repeatedWork", Class: "observation-unavailable", Adapter: "test", Op: "read"}})
	if result.Finding != nil || result.Status.Terminal != "repeatedStop" || !result.PreludeFaultInjected {
		t.Fatal(result)
	}
	backoffs, unavailable, prepared := 0, 0, 0
	for _, record := range result.Trace.Journal() {
		if record.Kind == wf.JournalBackoffScheduled && record.State == "repeatedWork" {
			backoffs++
		}
		if record.Kind == wf.JournalObserved {
			var data wf.ObservedData
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			if record.State == "repeatedWork" && data.Unavailable {
				unavailable++
			}
			if record.State == "repeatedPrepare" && data.Error == "" {
				prepared++
			}
		}
	}
	if backoffs != 5 || unavailable != 6 || prepared != 1 {
		t.Fatalf("backoffs=%d unavailable=%d prepared=%d", backoffs, unavailable, prepared)
	}
}

func TestRepeatedHelperIncludesObservationFailures(t *testing.T) {
	seen := map[string]bool{}
	runs := checkRepeatedFaultPaths(repeatedObservationFactory, CheckConfig{CalibrationConfig: CalibrationConfig{MaxSteps: 12}}, func(result *FuzzResult) {
		fault := result.config.PreludeFault
		seen[fault.State+"/"+fault.Class] = true
	})
	expected := []string{"repeatedPrepare/observation-error", "repeatedPrepare/observation-unavailable", "repeatedWork/observation-error", "repeatedWork/observation-unavailable"}
	if runs != len(expected) || len(seen) != len(expected) {
		t.Fatalf("runs=%d seen=%v", runs, seen)
	}
	for _, key := range expected {
		if !seen[key] {
			t.Fatalf("missing %s", key)
		}
	}
}
