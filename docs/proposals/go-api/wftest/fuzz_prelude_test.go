package wftest_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type preludeStart struct{}
type preludeWork struct{}
type preludeRecovery struct{}
type preludeDone struct{}
type preludeStopped struct{}

func preludeFactory(retry bool) func() *wftest.Sim {
	return func() *wftest.Sim {
		d := wf.New("isolated-prelude")
		first := wf.NewCommand[preludeStart](d, "prepare")
		work := wf.NewCommand[preludeWork](d, "work")
		gate := wf.NewGate[preludeRecovery, string](d, "recover?", wf.NewEnum(d, "choice", "stop"), wf.Reserved)
		done := wf.NewTerminal[preludeDone](d, wf.Success)
		stop := wf.NewTerminal[preludeStopped](d, wf.Stopped)
		prepared, earlyFailure := first.To(work, "prepared"), first.To(stop, "preparation failed").Ends("preparation failed")
		first.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[preludeStart] {
			if result.Exit != 0 {
				return earlyFailure
			}
			return prepared
		}, prepared, earlyFailure)
		completed, recoverEdge := work.To(done, "completed"), work.To(gate, "work failed")
		if retry {
			again := work.To(work, "try again")
			work.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[preludeWork] {
				if result.Exit != 0 {
					return again
				}
				return recoverEdge
			}, again, recoverEdge)
		} else {
			work.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[preludeWork] {
				if result.Exit != 0 {
					return recoverEdge
				}
				return completed
			}, completed, recoverEdge)
		}
		gate.Answers(wf.On("stop", gate.To(stop, "human stop"))).Timeout(time.Minute, gate.To(done, "timeout repaired"))
		return wftest.New(d)
	}
}

func preludeEffects(t *testing.T, result *wftest.FuzzResult) []struct {
	State string
	Exit  int
} {
	t.Helper()
	var out []struct {
		State string
		Exit  int
	}
	for _, record := range result.Trace.Journal() {
		if record.Kind != wf.JournalEffectResult {
			continue
		}
		var data wf.EffectResultData
		if err := json.Unmarshal(record.Data, &data); err != nil {
			t.Fatal(err)
		}
		if !data.OK {
			t.Fatalf("unexpected unavailable/failed command: %+v", data)
		}
		var command wf.RunResult
		if err := json.Unmarshal(data.Result, &command); err != nil {
			t.Fatal(err)
		}
		out = append(out, struct {
			State string
			Exit  int
		}{record.State, command.Exit})
	}
	return out
}

func TestIsolatedPreludeReachesFailureGateTimeoutAndReplays(t *testing.T) {
	for _, retry := range []bool{false, true} {
		factory := preludeFactory(retry)
		cfg := wftest.FuzzConfig{Seed: 17, MaxSteps: 16, Fault: &wftest.FuzzFault{State: "preludeRecovery", Class: "timeout"}, PreludeFault: &wftest.FuzzFault{State: "preludeWork", Class: "effect-adverse", Adapter: "shell", Op: "run"}}
		result := wftest.Fuzz(factory, cfg)
		if !result.PreludeFaultInjected || !result.FaultInjected || result.Finding != nil || result.Status.Terminal != "preludeDone" {
			t.Fatalf("isolated prelude retry=%v: prelude=%v target=%v %s", retry, result.PreludeFaultInjected, result.FaultInjected, result)
		}
		want := []struct {
			State string
			Exit  int
		}{{"preludeStart", 0}, {"preludeWork", 1}}
		if retry {
			want = append(want, struct {
				State string
				Exit  int
			}{"preludeWork", 0})
		}
		if got := preludeEffects(t, result); !reflect.DeepEqual(got, want) {
			t.Fatalf("wrong fault position/count: got %v want %v", got, want)
		}
		if len(result.Script) != 1 || result.Script[0].Kind != wftest.FuzzTimeout {
			t.Fatalf("timeout suppressed by prelude good phase: %+v", result.Script)
		}
		replay := result.Replay(factory)
		if !replay.PreludeFaultInjected || !replay.FaultInjected || replay.Finding != nil || replay.Status.Terminal != result.Status.Terminal || !reflect.DeepEqual(preludeEffects(t, result), preludeEffects(t, replay)) {
			t.Fatalf("isolated prelude replay differs: %s", replay)
		}
	}
}
