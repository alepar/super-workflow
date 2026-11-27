package wftest

import (
	"encoding/json"
	"strings"
)

// checkGatePreludePaths tries one isolated effect fault at a time after the
// ordinary progress path could not answer a gate. State is root-relative, like
// FuzzFault.State. A matching recorded answer action, not arrival at a
// human gate, discharges the obligation. Each declared effect target costs at
// most one deterministic run; the caller records coverage and findings.
func checkGatePreludePaths(factory func() *Sim, cfg CheckConfig, state, answer string, targets []FuzzFault, record func(*FuzzResult)) (runs int, answered bool) {
	valid := map[string][]json.RawMessage{}
	for key, values := range cfg.Valid {
		valid[key] = values
	}
	for key, values := range cfg.Progress {
		valid[key] = values
	}
	root := factory().def.Name()
	for _, prelude := range targets {
		if prelude.Adapter == "" {
			continue
		}
		result := cfg.run(factory, "gate "+state+" answer "+answer, FuzzConfig{MaxSteps: cfg.MaxSteps, Valid: valid, Progress: cfg.Progress, Artifacts: cfg.Artifacts, GateAnswers: map[string]string{state: answer}, FailPercent: -1, SchemaPercent: -1, PreludeFault: &prelude})
		if result == nil {
			break
		}
		record(result)
		runs++
		for _, action := range result.Script {
			if action.Kind != FuzzAnswer || strings.TrimPrefix(action.State, root+"/") != state {
				continue
			}
			var value string
			if json.Unmarshal(action.Output, &value) == nil && value == answer {
				return runs, true
			}
		}
	}
	return runs, false
}
