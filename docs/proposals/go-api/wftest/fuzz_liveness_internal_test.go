package wftest

import (
	"errors"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"testing"
)

func TestUnavailableSwitchDoesNotApply(t *testing.T) {
	world := NewWorld()
	calls := 0
	OnApply(world, func(wf.ExternalEffect) { calls++ })
	world.fuzzFault = func(wf.EffectContext, wf.ExternalEffect) string { return "effect-unavailable" }
	_, err := world.Apply("", wf.ExternalEffect{Adapter: "x", Op: "write"})
	if !errors.Is(err, wf.ErrUnavailable) || calls != 0 || len(Applied(world)) != 0 {
		t.Fatalf("unavailable effect applied: err=%v calls=%d applied=%v", err, calls, Applied(world))
	}
}

type switchCommand struct{}
type switchDone struct{}

func TestOutcomeSwitchEffectDoesNotReenterJournal(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("effect-switch")
		command := wf.NewCommand[switchCommand](d, "true")
		done := wf.NewTerminal[switchDone](d, wf.Success)
		command.Then(command.To(done, "done"))
		return New(d)
	}
	got := Fuzz(factory, FuzzConfig{Seed: 0, MaxSteps: 3, OutcomeSwitches: true})
	if got.Trace == nil {
		t.Fatal("effect switch must return a trace")
	}
}

// A held unit or an unproven manual instruction is not a human boundary.
func TestLivenessRejectsExternalStops(t *testing.T) {
	for _, kind := range []wf.InstructionKind{wf.InstructionWaiting, wf.InstructionManualVerification} {
		f := &fuzzRun{good: true, sim: New(wf.New("stop")), result: &FuzzResult{config: FuzzConfig{Liveness: true}}}
		f.externalStop(wf.Instruction{Kind: kind, State: "Held"})
		if f.result.Finding == nil || f.result.Finding.Rule != "liveness" {
			t.Fatalf("%s passed liveness: %+v", kind, f.result)
		}
	}
}
