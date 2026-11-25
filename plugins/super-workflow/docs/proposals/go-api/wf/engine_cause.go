package wf

import (
	"fmt"
	"strings"
)

// runCause is the Run cause: why an ended run ended, read from the folded
// journal. It walks back from the terminal entry (the run_ended record, or the
// end of the journal for a budget-stopped root whose run_ended is not written
// yet) over the records of the terminal's instance and, recursively, the child
// (Sub or Map) instances whose failed settlement fed the edge into it, and
// answers with the last failed observation or effect error found, else a
// budget stop, else a failed child's settlement, else the label of the edge
// into the terminal. The walk is bounded below by the final edge's source
// state's last entry: the root record that carries the plan of the edge whose
// transition last entered that state. Bounding at the plan rather than the
// transition keeps that edge's own effects (journaled between the two) in the
// window, which a failure route of the source state is decided on. Only that visit fed the
// final edge, so a root failure recorded before it, or a child that settled in
// an earlier visit, is history the run moved past. A failure
// later superseded by a successful observation (or effect result) of the same
// state is skipped, and sibling branches that did not fail never join the path.
// A successful run has no failure to explain: its cause is the edge label
// alone. The cause is empty only when neither source exists.
//
// Next (the ended instruction) and Status both read it; neither computes a
// cause itself.
func (c *cycle) runCause() string {
	end := len(c.recs)
	term, outcome := c.st.term, c.st.outcome
	for i := len(c.recs) - 1; i >= 0; i-- {
		if c.recs[i].Kind != JournalRunEnded {
			continue
		}
		end = i
		if d, err := decodeData(c.recs[i]); err == nil {
			ended := d.(*RunEndedData)
			if ended.Cause != "" {
				return ended.Cause
			}
			term, outcome = ended.Terminal, ended.Outcome
		}
		break
	}
	edge, from, label := c.finalEdge(term, end)
	if outcome.Delivered() {
		return label
	}
	lo := -1 // the final edge's source state's last entry; -1 when unbounded
	if edge >= 0 {
		lo = c.lastEntry(from, edge)
	}

	onPath := map[string]bool{"": true}
	recovered := map[string]bool{} // "<kind>|<instance>|<state>": a later success of that state
	intents := map[int64]EffectData{}
	for _, r := range c.recs[:end] {
		if r.Kind == JournalEffectIntent {
			if d, err := decodeData(r); err == nil {
				intents[r.Seq] = d.(*EffectIntentData).Effect
			}
		}
	}
	var childFallback string
	for i := end - 1; i > lo; i-- {
		r := c.recs[i]
		if r.Kind == JournalChildSettled {
			// The parent's record names the child: a failed settlement puts
			// the child's own records on the path.
			if !onPath[r.Instance] {
				continue
			}
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			s := d.(*ChildSettledData)
			if s.Cancelled || (s.Class == Accepted && s.Outcome.Delivered()) {
				continue
			}
			onPath[s.ID] = true
			if childFallback == "" {
				childFallback = fmt.Sprintf("%s ended at %s (%s)", c.instanceName(s.ID), s.Terminal, s.Outcome)
				if why := c.deciding(s.ID, s.Terminal, i); why != "" {
					childFallback += ": " + why
				}
			}
			continue
		}
		if !onPath[r.Instance] {
			continue
		}
		key := func(kind string) string { return kind + "|" + r.Instance + "|" + r.State }
		d, err := decodeData(r)
		if err != nil {
			continue
		}
		switch v := d.(type) {
		case *ObservedData:
			if v.Error == "" && !v.Unavailable {
				recovered[key("observed")] = true
				continue
			}
			if recovered[key("observed")] {
				continue
			}
			what := "failed"
			if v.Unavailable {
				what = "unavailable"
			}
			return fmt.Sprintf("%s: %s.%s %s: %s", c.causeWhere(r), v.Desc.Adapter, v.Desc.Op, what, v.Error)
		case *EffectResultData:
			if v.OK {
				recovered[key("effect")] = true
				continue
			}
			if recovered[key("effect")] {
				continue
			}
			ef := intents[v.Intent]
			what := v.Outcome
			if what == "" {
				what = "failed"
			}
			return fmt.Sprintf("%s: %s.%s %s: %s", c.causeWhere(r), ef.Adapter, ef.Op, what, v.Error)
		case *BudgetStoppedData:
			return fmt.Sprintf("%s: budget stopped: %s", c.causeWhere(r), v.Reason)
		}
	}
	if childFallback != "" {
		return childFallback
	}
	return c.deciding("", term, end)
}

// deciding names what decided a non-success end that no failed read or
// effect explains: the state whose edge entered the terminal, the edge's
// label (a client names the record holding the details in it, such as a
// graph gate's graph-gate-stop.json) and, when that edge was a refusal's
// exhausted route, the deciding check's reasons. inst is the instance whose
// terminal it is; end bounds the records read. It is "" when no edge
// carries a label.
func (c *cycle) deciding(inst, term string, end int) string {
	idx, from, label := c.finalEdgeIn(inst, term, end)
	if idx < 0 || label == "" {
		return label
	}
	where := from
	if inst != "" {
		where = c.instanceName(inst) + "/" + from
	}
	out := where + ": " + label
	if d, err := decodeData(c.recs[idx]); err == nil {
		if rt, ok := d.(*RetriedData); ok && rt.Answer == RetryStop {
			// A stop or recovery give-up took the edge: the halt it answered
			// holds the deciding refusal.
			for j := idx - 1; j >= 0; j-- {
				if c.recs[j].Instance != c.recs[idx].Instance || c.recs[j].Kind != JournalRefused {
					continue
				}
				if hd, err := decodeData(c.recs[j]); err == nil {
					if hr, ok := hd.(*RefusedData); ok && hr.Halt {
						d = hr
					}
				}
				break
			}
		}
		if r, ok := d.(*RefusedData); ok && len(r.Reasons) > 0 {
			lines := make([]string, len(r.Reasons))
			for i, why := range r.Reasons {
				first, _, _ := strings.Cut(why, "\n")
				lines[i] = first
			}
			out += " · refused: " + strings.Join(lines, "; ")
		}
	}
	return out
}

// causeWhere names the state a cause record belongs to: the child definition
// names and the state, with the instance id for a child.
func (c *cycle) causeWhere(r JournalRecord) string {
	if r.Instance == "" {
		return r.State
	}
	if in := c.st.insts[r.Instance]; in != nil {
		return c.instanceName(r.Instance) + "/" + r.State
	}
	return r.Instance + "/" + r.State
}

// instanceName is the child instance's definition path (its id when unknown).
func (c *cycle) instanceName(id string) string {
	if in := c.st.insts[id]; in != nil {
		if p := c.st.defPath(in); len(p) > 0 {
			return fmt.Sprintf("%s[%s]", strings.Join(p, "/"), id)
		}
	}
	return id
}

// finalEdge finds the edge the root instance last took into the terminal
// before record index end: the index of the record that carries it, the state
// it leaves and its label. idx is -1 (and label "") when no record carries one.
func (c *cycle) finalEdge(term string, end int) (idx int, from, label string) {
	return c.finalEdgeIn("", term, end)
}

// finalEdgeIn is finalEdge for instance inst.
func (c *cycle) finalEdgeIn(inst, term string, end int) (idx int, from, label string) {
	for i := end - 1; i >= 0; i-- {
		r := c.recs[i]
		if r.Instance != inst {
			continue
		}
		plan := recordPlan(r)
		if plan == nil || len(plan.Steps) == 0 {
			continue
		}
		if last := plan.Steps[len(plan.Steps)-1]; last.To == term {
			return i, plan.Steps[0].From, last.Label
		}
	}
	return -1, "", ""
}

// recordPlan is the edge plan a record carries, nil when it carries none.
func recordPlan(r JournalRecord) *EdgePlan {
	d, err := decodeData(r)
	if err != nil {
		return nil
	}
	switch v := d.(type) {
	case *AcceptedData:
		return &v.Plan
	case *CompletedByObservationData:
		return &v.Plan
	case *AutoAnsweredData:
		return v.Plan
	case *GateAnsweredData:
		return v.Plan
	case *RefusedData:
		return v.Plan
	case *RetriedData:
		return v.Plan
	case *RouteOffCandidatesData:
		return v.Plan
	case *PromptOffCandidatesData:
		return v.Plan
	case *EffectSettledData:
		if v.Refused != nil {
			return v.Refused.Plan
		}
	}
	return nil
}

// lastEntry is the index of the root record carrying the plan of the edge
// whose transition last entered state before record index before: the root
// instance's last transition into state, then the nearest plan record before
// it whose edge ends there. That edge's effects lie between the two. It is -1
// when the run started there.
func (c *cycle) lastEntry(state string, before int) int {
	t := -1
	for i := before - 1; i >= 0; i-- {
		r := c.recs[i]
		if r.Instance != "" || r.Kind != JournalTransition {
			continue
		}
		if d, err := decodeData(r); err == nil && d.(*TransitionData).To == state {
			t = i
			break
		}
	}
	for i := t - 1; i >= 0; i-- {
		r := c.recs[i]
		if r.Instance != "" {
			continue
		}
		if plan := recordPlan(r); plan != nil && len(plan.Steps) > 0 && plan.Steps[len(plan.Steps)-1].To == state {
			return i
		}
	}
	return t
}

// endedInstruction is the single instruction an ended run hands out on every
// form of next.
func (c *cycle) endedInstruction() Instruction {
	return Instruction{
		Run: c.run, State: c.st.term, Kind: InstructionEnded,
		Terminal: c.st.term, Outcome: c.st.outcome, Cause: c.runCause(), Resume: c.resumeLine(),
		Counts: c.countsLine(),
	}
}

// countsLine is the run's subagent totals, the head of status's totals line
// ("subagents 14: 7 done, 3 handed, 4 waiting"): what a coordinator's final
// report can quote directly.
func (c *cycle) countsLine() string {
	var rs RunStatus
	c.statusUnits(&rs, c.now())
	head, _, _ := strings.Cut(StatusTotals(rs), " · ")
	return head
}
