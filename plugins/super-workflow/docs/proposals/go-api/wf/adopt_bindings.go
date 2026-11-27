package wf

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// adoptBindingProblems checks, for an adoption onto a changed definition,
// that every {name} a live instance can still bind is one its run can supply
// . Without it, a step whose placeholder the journaled outputs never
// produced halts mid-run as a definition error, after the adoption has been
// journaled and the run carried on.
//
// The bindings are the static ones: a command's argv, stdin and idempotency
// key, a read's shell argv, and the shell argv of an effect on an edge the
// engine takes itself (not a Route's or an OnRoute case's candidate, whose
// effects the closure builds). A name a compute or
// route closure looks up through Ctx is not known. Prompts are not checked: a
// prompt's unknown {{name}} binds an Out input and its path leaves an unknown
// name unbound, so neither halts on one. Artifact paths are not checked
// either: a path a submission recorded wins over the template, and an
// exported artifact's template binds in the child that writes it, so a
// template the run's own scope cannot bind is no evidence of a halt.
//
// A binding can carry over when its run scope supplies it, or when some
// route from the live state can run a producer before the binding. A route
// callback's candidate is not proof that a bypass of that producer can
// happen. The executor still checks the binding when the route actually runs.
//
// A journaled value must keep the state that supplied it. When no value was
// recorded and the producer can no longer run before the binding, adoption
// refuses and points at the checkpoint before its last visit, if there is one.
func (c *cycle) adoptBindingProblems(def *Def, stateName func(*instance, string) string, recs []JournalRecord) []string {
	var out []string
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in == nil || in.Phase == phaseDone {
			continue
		}
		d := defNamed(def, c.st.ownDef(in))
		if d == nil {
			continue
		}
		start := d.find(stateName(in, in.Cur))
		if start == nil {
			continue
		}
		// The current state's work is still next unless its result is
		// stored and an edge chosen: then it binds nothing again and
		// supplies only what it stored, which the scope already reads.
		pending := in.Phase != phaseTransition
		workNext := in.Phase == phaseReady || in.Phase == phaseBackoff || in.Phase == phaseHalted
		s := c.scope(in)
		views := ownStates(d)
		routes := bindingRoutes(views)
		for _, b := range reachableBindings(views, start, workNext, in.Phase == phaseIssued) {
			if s.adoptionBuiltinOrParam(b.name) {
				continue
			}
			where := strings.Join(append(c.st.defPath(in), b.step.name), "/")
			binding := fmt.Sprintf("%s binds {%s} in its %s", where, b.name, b.where)
			_, owner, producer, recorded := s.lookupDocumentSource(b.name)
			if recorded {
				if producer == "" {
					continue // a journaled input is independent of a state output
				}
				source := strings.Join(append(c.st.defPath(owner), producer), "/")
				ownerDef := defNamed(def, c.st.ownDef(owner))
				var node *node
				if ownerDef != nil {
					node = ownerDef.find(stateName(owner, producer))
				}
				if node == nil {
					out = append(out, fmt.Sprintf("%s, whose journaled producer %s was removed", binding, source))
				} else if t := bindingOutputType(node); t != nil && !typeProducesBinding(t, b.name) {
					out = append(out, fmt.Sprintf("%s, whose journaled producer %s no longer declares the field", binding, source))
				}
				continue
			}
			if !b.submit && futureBindingProducer(routes, start, pending, workNext, b) {
				continue
			}
			msg := fmt.Sprintf("%s, which nothing the run journaled supplies and no producer can supply before this binding from %s", binding, start.name)
			if p, seq := lastProducer(d, in, b.name, recs); p != "" {
				target := strings.Join(append(c.st.defPath(in), p), "/")
				if cp := checkpointBefore(recs, seq); cp > 0 {
					msg += fmt.Sprintf("; rewind before %s (wf dev rewind --to %d), so this definition runs it again", target, cp)
				} else {
					msg += fmt.Sprintf("; rewind before %s, so this definition runs it again (the journal has no checkpoint before it)", target)
				}
			} else {
				msg += "; no state the run has visited produces it under this definition"
			}
			out = append(out, msg)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// adoptBinding is one static {name} a state binds: in its own work, or in
// the effects of edge as it leaves.
type adoptBinding struct {
	step   *node
	route  bindingRoute
	name   string
	where  string
	submit bool // needed before the handed-out state can select an edge
}

// reachableBindings lists the bindings on reachable work and edge uses.
// Already handed-out work is checked only when the graph can visit it again.
func reachableBindings(views []StateView, start *node, workNext, submitNext bool) []adoptBinding {
	nodes, revisits := reachable(start)
	var out []adoptBinding
	for _, v := range views {
		n := v.node
		if !nodes[n] {
			continue
		}
		add := func(route bindingRoute, where string, submit bool, strs ...string) {
			for _, s := range strs {
				for _, m := range bindPattern.FindAllStringSubmatch(s, -1) {
					out = append(out, adoptBinding{n, route, m[1], where, submit})
				}
			}
		}
		addReads := func(reads []ObservationDesc, submit bool) {
			for _, r := range reads {
				if r.Adapter == "shell" && r.Op == "run" {
					add(bindingRoute{}, "read", submit, argvOf(ExternalEffect{Adapter: "shell", Op: "run", Args: r.Args})...)
				}
			}
		}
		if n != start || workNext || revisits {
			if k, ok := n.kind.(*commandKind); ok {
				add(bindingRoute{}, "command", false, k.argv...)
				add(bindingRoute{}, "stdin", false, k.stdin)
				add(bindingRoute{}, "idempotency key", false, k.key...)
			}
			addReads(nodeReads(n), false)
		}
		if k, ok := n.kind.(*agentKind); ok && n == start && submitNext {
			// Submission rebinds the check and staleness observations of
			// this very attempt, before a routing decision can add outputs.
			addReads(k.observes, true)
			addReads(k.inputs, true)
		}
		for i, u := range v.uses {
			if v.useKinds[i] == candidateUse {
				continue // the callback supplies the effects of the actual use
			}
			for _, ef := range u.effects {
				add(bindingRoute{u.e, v.useKinds[i] == failedUse}, fmt.Sprintf("edge %q effect", useLabel(u)), false, argvOf(ef)...)
			}
		}
	}
	return out
}

type bindingRoute struct {
	edge   *edgeData
	failed bool
}

// bindingRoutes preserves separate failure and output-producing uses of a
// shared edge. The Edge pointer alone does not indicate whether work ran.
func bindingRoutes(views []StateView) map[*node][]bindingRoute {
	routes := make(map[*node][]bindingRoute, len(views))
	for _, v := range views {
		n := v.node
		failed := map[*edgeData]bool{}
		success := map[*edgeData]bool{}
		for i, u := range v.uses {
			if v.useKinds[i] == failedUse {
				failed[u.e] = true
			} else {
				success[u.e] = true
			}
		}
		for _, u := range n.defaults {
			failed[u.e] = true
		}
		for _, e := range outEdges(n) {
			if e.to == nil || e.to.def != n.def {
				continue
			}
			isFailure := failed[e] || e.class != 0 || e.timeout != 0
			if !isFailure || success[e] {
				routes[n] = append(routes[n], bindingRoute{edge: e})
			}
			if isFailure {
				routes[n] = append(routes[n], bindingRoute{edge: e, failed: true})
			}
		}
	}
	return routes
}

// outEdges are n's edges and the failure defaults it takes.
func outEdges(n *node) []*edgeData {
	out := slices.Clone(n.edges)
	for _, u := range n.defaults {
		if u.e != nil {
			out = append(out, u.e)
		}
	}
	return out
}

// reachable returns the set of states reachable from start, including
// start, and whether an edge can return to start after leaving its work.
func reachable(start *node) (map[*node]bool, bool) {
	seen := map[*node]bool{start: true}
	queue := []*node{start}
	revisits := false
	for i := 0; i < len(queue); i++ {
		for _, e := range outEdges(queue[i]) {
			to := e.to
			if to == start {
				revisits = true
			}
			if to != nil && to.def == start.def && !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	return seen, revisits
}

// futureBindingProducer looks for a route on which an instance can produce
// b.name before binding it. A successful output is available on its own
// outgoing effects. A failed attempt, or a live state already in transition,
// does not produce another output.
func futureBindingProducer(routes map[*node][]bindingRoute, start *node, pending, workNext bool, b adoptBinding) bool {
	type position struct {
		node     *node
		supplied bool
	}
	queue := []position{{node: start}}
	seen := map[position]bool{}
	for i := 0; i < len(queue); i++ {
		at := queue[i]
		if at.node == b.step && b.route.edge == nil && (i > 0 || workNext) {
			if at.supplied {
				return true
			}
			continue
		}
		producer := !at.supplied && (i > 0 || pending) && produces(at.node, b.name)
		for _, route := range routes[at.node] {
			supplied := at.supplied || producer && !route.failed
			if at.node == b.step && route == b.route {
				if supplied {
					return true
				}
				continue
			}
			next := position{route.edge.to, supplied}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// produces reports whether n, leaving on a successful edge, stores a scalar
// at field path name: a field of the type it stores, as the exec lint's
// binders walk it. An omitempty or pointer field counts: whether a state
// still ahead fills it is the new definition's own contract, which halts the
// same way on a fresh run (a lint matter, not adoption skew); only what the
// run already journaled must hold the field, and the scope reads that JSON.
// A Map stores its join, keyed by item, and never a flat field.
func produces(n *node, name string) bool {
	t := bindingOutputType(n)
	return t != nil && typeProducesBinding(t, name)
}

func typeProducesBinding(t reflect.Type, name string) bool {
	return binding(t, strings.Split(name, "."), "", nil) != bindsNot
}

// bindingOutputType is the shape used for flat bindings. A Map's retained
// join can still supply a concrete journaled path, but its child result type
// does not predict a flat field for a future join.
func bindingOutputType(n *node) reflect.Type {
	switch k := n.kind.(type) {
	case *agentKind:
		return k.outT
	case *computeKind:
		return k.outT
	case *commandKind:
		return reflect.TypeFor[RunResult]()
	case *subKind:
		if k.child != nil {
			return k.child.result
		}
	}
	return nil
}

// adoptionBuiltinOrParam checks values independent of the journaled state
// documents. It avoids calling builtins that allocate directories.
func (s scope) adoptionBuiltinOrParam(name string) bool {
	switch name {
	case "run", "workspace", "harness", "resolvedModels", recordsName, scratchName, blockedCauseName, predecessorName, workdirName, "assets", "skillsRoot":
		return true
	}
	if strings.HasPrefix(name, artifactToken) {
		return true
	}
	_, ok := s.c.st.started.Params[name]
	return ok
}

// lastProducer is the state of d producing name that in visited last, and the
// seq of the first record of that visit ("" when it visited none).
func lastProducer(d *Def, in *instance, name string, recs []JournalRecord) (string, int64) {
	state, first := "", int64(0)
	for i, r := range recs {
		if r.Instance != in.ID || r.State == "" {
			continue
		}
		n := d.find(r.State)
		if n == nil || !produces(n, name) {
			continue
		}
		if r.State != state || !sameVisit(recs[:i], in.ID, r.State) {
			state, first = r.State, r.Seq
		}
	}
	return state, first
}

// sameVisit reports whether the instance's previous record before recs' end
// is at state: a record continuing that visit.
func sameVisit(recs []JournalRecord, id, state string) bool {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Instance != id || recs[i].State == "" {
			continue
		}
		return recs[i].State == state && recs[i].Kind != JournalTransition
	}
	return false
}

// checkpointBefore is the seq of the last available world snapshot before seq
// (0 when none).
func checkpointBefore(recs []JournalRecord, seq int64) int64 {
	var cp int64
	for _, r := range recs {
		if r.Seq >= seq {
			break
		}
		var w WorldSnapshotData
		if r.Kind == JournalWorldSnapshot && json.Unmarshal(r.Data, &w) == nil && w.Unavailable == "" {
			cp = r.Seq
		}
	}
	return cp
}

// defNamed is the definition named name among d and its children.
func defNamed(d *Def, name string) *Def {
	if d.name == name {
		return d
	}
	for _, ch := range d.children {
		if found := defNamed(ch, name); found != nil {
			return found
		}
	}
	return nil
}
