package wf

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The soundness rules check properties of the graph
// that a live run used to be the only way to find. They are not in rules, so
// Def.Lint and the served lint are unchanged; LintSound reports them, and a
// client runs it in its tests over each definition it serves.
const (
	RuleStopUnmarked    RuleID = "stop-unmarked"     // A1: a non-success end with no reason a human would accept
	RuleRefusalNoRepair RuleID = "refusal-no-repair" // A2: a loop back into a state that nothing on the way can change
	RuleCycleNoMeasure  RuleID = "cycle-no-measure"  // A3: a cycle with no visit cap and no progress measure
	RuleEndsReasonEmpty RuleID = "ends-reason-empty" // Edge.Ends("")
	RuleMeasureNoReason RuleID = "measure-no-reason" // Measure(s, "")
)

var soundRules = []rule{ruleStopUnmarked, ruleRefusalNoRepair, ruleCycleNoMeasure, ruleEndsOutcomes, ruleGateAnswerOutcomes}

// LintSound is the soundness rules over d and its children, as errors.
func (d *Def) LintSound() []Problem { return lintDef(d, "", map[*Def]bool{}, soundRules, false) }

// Line renders a problem as a lint finding line:
// "lint: <rule> <def>/<state> <msg>", where an edge's msg reads
// `edge "<label>" -> <target> · <why>`.
func (p Problem) Line() string {
	s := fmt.Sprintf("lint: %s %s %s", p.Rule, p.Path, p.Msg)
	if p.Route != "" {
		s += " · path: " + p.Route
	}
	return s
}

// Ends(reason) marks an intentional stop, including a path through cleanup.
// Ends(expectedOutcome, reason) additionally requires a direct terminal with
// that outcome. Gate answers entering terminals must use the outcome form.
// Both forms require a nonblank reason; LintSound checks the contracts.
func (e Edge[S]) Ends(intent any, reasons ...string) Edge[S] {
	var reason string
	var outcome Outcome
	valid := false
	switch value := intent.(type) {
	case string:
		reason, valid = value, len(reasons) == 0
	case Outcome:
		outcome = value
		valid = len(reasons) == 1 && (value == Success || value == Partial || value == Stopped || value == Stalled)
		if len(reasons) == 1 {
			reason = reasons[0]
		}
	}

	if e.e == nil {
		return e
	}
	if !valid && e.e.from != nil && e.e.from.def != nil {
		e.e.from.def.misuse("Ends needs a reason or an Outcome and reason")
	}
	if strings.TrimSpace(reason) == "" && e.e.from != nil && e.e.from.def != nil {
		d := e.e.from.def
		d.problems = append(d.problems, Problem{Rule: RuleEndsReasonEmpty, Path: d.name + "/" + e.e.from.name,
			Msg: fmt.Sprintf("edge %q: Ends needs a reason", e.e.label)})
	}
	if e.e.from != nil && e.e.from.def != nil && e.e.from.def.sealed {
		d := e.e.from.def
		d.problems = append(d.problems, Problem{Rule: RuleBuild, Path: d.name + "/" + e.e.from.name,
			Msg: fmt.Sprintf("edge %q: Ends after the definition was sealed (inspected, hashed or run)", e.e.label)})
		return e
	}
	e.e.ends = reason
	e.e.endsOutcome = outcome
	return e
}

// Measure declares the strict progress measure that bounds every cycle
// through s (A3): each pass makes measure strictly smaller, so the cycle ends
// without a visit cap. The engine does not evaluate it; the definition's own
// checks must enforce it (say so in measure).
func Measure(s State, measure string) {
	n := s.node()
	if strings.TrimSpace(measure) == "" && n.def != nil {
		n.def.problems = append(n.def.problems, Problem{Rule: RuleMeasureNoReason, Path: n.def.name + "/" + n.name,
			Msg: "Measure needs the measure it declares"})
	}
	n.measure = measure
}

// soundGraph is the same-definition graph of a lint pass, by state index.
type soundGraph struct {
	c     *lintCtx
	index map[*node]int
}

func newSoundGraph(c *lintCtx) soundGraph {
	g := soundGraph{c: c, index: map[*node]int{}}
	for i, n := range c.d.nodes {
		g.index[n] = i
	}
	return g
}

// to is the index of edge e's target in this definition, -1 outside it.
func (g soundGraph) to(e *edgeData) int {
	if e.to == nil || e.to.def != g.c.d {
		return -1
	}
	return g.index[e.to]
}

func (g soundGraph) view(i int) StateView { return g.c.states[i] }

func edgeMsg(ev EdgeView, why string) string {
	return fmt.Sprintf("edge %q -> %s · %s", ev.Label, ev.To, why)
}

// runStops is d's terminals that end the run without success. In a root
// definition that is every non-success terminal. In a child it is the ones
// its parent turns into a stop: a Sub exit that leads into the parent's stop
// path. A Map item's non-success end is an item result: the parent's join
// (its partial edge) decides, and is linted there.
func runStops(d *Def, ancestors []*Def) map[*node]bool {
	out := map[*node]bool{}
	var parent *Def
	if len(ancestors) > 0 {
		parent = ancestors[len(ancestors)-1]
	}
	var pg soundGraph
	var pdoomed []bool
	if parent != nil {
		pg = newSoundGraph(&lintCtx{d: parent, states: inspectStates(parent, parent.name), ancestors: ancestors[:len(ancestors)-1]})
		pdoomed = pg.doomed(runStops(parent, ancestors[:len(ancestors)-1]))
	}
	for _, n := range d.nodes {
		t, ok := n.kind.(*terminalKind)
		if !ok || t.outcome.Delivered() {
			continue
		}
		if parent == nil {
			out[n] = true
			continue
		}
		for _, pn := range parent.nodes {
			sk, ok := pn.kind.(*subKind)
			if !ok || sk.child != d {
				continue
			}
			for _, x := range sk.exits {
				if x.terminal != n.name || x.e.e == nil {
					continue
				}
				if to := pg.to(x.e.e); to >= 0 && pdoomed[to] {
					out[n] = true
				}
			}
		}
	}
	return out
}

// doomed is the states from which every way on ends at one of stops: those
// terminals, and every other state with edges all of which go to doomed
// states.
func (g soundGraph) doomed(stops map[*node]bool) []bool {
	ns := g.c.d.nodes
	out := make([]bool, len(ns))
	for i, n := range ns {
		out[i] = stops[n]
	}
	for changed := true; changed; {
		changed = false
		for i, n := range ns {
			if out[i] || g.view(i).Kind == "terminal" || len(n.edges) == 0 {
				continue
			}
			all := true
			for _, e := range n.edges {
				if t := g.to(e); t < 0 || !out[t] {
					all = false
					break
				}
			}
			if all {
				out[i], changed = true, true
			}
		}
	}
	return out
}

// childStopEdges is the edges a sub state takes when its child ends at a
// non-success terminal: forwarding a child's stop is the child's decision,
// linted in the child.
func childStopEdges(n *node) map[*edgeData]bool {
	out := map[*edgeData]bool{}
	sk, ok := n.kind.(*subKind)
	if !ok || sk.child == nil {
		return out
	}
	for _, x := range sk.exits {
		for _, cn := range sk.child.nodes {
			if t, ok := cn.kind.(*terminalKind); ok && cn.name == x.terminal && !t.outcome.Delivered() && x.e.e != nil {
				out[x.e.e] = true
			}
		}
	}
	return out
}

// routed is the edges n's Route or Cases table can pick: its decisions. An
// exempt role (a retry's fallback, a failure-class route, a visit cap's over
// edge) does not exempt an edge that is also one of these.
func routed(n *node) map[*edgeData]bool {
	out := map[*edgeData]bool{}
	if n.route != nil {
		for _, u := range n.route.cands {
			out[u.e] = true
		}
	}
	if n.cases != nil {
		for _, c := range n.cases.cases {
			out[c.e.e] = true
			for _, u := range c.cands {
				out[u.e] = true
			}
		}
		if n.cases.otherwise != nil {
			out[n.cases.otherwise.e] = true
		}
	}
	return out
}

// ruleStopUnmarked is A1: every edge that commits the run to a non-success
// end (from a state that could still succeed into one that cannot) is a
// reason a human would accept: it leaves a human gate, it is an exhausted
// retry or visit budget, a failure-class route or a deadline, it forwards a
// child's own stop, or it is marked Ends(reason).
func ruleStopUnmarked(c *lintCtx) []Problem {
	g := newSoundGraph(c)
	doomed := g.doomed(runStops(c.d, c.ancestors))
	var out []Problem
	if len(c.d.nodes) > 0 && doomed[0] && g.view(0).Kind != "terminal" {
		out = append(out, Problem{Rule: RuleStopUnmarked, Path: g.view(0).Path,
			Msg: "· every way through this definition ends at a stop: it cannot succeed"})
	}
	for i, n := range c.d.nodes {
		if doomed[i] {
			continue
		}
		v := g.view(i)
		forwards, decides := childStopEdges(n), routed(n)
		human := false
		if gk, ok := n.kind.(*gateKind); ok && gk.reservation == Reserved {
			human = true
		}
		for j, e := range n.edges {
			t := g.to(e)
			if t < 0 || !doomed[t] {
				continue
			}
			ev := v.Edges[j]
			role := !decides[e] && (ev.Fallback != "" || ev.Class != "" || e.visits != nil)
			// A refused tracker write is data the run can repair (graph-gate
			// plan E2): the Blocked route of a deterministic state whose
			// edges write the tracker must reach a repair, not a stop. An
			// agent's Blocked route is its own declaration, with the
			// recovery offer.
			if role && e.class == Blocked && v.Kind != "agent" && writesTracker(v) {
				role = false
			}
			if e.ends != "" || role || ev.Timeout > 0 || human || forwards[e] {
				continue
			}
			out = append(out, Problem{Rule: RuleStopUnmarked, Path: v.Path,
				Msg: edgeMsg(ev, "a stop reached by no human gate, exhausted budget or failure route: mark it Ends(reason) or route it to a repair")})
		}
	}
	return out
}

// writesTracker reports whether some edge use of the state writes the
// tracker: a bd effect, or a shell effect that runs a bd write (bd as the
// command, or a bd write verb in a bash -c script).
func writesTracker(v StateView) bool {
	for _, u := range v.uses {
		for _, ef := range u.effects {
			x, ok := ef.(ExternalEffect)
			if !ok {
				continue
			}
			if x.Adapter == "bd" || (x.Adapter == "shell" && shellWritesTracker(x.Args["argv"])) {
				return true
			}
		}
	}
	return false
}

// bdWrite matches a bd write command in a script: bd followed by a verb
// that changes the tracker.
var bdWrite = regexp.MustCompile(`(^|[^A-Za-z0-9_-])bd\s+(create|update|close|reopen|delete|supersede|dep\s+(add|remove|rm)|label\s+(add|remove))\b`)

// shellWritesTracker reports whether a shell effect's argv writes the
// tracker.
func shellWritesTracker(argv any) bool {
	var args []string
	switch a := argv.(type) {
	case []string:
		args = a
	case []any:
		for _, x := range a {
			s, _ := x.(string)
			args = append(args, s)
		}
	}
	if len(args) == 0 {
		return false
	}
	if args[0] == "bd" {
		return bdWrite.MatchString(strings.Join(args, " "))
	}
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && bdWrite.MatchString(args[i+1]) {
			return true
		}
	}
	return false
}

// sccs is the strongly connected components of the definition's own graph
// that hold a cycle (more than one state, or a state with an edge to itself).
func (g soundGraph) sccs(removed func(int) bool, cut func(int, *edgeData) bool) [][]int {
	ns := g.c.d.nodes
	idx, low := make([]int, len(ns)), make([]int, len(ns))
	on := make([]bool, len(ns))
	for i := range idx {
		idx[i] = -1
	}
	var stack []int
	var out [][]int
	next := 0
	var visit func(int)
	visit = func(v int) {
		idx[v], low[v] = next, next
		next++
		stack = append(stack, v)
		on[v] = true
		for _, e := range ns[v].edges {
			w := g.to(e)
			if w < 0 || removed(w) || cut(v, e) {
				continue
			}
			if idx[w] < 0 {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], idx[w])
			}
		}
		if low[v] != idx[v] {
			return
		}
		var comp []int
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			on[w] = false
			comp = append(comp, w)
			if w == v {
				break
			}
		}
		self := slices.ContainsFunc(ns[v].edges, func(e *edgeData) bool { return g.to(e) == v && !cut(v, e) })
		if len(comp) > 1 || self {
			slices.Sort(comp)
			out = append(out, comp)
		}
	}
	for i := range ns {
		if idx[i] < 0 && !removed(i) {
			visit(i)
		}
	}
	slices.SortFunc(out, func(a, b []int) int { return a[0] - b[0] })
	return out
}

func (g soundGraph) names(comp []int) string {
	names := make([]string, len(comp))
	for i, s := range comp {
		names[i] = g.c.d.nodes[s].name
	}
	return strings.Join(names, ", ")
}

// ruleCycleNoMeasure is A3: every cycle declares what bounds it, a visit cap
// (MaxVisits) on one of its states or a strict progress measure (Measure),
// or passes a reserved gate, where a human answers every pass. The graph
// left is what can repeat for ever: a Measure state and a reserved gate are
// taken out, and a capped state keeps only its over edge, which is all it
// takes once its cap is spent. Any cycle left is unbounded.
func ruleCycleNoMeasure(c *lintCtx) []Problem {
	g := newSoundGraph(c)
	removed := func(s int) bool {
		n := c.d.nodes[s]
		if gk, ok := n.kind.(*gateKind); ok && gk.reservation == Reserved {
			return true // a human answers every pass
		}
		return n.measure != ""
	}
	cut := func(s int, e *edgeData) bool {
		n := c.d.nodes[s]
		return n.maxVisits != nil && e != n.maxVisits.over.e
	}
	var out []Problem
	for _, comp := range g.sccs(removed, cut) {
		out = append(out, Problem{Rule: RuleCycleNoMeasure, Path: g.view(comp[0]).Path,
			Msg: fmt.Sprintf("· the cycle through %s has no MaxVisits cap and no Measure", g.names(comp))})
	}
	return out
}

// changes reports whether passing through state s, or taking edge e out of
// it, can change what a state on the loop reads: an agent, a human gate, a
// command, a wait, a child run, or an edge with an effect.
func (g soundGraph) changes(s int) bool {
	switch g.view(s).Kind {
	case "agent", "gate", "command", "wait", "sub", "map":
		return true
	}
	return false
}

// ruleRefusalNoRepair is A2: a loop that comes back to a state must pass
// something that can change what it reads. An agent's checks are repaired in
// place by the agent's next answer; a loop of computes whose edges carry no
// effect re-reads the same inputs and decides the same way, so a refusal
// routed round it can never be repaired. The states and edges that can
// change something are taken out: any cycle left has none of them.
func ruleRefusalNoRepair(c *lintCtx) []Problem {
	g := newSoundGraph(c)
	var out []Problem
	for _, comp := range g.sccs(g.changes, g.edgeWrites) {
		out = append(out, Problem{Rule: RuleRefusalNoRepair, Path: g.view(comp[0]).Path,
			Msg: fmt.Sprintf("· the loop through %s passes no agent, gate, command, wait, child or effect: nothing on it can change what it reads", g.names(comp))})
	}
	return out
}

// edgeWrites reports whether every traversal of edge e out of state s
// carries an effect: a use with none is a way round the loop that writes
// nothing (a Route closure may return the bare edge).
func (g soundGraph) edgeWrites(s int, e *edgeData) bool {
	if len(e.effects) > 0 {
		return true
	}
	v := g.view(s)
	used := false
	for _, u := range v.uses {
		if u.e != e {
			continue
		}
		used = true
		if len(u.effects) == 0 {
			return false
		}
	}
	return used
}
