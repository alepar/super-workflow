package wftest

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"math/rand"
	"slices"
	"strings"
)

func (f *fuzzRun) setupPhases(cfg FuzzConfig) {
	f.sim.world.fuzzInjected = func() { f.result.adversarialInjected = true }
	r := rand.New(rand.NewSource(cfg.Seed ^ 0x1a1e))
	f.result.LivenessStart = cfg.LivenessAfter
	if cfg.LivenessAfter < 0 {
		f.result.LivenessStart = rand.New(rand.NewSource(cfg.Seed ^ 0xc4ec)).Intn(cfg.MaxSteps)
	}
	if !cfg.OutcomeSwitches && cfg.Fault == nil && cfg.PreludeFault == nil {
		return
	}
	f.result.Switches = map[string]bool{}
	keys := make([]string, 0, len(f.schemas))
	for key := range f.schemas {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		for _, class := range []string{"submit", "refuse", "transient", "blocked", "ended-without-done", "effect-unavailable", "effect-timeout", "effect-adverse"} {
			f.result.Switches[key+"/"+class] = r.Intn(2) == 0
		}
	}
	for _, key := range keys {
		state := f.schemas[key]
		if len(state.ObservationKinds()) > 0 {
			for _, class := range []string{"observation-unavailable", "observation-error"} {
				f.result.Switches[key+"/"+class] = r.Intn(2) == 0
			}
		}
		if len(state.EffectKinds()) > 0 {
			f.result.Switches[key+"/effect-uncertain"] = r.Intn(2) == 0
		}
		if state.Kind == "gate" {
			for _, edge := range state.Edges {
				if edge.Timeout > 0 {
					f.result.Switches[key+"/timeout"] = r.Intn(2) == 0
					break
				}
			}
		}
	}
	if !cfg.OutcomeSwitches {
		f.result.Switches = map[string]bool{}
		if cfg.Fault != nil {
			f.result.Switches[cfg.Fault.State+"/"+cfg.Fault.Class] = true
		}
		if cfg.PreludeFault != nil {
			f.result.Switches[cfg.PreludeFault.State+"/"+cfg.PreludeFault.Class] = true
		}
	}
	w := f.sim.world
	if cfg.Fault != nil {
		f.sim.world.fuzzRecover = func(at wf.EffectContext, class string) bool {
			// Prelude and primary may share a state/class but target different ops.
			// Only the primary injection starts the all-good recovery phase.
			return f.recoveredFault(at, class)
		}
	}
	counts := map[string]int{}
	targeted := false
	f.sim.world.fuzzFault = func(at wf.EffectContext, effect wf.ExternalEffect) string {
		w.fuzzPrelude = false
		if prelude := cfg.PreludeFault; prelude != nil && (cfg.RepeatPrelude || !f.result.PreludeFaultInjected) && strings.HasPrefix(prelude.Class, "effect-") && at.State == prelude.State && effect.Adapter == prelude.Adapter && effect.Op == prelude.Op {
			w.fuzzPrelude = true
			f.result.PreludeFaultInjected = true
			return f.effectFaultClass(at, prelude.Class)
		}
		if target := cfg.Fault; target != nil {
			if !targeted && strings.HasPrefix(target.Class, "effect-") && at.State == target.State && effect.Adapter == target.Adapter && effect.Op == target.Op {
				targeted = true
				return f.effectFaultClass(at, target.Class)
			}
		}
		if !cfg.OutcomeSwitches {
			return ""
		}
		var enabled []string
		for _, class := range []string{"effect-unavailable", "effect-timeout", "effect-adverse", "effect-uncertain"} {
			if f.result.Switches[at.State+"/"+class] {
				enabled = append(enabled, class)
			}
		}
		if len(enabled) == 0 {
			return ""
		}
		key := at.State + "\x00" + effect.Adapter + "\x00" + effect.Op
		class := enabled[counts[key]%len(enabled)]
		counts[key]++
		return f.effectFaultClass(at, class)
	}
	observationCounts := map[string]int{}
	observationTargeted := false
	w.fuzzObservation = func(at wf.EffectContext, d wf.ObservationDesc) string {
		w.fuzzPrelude = false
		isObservation := func(class string) bool { return class == "observation-unavailable" || class == "observation-error" }
		// Agents' declared check reads are first observed at issue. Inject at check
		// so the selected error is actually consumed by the acceptance closure.
		if state := f.schemas[at.State]; state.Kind == "agent" && at.Purpose == "issue" {
			return ""
		}
		if prelude := cfg.PreludeFault; prelude != nil && isObservation(prelude.Class) && (cfg.RepeatPrelude || !f.result.PreludeFaultInjected) && at.State == prelude.State && d.Adapter == prelude.Adapter && d.Op == prelude.Op {
			w.fuzzPrelude = true
			f.result.PreludeFaultInjected = true
			return prelude.Class
		}
		if target := cfg.Fault; target != nil && isObservation(target.Class) && !observationTargeted && at.State == target.State && d.Adapter == target.Adapter && d.Op == target.Op {
			observationTargeted = true
			return target.Class
		}
		if !cfg.OutcomeSwitches {
			return ""
		}
		var enabled []string
		for _, class := range []string{"observation-unavailable", "observation-error"} {
			if f.result.Switches[at.State+"/"+class] {
				enabled = append(enabled, class)
			}
		}
		if len(enabled) == 0 {
			return ""
		}
		key := at.State + "\x00" + d.Adapter + "\x00" + d.Op
		class := enabled[observationCounts[key]%len(enabled)]
		observationCounts[key]++
		return class
	}

	if f.sim.runner != nil {
		f.sim.runner = newFuzzEffectRunner(f.sim.runner, f.sim.world)
	}
}

func (f *fuzzRun) effectFaultClass(at wf.EffectContext, class string) string {
	if class == "effect-uncertain" && f.schemas[at.State].Detached > 0 && f.sim.runner != nil {
		f.last = wf.Instruction{State: at.State}
		f.find("fixture-gap", "detached effect-uncertain requires launch adoption reconciliation, which this fault generator does not provide")
		return ""
	}
	return class
}
