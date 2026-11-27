package wftest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// checkRepeatedFaultPaths exercises exhaustion paths under a sustained fault.
// These are coverage runs, with no all-good phase or liveness claim. Bounds and
// natural gates/terminals remain the real engine's outcomes.
func checkRepeatedFaultPaths(factory func() *Sim, cfg CheckConfig, record func(*FuzzResult)) int {
	valid := map[string][]json.RawMessage{}
	for key, values := range cfg.Valid {
		valid[key] = values
	}
	for key, values := range cfg.Progress {
		valid[key] = values
	}
	d := factory().def
	runs := 0
	run := func(state wf.StateView, kind wf.EffectKind, class string) {
		fault := FuzzFault{State: strings.TrimPrefix(state.Path, d.Name()+"/"), Class: class, Adapter: kind.Adapter, Op: kind.Op}
		result := cfg.run(factory, fmt.Sprintf("repeated fault %s %s %s.%s", fault.State, class, kind.Adapter, kind.Op), FuzzConfig{MaxSteps: cfg.MaxSteps, Valid: valid, Progress: cfg.Progress, Artifacts: cfg.Artifacts, FailPercent: -1, SchemaPercent: -1, PreludeFault: &fault, RepeatPrelude: true})
		if result == nil {
			return
		}
		record(result)
		runs++
	}
	var visit func(wf.DefView)
	visit = func(view wf.DefView) {
		for _, state := range view.States {
			for _, effect := range state.EffectKinds() {
				for _, class := range []string{"effect-error", "effect-adverse", "effect-unavailable"} {
					run(state, effect, class)
				}
			}
			for _, observation := range state.ObservationKinds() {
				for _, class := range []string{"observation-unavailable", "observation-error"} {
					run(state, observation, class)
				}
			}
		}
		for _, child := range view.Children {
			visit(child)
		}
	}
	visit(wf.Inspect(d))
	return runs
}
