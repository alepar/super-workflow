package wftest_test

import (
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type deepFirst struct{}
type deepSecond struct{}
type deepGate struct{}
type deepDone struct{}
type deepStop struct{}

func TestCheckIsolatesPreludeFaultForDeepRecoveryGate(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("deep-recovery")
		first := wf.NewCommand[deepFirst](d, "first")
		second := wf.NewCommand[deepSecond](d, "second")
		gate := wf.NewGate[deepGate, string](d, "retain?", wf.NewEnum(d, "answer", "yes"), wf.Reserved)
		done := wf.NewTerminal[deepDone](d, wf.Success)
		stop := wf.NewTerminal[deepStop](d, wf.Stopped)
		next, halt := first.To(second, "continue"), first.To(stop, "early fault").Ends("early failure")
		first.Route(func(r wf.RunResult, _ wf.Ctx) wf.Edge[deepFirst] {
			if r.Exit != 0 {
				return halt
			}
			return next
		}, next, halt)
		pass, repair := second.To(done, "success"), second.To(gate, "needs human repair").Ends("requires person")
		second.Route(func(r wf.RunResult, _ wf.Ctx) wf.Edge[deepSecond] {
			if r.Exit != 0 {
				return repair
			}
			return pass
		}, pass, repair)
		gate.Answers(wf.On("yes", gate.To(stop, "keep").Ends(wf.Stopped, "retain work for recovery"))).Timeout(time.Second, gate.To(stop, "expired"))
		d.OnFailure(wf.Unavailable, stop)
		d.OnFailure(wf.Blocked, stop)
		return wftest.New(d)
	}
	got := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1, MaxSteps: 8}})
	if got.FaultSeeds == 0 {
		t.Fatalf("fixture failed lint: %s", got)
	}
	for _, fault := range got.UnreachedFaults {
		if fault == "deepGate timeout ." {
			t.Fatalf("deep timeout starved by early faults: %s", got)
		}
	}
}
