package wf

import (
	"encoding/json"
	"reflect"
)

// terminalOps is the typed half of a Terminal.
type terminalOps[S any] struct{ t *Terminal[S] }

// execute ends the run (root instance) or settles the child (Sub or Map
// instance) at the terminal, with the result its Yield produced.
func (o terminalOps[S]) execute(c *cycle, in *instance, n *node) error {
	k := o.t.k
	var result json.RawMessage
	outcome := k.outcome
	if k.partial != nil {
		ctx, err := c.ctx(in)
		if err != nil {
			return c.halt(in, err.Error())
		}
		partial := k.partial(ctx)
		if ctx.blocked {
			return c.halt(in, ctx.blockedErr().Error())
		}
		if partial {
			outcome = Partial
		}
	}
	if k.yield != nil {
		ctx, err := c.ctx(in)
		if err != nil {
			return c.halt(in, err.Error())
		}
		out := callYield(k.yield, ctx)
		if ctx.blocked {
			return c.halt(in, ctx.blockedErr().Error())
		}
		if err := validatePatterns(reflect.ValueOf(out), "result"); err != nil {
			return c.halt(in, "yield result: "+err.Error())
		}
		b, err := c.marshalPathValue(in, out)
		if err != nil {
			return c.halt(in, "yield result is not JSON: "+err.Error())
		}
		result = b
	}
	if in.ID == "" {
		c.killAllHeadless() // a child still running would outlive its run
		return c.emit(in, JournalRunEnded, Accepted, RunEndedData{Terminal: n.name, Outcome: outcome, Result: result})
	}
	// A child that delivers (a Success terminal, Partial or not) settled
	// Accepted; any other terminal settles with the class of its last failed
	// attempt, Refused when it never failed.
	class := Accepted
	if !outcome.Delivered() {
		if class = in.LastFail; class == 0 {
			class = Refused
		}
	}
	if err := c.emitAs(in.Parent, in.Spawner, in.Attempt, JournalChildSettled, class, ChildSettledData{
		ID: in.ID, State: in.Spawner, Terminal: n.name, Outcome: outcome, Class: class, Result: result, Judged: k.partial != nil,
	}); err != nil {
		return err
	}
	return c.clearEndedChildHalt(in)
}

// clearEndedChildHalt returns a parent to ready when it halted on this child
// (a Sub halted as "did not reach a terminal") and the child has now ended: the
// parent's blocked state follows its current children, so it never keeps the
// reason of a child that already ended and needs no retry of its own. The
// parent may itself be a child its own parent already settled Blocked (a Sub
// inside a Sub): it is revived under Retry's rule, re-executes, and when it
// reaches its terminal this clears the next level up the same way.
func (c *cycle) clearEndedChildHalt(child *instance) error {
	p := c.st.insts[child.Parent]
	if p == nil || !p.halted() || (p.ID != "" && !c.mapUndecided(p)) || p.Cur != child.Spawner {
		return nil
	}
	n := p.node()
	if _, ok := n.kind.(*subKind); !ok || subChildID(p, n) != child.ID {
		return nil
	}
	return c.emit(p, JournalRetried, 0, RetriedData{Child: p.ID, Reason: p.Reason})
}

// endStoppedRun journals run_ended for a run whose root the budget stopped,
// once: the journal then says the run ended, as a terminal would.
func (c *cycle) endStoppedRun() error {
	root := c.st.insts[""]
	if c.st.ended || root == nil || root.Phase != phaseBudgetStoped {
		return nil
	}
	c.killAllHeadless()
	return c.emit(root, JournalRunEnded, 0, RunEndedData{Outcome: Stopped})
}
