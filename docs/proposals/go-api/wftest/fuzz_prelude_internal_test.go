package wftest

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestPreludeDoesNotConsumePrimaryOfSameStateAndClass(t *testing.T) {
	cfg := FuzzConfig{MaxSteps: 8, Fault: &FuzzFault{State: "Work", Adapter: "tracker", Op: "primary", Class: "effect-adverse"}, PreludeFault: &FuzzFault{State: "Work", Adapter: "tracker", Op: "prelude", Class: "effect-adverse"}}
	f := &fuzzRun{sim: New(wf.New("root")), result: &FuzzResult{config: cfg}, schemas: map[string]wf.StateView{"Work": {Kind: "agent"}}}
	f.setupPhases(cfg)
	at := wf.EffectContext{State: "Work"}
	if _, err := f.sim.world.ApplyAt("", at, wf.ExternalEffect{Adapter: "tracker", Op: "ordinary"}); err != nil {
		t.Fatalf("unrelated effect failed: %v", err)
	}
	if _, err := f.sim.world.ApplyAt("", at, wf.ExternalEffect{Adapter: "tracker", Op: "prelude"}); err == nil {
		t.Fatal("prelude did not fire")
	}
	if !f.result.PreludeFaultInjected || f.result.FaultInjected || f.good {
		t.Fatalf("prelude consumed same-state primary: %+v", f.result)
	}
	if _, err := f.sim.world.ApplyAt("", at, wf.ExternalEffect{Adapter: "tracker", Op: "prelude"}); err != nil {
		t.Fatalf("prelude fired twice: %v", err)
	}
	if _, err := f.sim.world.ApplyAt("", at, wf.ExternalEffect{Adapter: "tracker", Op: "primary"}); err == nil {
		t.Fatal("primary was suppressed")
	}
	if !f.result.FaultInjected || !f.good {
		t.Fatal("primary did not start all-good phase")
	}
}
