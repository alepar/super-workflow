package wftest_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type boundFan struct{}
type boundWork struct{}
type boundDone struct{}

func boundFanFactory(n int, spawnCycle bool) func() *wftest.Sim {
	return func() *wftest.Sim {
		child := wf.New("bound-child")
		if !spawnCycle {
			a := wf.NewAgent[boundWork, bool](child)
			done := wf.NewTerminal[boundDone](child, wf.Success)
			a.Spec(wf.PromptText("work"), wf.Retry[boundWork](wf.Const(1), a.To(done, "failed")))
			again, finish := a.To(a, "again"), a.To(done, "finished")
			a.Route(func(v bool, _ wf.Ctx) wf.Edge[boundWork] {
				if v {
					return finish
				}
				return again
			}, again, finish)
			wf.Yield(done, func(wf.Ctx) bool { return true })
		} else {
			done := wf.NewTerminal[boundDone](child, wf.Success)
			wf.Yield(done, func(wf.Ctx) bool { return true })
		}
		d := wf.New("bound-fan")
		items := make([]int, n)
		for i := range items {
			items[i] = i
		}
		m := wf.NewMap[boundFan, int, string, bool](d, wf.FromValues(items...), func(i int) string { return fmt.Sprint(i) }, wf.Typed[int, bool](child))
		end := wf.NewTerminal[boundDone](d, wf.Success)
		target := m.To(end, "joined")
		if spawnCycle {
			target = m.To(m, "spawn again")
		}
		m.Parallel(1).Join(wf.AllOK(), target, target)
		return wftest.New(d)
	}
}

func TestFuzzBoundsAllowProgressingFanout(t *testing.T) {
	actions := make([]wftest.FuzzAction, 0, 161)
	for i := 0; i < 23; i++ {
		for j := 0; j < 7; j++ {
			v := `false`
			if j == 6 {
				v = `true`
			}
			actions = append(actions, wftest.FuzzAction{Kind: wftest.FuzzSubmit, Output: json.RawMessage(v)})
		}
	}
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			cfg := wftest.FuzzConfig{MaxSteps: 128, Prefix: actions}
			if live {
				cfg.Liveness = true
				cfg.LivenessAfter = 0
				cfg.Progress = map[string][]json.RawMessage{"boundWork": {json.RawMessage(`true`)}}
				cfg.MaxSteps = 8
				cfg.LivenessSteps = 8
			}
			got := wftest.Fuzz(boundFanFactory(23, false), cfg)
			if got.Finding != nil || got.Status.Outcome != wf.Success {
				t.Fatalf("progressing fanout: finding=%+v agents=%d", got.Finding, got.Agents)
			}
			if !live && got.Agents <= 128 {
				t.Fatalf("fixture did not exceed old bound: %d", got.Agents)
			}
		})
	}
}

func TestFuzzBoundsSiblingProgressDoesNotFundLoop(t *testing.T) {
	actions := make([]wftest.FuzzAction, 100)
	for i := range actions {
		v := `false`
		if i < 22 {
			v = `true`
		}
		actions[i] = wftest.FuzzAction{Kind: wftest.FuzzSubmit, Output: json.RawMessage(v)}
	}
	got := wftest.Fuzz(boundFanFactory(23, false), wftest.FuzzConfig{MaxSteps: 8, Prefix: actions})
	if got.Finding == nil || got.Finding.Rule != "loops-unbounded" {
		t.Fatalf("loop escaped: %s", got)
	}
	if got.Agents != 30 {
		t.Fatalf("want 22 completed siblings and 8 looping turns, got %d: %+v", got.Agents, got.Finding)
	}
}

func TestFuzzBoundsRepeatedSpawning(t *testing.T) {
	got := wftest.Fuzz(boundFanFactory(1, true), wftest.FuzzConfig{MaxSteps: 8})
	if got.Finding == nil || got.Finding.Rule != "loops-unbounded" {
		t.Fatalf("spawn loop escaped: %s", got)
	}
}

// A compute loop runs inside one engine call. Earlier sibling settlements
// must not buy that instance additional journal work.
func TestFuzzBoundsSiblingProgressDoesNotFundComputeLoop(t *testing.T) {
	transitions := func(n int) int {
		factory := func() *wftest.Sim {
			child := wf.New("compute-child")
			spin := wf.NewCompute[boundWork, bool](child, func(c wf.Ctx) (bool, error) { return wf.InputOf[int](c) == n-1, nil })
			done := wf.NewTerminal[boundDone](child, wf.Success)
			again, finish := spin.To(spin, "again"), spin.To(done, "finished")
			spin.Route(func(loop bool, _ wf.Ctx) wf.Edge[boundWork] {
				if loop {
					return again
				}
				return finish
			}, again, finish)
			wf.Yield(done, func(wf.Ctx) bool { return true })
			d := wf.New("compute-fan")
			items := make([]int, n)
			for i := range items {
				items[i] = i
			}
			m := wf.NewMap[boundFan, int, string, bool](d, wf.FromValues(items...), func(i int) string { return fmt.Sprint(i) }, wf.Typed[int, bool](child))
			end := wf.NewTerminal[boundDone](d, wf.Success)
			m.Parallel(1).Join(wf.AllOK(), m.To(end, "joined"), m.To(end, "failed"))
			return wftest.New(d)
		}
		got := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 8})
		if got.Finding == nil || got.Finding.Rule != "loops-unbounded" {
			t.Fatalf("compute loop escaped: %+v", got.Finding)
		}
		count := 0
		for _, r := range got.Trace.Journal() {
			if r.Kind == wf.JournalTransition && r.Instance == fmt.Sprintf("boundFan[%d]", n-1) {
				count++
			}
		}
		return count
	}
	alone, withSiblings := transitions(1), transitions(23)
	if alone == 0 || withSiblings != alone {
		t.Fatalf("siblings changed looping instance budget: alone=%d siblings=%d", alone, withSiblings)
	}
}
