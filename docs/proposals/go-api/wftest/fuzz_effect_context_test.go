package wftest

import (
	"errors"
	"sync"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestFuzzEffectSwitchUsesOwningInstanceContext(t *testing.T) {
	w := NewWorld()
	f := &fuzzRun{sim: New(wf.New("root"), WithWorld(w)), result: &FuzzResult{}, schemas: map[string]wf.StateView{"child/Work": {}, "other/Work": {}}}
	f.setupPhases(FuzzConfig{OutcomeSwitches: true, MaxSteps: 10})
	f.result.Switches["child/Work/effect-unavailable"] = true
	f.result.Switches["other/Work/effect-unavailable"] = false
	effect := wf.ExternalEffect{Adapter: "test", Op: "write"}
	// Calls from different instances must use their own qualified state switch.
	_, err := w.ApplyAt("", wf.EffectContext{Instance: "child#1", State: "child/Work"}, effect)
	if !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("resumed child effect missed its switch: %v", err)
	}
	_, err = w.ApplyAt("", wf.EffectContext{Instance: "other", State: "other/Work"}, effect)
	if err != nil {
		t.Fatalf("other instance inherited child's switch: %v", err)
	}
}

func TestWorldLivenessModeConcurrentWithEffects(t *testing.T) {
	w := NewWorld()
	w.fuzzFault = func(wf.EffectContext, wf.ExternalEffect) string { return "effect-unavailable" }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			w.setFuzzGood(i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = w.ApplyAt("", wf.EffectContext{State: "Work"}, wf.ExternalEffect{Adapter: "test", Op: "write"})
		}
	}()
	wg.Wait()
	w.setFuzzGood(true)
	if _, err := w.Apply("", wf.ExternalEffect{Adapter: "test", Op: "write"}); err != nil {
		t.Fatalf("good phase still fails: %v", err)
	}
}
