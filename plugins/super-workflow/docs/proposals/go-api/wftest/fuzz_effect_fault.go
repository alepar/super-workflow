package wftest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// nextFuzzFault runs under World.mu. Recovery takes effect for subsequent calls,
// including later effects within the same engine drive, without suppressing the
// fault selected for this invocation. The callback must not reenter World.
func (w *World) nextFuzzFault(at wf.EffectContext, effect wf.ExternalEffect) string {
	if w.fuzzGood || w.fuzzFault == nil {
		return ""
	}
	class := w.fuzzFault(at, effect)
	if class != "" && class != "effect-uncertain" && !w.fuzzPrelude && w.fuzzRecover != nil && w.fuzzRecover(at, class) {
		w.fuzzGood = true
	}
	return class
}

func effectFaultResult(class string, effect wf.ExternalEffect) (json.RawMessage, error) {
	switch class {
	case "effect-error":
		return nil, fmt.Errorf("fuzz %s.%s execution failed", effect.Adapter, effect.Op)
	case "effect-unavailable":
		return nil, fmt.Errorf("%w: fuzz %s.%s unavailable", wf.ErrUnavailable, effect.Adapter, effect.Op)
	case "effect-timeout":
		if effect.Adapter == "shell" && effect.Op == "run" {
			if timeout, _ := effect.Args["Timeout"].(string); timeout != "" {
				return json.Marshal(wf.RunResult{Exit: 124, Stderr: "fuzz command timed out"})
			}
			return nil, fmt.Errorf("fuzz shell command timed out: %w", context.DeadlineExceeded)
		}
		return nil, fmt.Errorf("%w: fuzz %s.%s timed out: %w", wf.ErrUnavailable, effect.Adapter, effect.Op, context.DeadlineExceeded)
	case "effect-adverse":
		if effect.Adapter == "shell" && effect.Op == "run" {
			return json.Marshal(wf.RunResult{Exit: 1, Stderr: "fuzz command failed"})
		}
		return nil, fmt.Errorf("fuzz %s.%s failed", effect.Adapter, effect.Op)
	default:
		return nil, fmt.Errorf("unknown fuzz effect fault %q", class)
	}
}

// fuzzUncertain is a single unresolved invocation, selected by state before its
// intent is interrupted. Check is keyed by effect identity, never merely op.
type fuzzUncertain struct {
	at      wf.EffectContext
	key     string
	prelude bool
}

func (w *World) finishUncertain() {
	pending := w.fuzzUncertain
	w.fuzzUncertain = nil
	if pending != nil && !pending.prelude && w.fuzzRecover != nil && w.fuzzRecover(pending.at, "effect-uncertain") {
		w.fuzzGood = true
	}
}
