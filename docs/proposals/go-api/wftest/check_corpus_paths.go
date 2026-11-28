package wftest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// checkCorpusPaths changes one state's output while preserving the progress
// context leading to it. Repeating that result also exercises bounded repair
// cycles; random schema prefixes often invalidate the IDs in a review corpus.
func checkCorpusPaths(factory func() *Sim, cfg CheckConfig, record func(*FuzzResult)) int {
	d := factory().def
	runs := 0
	var walk func(wf.DefView)
	walk = func(view wf.DefView) {
		for _, state := range view.States {
			if state.Kind != "agent" {
				continue
			}
			key := strings.TrimPrefix(state.Path, d.Name()+"/")
			samples, ok := cfg.Valid[key]
			if !ok {
				samples = cfg.Valid[state.Name]
			}
			for i, sample := range samples {
				valid := make(map[string][]json.RawMessage, len(cfg.Progress)+1)
				for state, values := range cfg.Progress {
					valid[state] = values
				}
				valid[key] = []json.RawMessage{sample}
				if result := cfg.run(factory, fmt.Sprintf("corpus %s sample %d", key, i), FuzzConfig{MaxSteps: cfg.MaxSteps, Valid: valid, Progress: cfg.Progress, Artifacts: cfg.Artifacts, FailPercent: -1, SchemaPercent: -1}); result != nil {
					record(result)
					runs++
				}
			}
		}
		for _, child := range view.Children {
			walk(child)
		}
	}
	walk(wf.Inspect(d))
	return runs
}

// checkAgentPaths drives one failure class until the selected state's retry or
// recovery route fires, keeping preceding states on curated progress outputs.
func checkAgentPaths(factory func() *Sim, cfg CheckConfig, record func(*FuzzResult)) int {
	runs := 0
	for _, key := range agentStateKeys(factory().def) {
		for _, action := range []FuzzActionKind{FuzzRefuse, FuzzBlocked, FuzzTransient, FuzzEndedWithoutDone} {
			if result := cfg.run(factory, fmt.Sprintf("agent %s %s", key, action), FuzzConfig{MaxSteps: cfg.MaxSteps, Valid: cfg.Progress, Progress: cfg.Progress, Artifacts: cfg.Artifacts, FailPercent: -1, SchemaPercent: -1, AgentActions: map[string]FuzzActionKind{key: action}}); result != nil {
				record(result)
				runs++
			}
		}
	}
	return runs
}

// agentStateKeys lists the definition tree's agent states, keyed as
// FuzzConfig keys them (the path below the root definition), in
// declaration order.
func agentStateKeys(d *wf.Def) []string {
	var keys []string
	var walk func(wf.DefView)
	walk = func(view wf.DefView) {
		for _, state := range view.States {
			if state.Kind == "agent" {
				keys = append(keys, strings.TrimPrefix(state.Path, d.Name()+"/"))
			}
		}
		for _, child := range view.Children {
			walk(child)
		}
	}
	walk(wf.Inspect(d))
	return keys
}
