package wftest

import (
	"encoding/json"
	"fmt"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// FuzzEffectSequence supplies finite structured results to actual applications
// of one effect kind at a root-relative qualified owning state. Once exhausted,
// the world's ordinary result applies. Checks and all-good phases consume none.
type FuzzEffectSequence struct {
	State, Adapter, Op string
	Results            []json.RawMessage
}

func (f *fuzzRun) setupEffectSequences(cfg FuzzConfig) func() {
	type target struct{ state, adapter, op string }
	samples := map[target][]json.RawMessage{}
	used := map[target]int{}
	for _, seq := range cfg.EffectResults {
		key := target{seq.State, seq.Adapter, seq.Op}
		label := seq.State + "/" + seq.Adapter + "." + seq.Op
		gap := func(reason string) { f.find("fixture-gap", label+": "+reason) }
		if _, exists := samples[key]; exists {
			gap("duplicate effect result target")
			return func() {}
		}
		state, exists := f.schemas[seq.State]
		known := false
		if exists {
			for _, kind := range state.EffectKinds() {
				if kind.Adapter == seq.Adapter && kind.Op == seq.Op {
					known = true
				}
			}
		}
		if !known {
			gap("unknown state or effect operation")
			return func() {}
		}
		if len(seq.Results) == 0 {
			gap("empty effect result sequence")
			return func() {}
		}
		for i, raw := range seq.Results {
			if !json.Valid(raw) {
				gap(fmt.Sprintf("result %d is invalid JSON", i+1))
				return func() {}
			}
		}
		samples[key] = seq.Results
	}
	w := f.sim.world
	w.fuzzResult = func(at wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, bool) {
		key := target{at.State, e.Adapter, e.Op}
		i := used[key]
		if i >= len(samples[key]) {
			return nil, false
		}
		raw := append(json.RawMessage(nil), samples[key][i]...)
		used[key]++
		// Structured data is not inherently adverse. Only the established command
		// failure convention supplies evidence for a declared stopped outcome.
		if e.Adapter == "shell" && e.Op == "run" {
			var result wf.RunResult
			if json.Unmarshal(raw, &result) == nil && result.Exit != 0 && w.fuzzInjected != nil {
				w.fuzzInjected()
			}
		}
		return raw, true
	}
	if len(cfg.EffectResults) > 0 && f.sim.runner != nil {
		switch f.sim.runner.(type) {
		case *fuzzEffectRunner, *fuzzEffectRunnerProber:
		default:
			f.sim.runner = newFuzzEffectRunner(f.sim.runner, w)
		}
	}
	return func() {
		for _, seq := range cfg.EffectResults {
			key := target{seq.State, seq.Adapter, seq.Op}
			if remaining := len(samples[key]) - used[key]; remaining > 0 {
				f.find("fixture-gap", fmt.Sprintf("%s/%s.%s: %d unconsumed effect results", seq.State, seq.Adapter, seq.Op, remaining))
				return
			}
		}
	}
}

// Called under the world lock, only after explicit fault selection.
func (w *World) nextFuzzResult(at wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, bool) {
	if w.fuzzGood || w.fuzzResult == nil {
		return nil, false
	}
	return w.fuzzResult(at, e)
}
