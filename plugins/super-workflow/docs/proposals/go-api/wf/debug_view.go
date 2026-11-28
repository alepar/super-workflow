package wf

import (
	"encoding/json"
	"fmt"
	"strings"
)

// journalRecordView preserves the machine record fields and adds readable
// kind/class names. The on-disk journal encoding remains unchanged.
func journalRecordView(r JournalRecord) any {
	class := ""
	if r.Class != 0 {
		class = r.Class.String()
	}
	return struct {
		JournalRecord
		KindName  string
		ClassName string `json:",omitempty"`
	}{r, r.Kind.String(), class}
}

// Explain reads a unit's journal without driving the graph. It shows its
// incoming transition, chosen route and deciding checks/effects, including
// refused submissions. Retired units remain explainable.
func (e *Engine) Explain(run RunID, name string) (string, error) {
	var out string
	err := e.withReadOnly(run, func(c *cycle) error {
		ref, ok := c.st.names[name]
		if !ok {
			return fmt.Errorf("%w: explain: no unit %q", ErrRejected, name)
		}
		start := -1
		for i, r := range c.recs {
			if r.Kind == JournalHandedOut {
				var d HandedOutData
				if json.Unmarshal(r.Data, &d) == nil && d.Name == name {
					start = i
					break
				}
			}
		}
		if start < 0 {
			return fmt.Errorf("%w: explain: no hand-out for %q", ErrRejected, name)
		}
		// Include the issue that preceded this unit's hand-out, and stop before
		// the next issue in the same instance so later visits cannot rewrite why.
		lo := start
		for lo > 0 {
			lo--
			if r := c.recs[lo]; r.Instance == ref.Instance && r.Kind == JournalInstructionIssued {
				break
			}
		}
		hi := len(c.recs)
		for i := start + 1; i < len(c.recs); i++ {
			r := c.recs[i]
			if r.Instance == ref.Instance && r.Kind == JournalInstructionIssued {
				hi = i
				break
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "unit %s (attempt %d)\n", name, ref.Attempt)
		for _, pd := range c.promptDeliveryStatus() {
			if pd.Unit == name {
				fmt.Fprintln(&b, pd.Coverage)
				break
			}
		}
		for i := lo - 1; i >= 0; i-- {
			r := c.recs[i]
			if r.Instance != ref.Instance || r.Kind != JournalTransition {
				continue
			}
			var d TransitionData
			if json.Unmarshal(r.Data, &d) == nil {
				fmt.Fprintf(&b, "last transition: %s -> %s\n", r.State, d.To)
			}
			// Include the decision and effects that caused this incoming
			// transition, bounded by its source visit.
			from := i
			for from > 0 {
				previous := c.recs[from-1]
				if previous.Instance == ref.Instance && (previous.Kind == JournalTransition || previous.Kind == JournalInstructionIssued) {
					break
				}
				from--
			}
			for _, incoming := range c.recs[from:i] {
				if incoming.Instance == ref.Instance && incoming.State == r.State {
					if err := explainRecord(&b, incoming); err != nil {
						return err
					}
				}
			}
			break
		}
		for _, r := range c.recs[lo:hi] {
			if r.Instance != ref.Instance || r.State != ref.State {
				continue
			}
			if err := explainRecord(&b, r); err != nil {
				return err
			}
		}
		out = b.String()
		return nil
	})
	return out, err
}

func explainRecord(b *strings.Builder, r JournalRecord) error {
	switch r.Kind {
	case JournalAccepted, JournalCompletedByObservation, JournalAutoAnswered, JournalGateAnswered, JournalRetried, JournalRefused:
		var d struct{ Plan *EdgePlan }
		if err := json.Unmarshal(r.Data, &d); err != nil {
			return err
		}
		if r.Kind == JournalGateAnswered {
			var answer GateAnsweredData
			if err := json.Unmarshal(r.Data, &answer); err != nil {
				return err
			}
			fmt.Fprintf(b, "%s %s: %s (%s)\n", r.Kind, r.Class, answer.Answer, gateAnsweredBy(answer.Session))
		} else if r.Kind != JournalAccepted {
			fmt.Fprintf(b, "%s %s: %s\n", r.Kind, r.Class, r.Data)
		}
		if d.Plan == nil {
			return nil
		}
		for _, edge := range d.Plan.Steps {
			fmt.Fprintf(b, "route: %s -> %s (%s)\n", edge.From, edge.To, edge.Label)
			for _, effect := range edge.Effects {
				raw, err := json.Marshal(effect)
				if err != nil {
					return err
				}
				fmt.Fprintf(b, "effect: %s\n", raw)
			}
		}
	case JournalTransition:
		var d TransitionData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			return err
		}
		fmt.Fprintf(b, "transition: %s -> %s\n", r.State, d.To)
	case JournalCleanupDeferred, JournalCleanupApplied, JournalSubmissionRefused, JournalRouteOffCandidates, JournalEffectIntent, JournalEffectResult, JournalUnitHeld, JournalAttemptLost:
		fmt.Fprintf(b, "%s %s: %s\n", r.Kind, r.Class, r.Data)
	}
	return nil
}
