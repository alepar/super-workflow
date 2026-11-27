package wf

import (
	"fmt"
	"slices"
	"strings"
)

// breakpointPaths indexes each declaration under its full definition path.
// A nil node marks a path shared by distinct declarations.
func breakpointPaths(d *Def) map[string]*node {
	states := map[string]*node{}
	ancestors := map[*Def]bool{}
	var walk func(*Def, string)
	walk = func(def *Def, path string) {
		if ancestors[def] {
			return
		}
		ancestors[def] = true
		defer delete(ancestors, def)
		for _, n := range def.nodes {
			key := path + "/" + n.name
			if old, exists := states[key]; exists && old != n {
				states[key] = nil
			} else {
				states[key] = n
			}
		}
		for _, child := range def.children {
			walk(child, path+"/"+child.name)
		}
	}
	walk(d, d.name)
	return states
}

// resolveBreakpoints accepts bare names and the paths printed by next, with
// an optional leading slash to identify a full path explicitly.
func resolveBreakpoints(d *Def, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	states := breakpointPaths(d)
	var out []string
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		var matches []string
		for state := range states {
			switch {
			case strings.HasPrefix(name, "/"):
				if name == "/"+state {
					matches = append(matches, state)
				}
			case strings.Contains(name, "/"):
				if name == state || name == strings.TrimPrefix(state, d.name+"/") {
					matches = append(matches, state)
				}
			case lastSegment(state) == name:
				matches = append(matches, state)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("%w: --halt-before: unknown state %q", ErrRejected, name)
		case 1:
			if states[matches[0]] == nil {
				return nil, fmt.Errorf("%w: --halt-before: path /%s identifies multiple state declarations", ErrRejected, matches[0])
			}
			out = append(out, matches[0])
		default:
			slices.Sort(matches)
			return nil, fmt.Errorf("%w: --halt-before: ambiguous state %q, choose /%s", ErrRejected, name, strings.Join(matches, ", /"))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func (e *Engine) matchRunBreakpoints(d *Def, started *RunStartedData, run RunID) error {
	if len(e.opts.HaltBefore) == 0 {
		return nil
	}
	targets, err := resolveBreakpoints(d, e.opts.HaltBefore)
	if err != nil {
		return err
	}
	if !slices.Equal(targets, started.HaltBefore) {
		return fmt.Errorf("%w: --halt-before is set at creation and differs from run %s's recorded setting", ErrRejected, run)
	}
	return nil
}

// haltAtBreakpoint checks arrivals under the run lock, before any ready
// state runs. The halt doubles as the record that this arrival has been
// checked, so a resume or retry can proceed and a later visit can halt.
func (c *cycle) haltAtBreakpoint(ids ...string) error {
	targets := c.st.started.HaltBefore
	if len(targets) == 0 || c.st.paused || c.st.ended {
		return nil
	}
	for _, id := range ids {
		in := c.st.insts[id]
		if !c.untilUnitAllows(in) || !c.st.breakpointArmed(in) {
			continue
		}
		return c.emit(in, JournalBreakpointHalted, 0, BreakpointHaltedData{
			State: c.st.qualified(in), ArrivalSeq: in.arrivalSeq, Visit: in.Visits[in.Cur],
			Reason: "matched the run's --halt-before setting",
		})
	}
	return nil
}

// breakpointArmed holds before a matching arrival is journaled as halted.
func (st *runState) breakpointArmed(in *instance) bool {
	return st.started != nil && len(st.started.HaltBefore) > 0 &&
		!in.Settled && in.Phase == phaseReady && in.Attempt == 1 &&
		in.breakpointArrival != in.arrivalSeq &&
		slices.Contains(st.started.HaltBefore, st.started.Def+"/"+st.qualified(in))
}

// breakpointBlocks protects paths that can act on a ready state outside drive.
func (st *runState) breakpointBlocks(in *instance) bool {
	b := st.breakpoint
	return (b != nil && b.Instance == in.ID && b.ArrivalSeq == in.arrivalSeq) || st.breakpointArmed(in)
}

// renameBreakpoint follows a recorded state rename within the same
// definition path, just as the instance's state fold does.
func renameBreakpoint(path string, states map[string]string) string {
	to, ok := states[path]
	if !ok {
		return path
	}
	return path[:strings.LastIndex(path, "/")+1] + lastSegment(to)
}

func (d *RunStartedData) renameBreakpoints(states map[string]string) {
	for i, path := range d.HaltBefore {
		relative := strings.TrimPrefix(path, d.Def+"/")
		d.HaltBefore[i] = d.Def + "/" + renameBreakpoint(relative, states)
	}
	slices.Sort(d.HaltBefore)
	d.HaltBefore = slices.Compact(d.HaltBefore)
}

// appendMigration keeps a changed definition from silently removing a
// configured stop from a live run. A declared state rename can carry it over.
func (c *cycle) appendMigration(def *Def, data MigratedData) error {
	if len(c.st.started.HaltBefore) > 0 {
		started := RunStartedData{Def: c.st.started.Def, HaltBefore: slices.Clone(c.st.started.HaltBefore)}
		started.renameBreakpoints(data.States)
		states := breakpointPaths(def)
		for _, path := range started.HaltBefore {
			if states[path] == nil {
				return fmt.Errorf("%w: run %s's breakpoint /%s needs a unique state in the changed definition; preserve it or declare its state rename", ErrDefChanged, c.run, path)
			}
		}
	}
	return c.emitAs("", "", 0, JournalMigrated, 0, data)
}

// admitBreakpointCoordinator is the bare next path. A running workflow is
// never driven here, even if an operator resumed it after the status read.
func (e *Engine) admitBreakpointCoordinator(run RunID, co Coordinator) (*BreakpointStatus, error) {
	var b *BreakpointStatus
	err := e.withRun(run, func(c *cycle) error {
		if c.st.ended || c.st.breakpoint == nil {
			return nil
		}
		if c.st.childSessions[co.Session] || c.st.refusedChildSessions[co.Session] {
			return fmt.Errorf("%w: next: this session was identified as a child of this run; report to its coordinator and end the child task", ErrRejected)
		}
		b = c.st.breakpoint
		return c.recordCoordinator(co)
	})
	return b, err
}

func (b BreakpointStatus) summary() string {
	if b.Instance != "" {
		return fmt.Sprintf("breakpoint before %s (instance %s, journal seq %d): %s", b.State, b.Instance, b.Seq, b.Reason)
	}
	return fmt.Sprintf("breakpoint before %s (journal seq %d): %s", b.State, b.Seq, b.Reason)
}
