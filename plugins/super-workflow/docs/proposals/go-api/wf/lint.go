package wf

import (
	"fmt"
	"slices"
	"strings"
)

// lintCtx is what every graph-wide rule reads: the definition and the shared
// walk of its own states (children are linted by their own pass).
type lintCtx struct {
	d         *Def
	states    []StateView
	ancestors []*Def
}

// rule is one graph-wide lint rule: a function over the whole definition.
type rule func(c *lintCtx) []Problem

// rules is the ordered rule slice. A new graph-wide rule is one function
// appended here; kind-specific rules are stepKind.lint.
var rules = []rule{
	ruleAssets,
	rulePathParamDeclarations,
	ruleNoStates,
	ruleUnrouted,
	ruleNoEdges,
	ruleUnreachable,
	ruleNoTerminal,
	ruleForeignEdge,
	ruleDuplicateName,
	ruleBudgetUnwired,
	ruleCounterScope,
	ruleCounterScopeDef,
	ruleArtifactNotUpstream,
	ruleArtifactType,
	ruleChildNotExported,
	ruleEffectKey,
	ruleCommandKey,
	ruleReservedPolicy,
	ruleInputUnresolved,
	ruleSubRecursive,
	ruleChildNameDuplicate,
	ruleHoldEffectOutside,
	ruleRouteCandidates,
	ruleArtifactName,
	ruleExplicitPromptOutputs,
	ruleFreeRouteEnum,
	ruleAgentBlockedRoute,
	ruleAgentOutputPath,
	ruleMaxlenTag,
}

// stagedRules become rules once every client definition is migrated to
// them (artifact-identity B); until then LintStaged reports them as notes and
// nothing fails on them.
var stagedRules = []rule{ruleArtifactTemplated}

// LintStaged is the staged rules over d and its children: notes for what the
// next migration turns into errors. The CLI's lint prints them; nothing
// fails on them.
func (d *Def) LintStaged() []Problem {
	ps := lintDef(d, "", map[*Def]bool{}, stagedRules, false)
	for i := range ps {
		ps[i].Severity = SeverityNote
	}
	return ps
}

// ruleArtifactName refuses an allocated artifact whose name is not a plain
// file name: the name is the file's name in its instance's directory, and a
// separator or a dot name would leave it.
func ruleArtifactName(c *lintCtx) []Problem {
	var out []Problem
	seen := map[string]bool{}
	for _, a := range c.d.artifacts {
		if a.path != "" || a.via != nil {
			continue
		}
		if a.name == "" || a.name == "." || a.name == ".." || strings.ContainsAny(a.name, "/\\") {
			out = append(out, a.producer.problem(RuleArtifactName, "allocated artifact %q is not a plain file name", a.name))
		}
		// An instance records artifacts by name: two allocated producers of one
		// name would alias each other's paths.
		if seen[a.name] {
			out = append(out, a.producer.problem(RuleArtifactName, "allocated artifact %q is declared twice in %s: give each producer's artifact its own name", a.name, c.d.name))
		}
		seen[a.name] = true
	}
	// A parent cannot resolve a child's allocated path (it lives in the
	// child's instance): the child passes it up in its result (wf.RefOf in a
	// Yield).
	for i := range c.states {
		n := c.states[i].node
		for _, a := range n.consumes {
			if a.via != nil && a.path == "" {
				out = append(out, n.problem(RuleArtifactName, "consumes the child's allocated artifact %q through FromChild: return its wf.Ref in the child's result instead", a.name))
			}
		}
	}
	return out
}

// ruleArtifactTemplated notes an artifact whose path the definition composes
// (a template): the engine should allocate it, and consumers bind
// wf.ArtifactPath. A repository file (NewRepoArtifact) keeps its product path.
func ruleArtifactTemplated(c *lintCtx) []Problem {
	var out []Problem
	for _, a := range c.d.artifacts {
		if a.path == "" || a.via != nil || a.repo {
			continue
		}
		out = append(out, a.producer.problem(RuleArtifactTemplated, "artifact %q composes its path %q: declare it without a path (wf.NewArtifact(producer, name)) and bind wf.ArtifactPath", a.name, a.path))
	}
	return out
}

// Lint checks what the Go type system cannot: graph shape and wiring. It never
// stops at the first problem, recurses through Sub and Map children (paths are
// qualified parent/child/State), and never executes a user closure. Callers
// match Problem.Rule, never message text.
func (d *Def) Lint() []Problem { return lintDef(d, "", map[*Def]bool{}, rules, true) }

// servedRules are the rules the CLI adds for definitions it serves. They are
// not in rules, so Def.Lint stays what engine tests and non-tiered callers see.
var servedRules = []rule{
	ruleLiteralModel,
	ruleAgentSelfEdge,
}

// LintServed is Lint plus the served-registration rules: what the CLI checks
// before it lists, renders or runs a registered definition (children
// included); wf.Main lints this way before every subcommand, so lint is not
// bypassable. A served definition names its model by tier (wf.ModelTier), never
// by a literal wf.Model.
func (d *Def) LintServed() []Problem {
	return append(d.Lint(), lintDef(d, "", map[*Def]bool{}, servedRules, false)...)
}

// ruleLiteralModel is RuleLiteralModel: an agent state whose dispatch sets a
// literal model.
func ruleLiteralModel(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		k, ok := n.kind.(*agentKind)
		if !ok {
			continue
		}
		if hintsOf(k.dispatch).Model != "" {
			out = append(out, n.problem(RuleLiteralModel, "agent sets a literal model via wf.Model; use wf.ModelTier"))
		}
	}
	return out
}

// ruleAgentBlockedRoute is RuleAgentBlockedRoute: every agent state resolves
// a Blocked route, its own OnClass(Blocked) or the definition's
// OnFailure(Blocked, ...). Without one a blocked failure halts the run.
func ruleAgentBlockedRoute(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		if _, ok := n.kind.(*agentKind); !ok {
			continue
		}
		if _, ok := classRoute(n, Blocked); !ok {
			out = append(out, n.problem(RuleAgentBlockedRoute, "agent state has no Blocked route: declare OnClass(wf.Blocked, ...) or d.OnFailure(wf.Blocked, ...)"))
		}
	}
	return out
}

// ruleAgentSelfEdge is RuleAgentSelfEdge: an agent state with a declared edge
// to itself. A retry is wf.Retry; a repair is its own state.
func ruleAgentSelfEdge(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		if _, ok := n.kind.(*agentKind); !ok {
			continue
		}
		for _, e := range n.edges {
			if e.to == n {
				out = append(out, n.problem(RuleAgentSelfEdge, "agent edge %q targets the agent itself; a retry is wf.Retry, a repair is its own state", e.label))
			}
		}
	}
	return out
}

// notes is the ordered note slice: findings that print and never fail the
// lint. A new note is one function appended here.
var notes = []rule{
	ruleMapParallelReview,
	ruleFetchCoverage,
}

// Notes runs the note rules over the definition and its children. Notes print
// and never fail the lint, so they are not part of Lint (about eighty callers
// treat any Lint problem as a failure); wf lint prints them after "ok".
func (d *Def) Notes() []Problem { return lintDef(d, "", map[*Def]bool{}, notes, false) }

// lintDef runs rs over d and its children. Build problems and kind-specific
// rules belong to the error pass only.
func lintDef(d *Def, prefix string, seen map[*Def]bool, rs []rule, errors bool) []Problem {
	return lintDefInScope(d, prefix, seen, rs, errors, nil, "")
}

// routeTo is the shortest path from d's entry to the state named name, by
// declared edges of d, as "Start -[label]-> A -[label]-> name": the first
// one found breadth-first in declaration order. "" when name is not one of
// d's states or no path reaches it (the unreachable rule reports that).
func routeTo(d *Def, name string) string {
	if len(d.nodes) == 0 {
		return ""
	}
	start := d.nodes[0]
	type step struct {
		from  *node
		label string
	}
	prev := map[*node]step{start: {}}
	queue := []*node{start}
	var found *node
	for len(queue) > 0 && found == nil {
		n := queue[0]
		queue = queue[1:]
		if n.name == name {
			found = n
			break
		}
		for _, e := range n.edges {
			if e.to == nil || e.to.def != d {
				continue
			}
			if _, ok := prev[e.to]; !ok {
				prev[e.to] = step{n, e.label}
				queue = append(queue, e.to)
			}
		}
	}
	if found == nil {
		return ""
	}
	parts := []string{found.name}
	for n := found; n != start; n = prev[n].from {
		parts = append(parts, "-["+prev[n].label+"]->", prev[n].from.name)
	}
	slices.Reverse(parts)
	return strings.Join(parts, " ")
}

// joinRoute continues a parent's route into a child's.
func joinRoute(parent, child string) string {
	switch {
	case parent == "":
		return child
	case child == "":
		return parent
	}
	return parent + " > " + child
}

func lintDefInScope(d *Def, prefix string, seen map[*Def]bool, rs []rule, errors bool, ancestors []*Def, route string) []Problem {
	seen[d] = true
	defer delete(seen, d) // a shared child must be checked in each ancestor context
	c := &lintCtx{d: d, states: inspectStates(d, d.name), ancestors: ancestors}
	var own []Problem
	if errors {
		own = append(own, d.problems...)
	}
	for _, r := range rs {
		own = append(own, r(c)...)
	}
	if errors {
		for _, n := range d.nodes {
			own = append(own, n.kind.lint(n)...)
		}
	}
	out := make([]Problem, 0, len(own))
	for _, p := range own {
		if p.Route == "" {
			if state, ok := strings.CutPrefix(p.Path, d.name+"/"); ok {
				if r := routeTo(d, state); r != "" {
					p.Route = joinRoute(route, r)
				}
			} else if p.Path == d.name {
				p.Route = route
			}
		}
		p.Path = prefix + p.Path
		out = append(out, p)
	}
	checkedHere := map[*Def]bool{}
	for _, ch := range d.children {
		if ch == nil || seen[ch] || checkedHere[ch] {
			continue
		}
		checkedHere[ch] = true
		host := ""
		for _, n := range d.nodes {
			if k, ok := n.kind.(interface{ childDef() *Def }); ok && k.childDef() == ch {
				host = routeTo(d, n.name)
				break
			}
		}
		out = append(out, lintDefInScope(ch, prefix+d.name+"/", seen, rs, errors, append(append([]*Def{}, ancestors...), d), joinRoute(route, host))...)
	}
	return out
}

func defProblem(d *Def, r RuleID, format string, args ...any) Problem {
	return Problem{Rule: r, Path: d.name, Msg: fmt.Sprintf(format, args...)}
}

func succs(n *node) []*node {
	var out []*node
	for _, e := range n.edges {
		if e.to != nil && e.to.def == n.def {
			out = append(out, e.to)
		}
	}
	return out
}

// reach returns the nodes reachable from n by one or more declared edges.
func reach(n *node) map[*node]bool {
	seen := map[*node]bool{}
	todo := succs(n)
	for len(todo) > 0 {
		x := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[x] {
			continue
		}
		seen[x] = true
		todo = append(todo, succs(x)...)
	}
	return seen
}

func ruleNoStates(c *lintCtx) []Problem {
	if len(c.d.nodes) == 0 {
		return []Problem{defProblem(c.d, RuleNoStates, "definition has no states")}
	}
	return nil
}

func ruleUnrouted(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		if !n.routed {
			out = append(out, n.problem(RuleUnrouted, "%s state has no routing (Route, RouteCases, Answers, Ready, Exits or Join)", n.kind.kindName()))
		}
	}
	return out
}

func ruleNoEdges(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		if !n.kind.terminal() && n.routed && len(n.edges) == 0 {
			out = append(out, n.problem(RuleNoEdges, "non-terminal state declares no edges"))
		}
	}
	return out
}

func ruleUnreachable(c *lintCtx) []Problem {
	if len(c.d.nodes) == 0 {
		return nil
	}
	start := c.d.nodes[0]
	r := reach(start)
	var out []Problem
	for _, n := range c.d.nodes[1:] {
		if n != start && !r[n] {
			out = append(out, n.problem(RuleUnreachable, "not reachable from start state %s", start.name))
		}
	}
	return out
}

func ruleNoTerminal(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		if n.kind.terminal() {
			continue
		}
		ok := false
		for x := range reach(n) {
			if x.kind.terminal() {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, n.problem(RuleNoTerminal, "no declared path to a terminal state"))
		}
	}
	return out
}

func ruleForeignEdge(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		for _, e := range n.edges {
			if e.to != nil && e.to.def != c.d {
				out = append(out, n.problem(RuleForeignEdge, "edge %q targets %s/%s, a state of another definition", e.label, e.to.def.name, e.to.name))
			}
		}
	}
	return out
}

func ruleDuplicateName(c *lintCtx) []Problem {
	var out []Problem
	seen := map[string]bool{}
	for _, n := range c.d.nodes {
		if seen[n.name] {
			out = append(out, n.problem(RuleDuplicateName, "state name %q is declared more than once (each state needs its own marker type; two type arguments of the same name, such as local types in different functions, name the same state)", n.name))
		}
		seen[n.name] = true
		if strings.ContainsAny(n.name, "·/") || strings.Count(n.name, "[") != strings.Count(n.name, "]") {
			out = append(out, n.problem(RuleDuplicateName, "state name %q is not a stable name: rename its marker type", n.name))
		}
	}
	return out
}

// ruleBudgetUnwired flags budgets that read a counter the definition does not
// declare, and a counter read by a budget that no edge or route candidate ever
// increments (route candidates are declared edges, so no effect hides in a
// closure).
func ruleBudgetUnwired(c *lintCtx) []Problem {
	declared := map[string]bool{}
	for _, k := range c.d.counters {
		declared[k.name] = true
	}
	inc := map[string]bool{}
	for i := range c.states {
		for _, u := range c.states[i].uses {
			for _, ef := range u.effects {
				if x, ok := ef.(incEffect); ok {
					inc[x.c.name] = true
				}
			}
		}
	}
	var out []Problem
	for i := range c.states {
		sv := &c.states[i]
		reported := map[string]bool{}
		for _, b := range sv.budgets {
			for _, k := range b.Counters() {
				switch {
				case reported[k.name]:
				case !declared[k.name]:
					reported[k.name] = true
					out = append(out, sv.node.problem(RuleBudgetUnwired, "budget %s reads counter %s, which this definition does not declare", b, k.name))
				case !inc[k.name]:
					reported[k.name] = true
					out = append(out, sv.node.problem(RuleBudgetUnwired, "budget %s reads counter %s, which no edge increments", b, k.name))
				}
			}
		}
	}
	return out
}

// ruleCounterScope flags a scoped counter that no declared edge or route
// candidate increments, and one whose scope counter is never incremented (it
// would never reset).
func ruleCounterScope(c *lintCtx) []Problem {
	inc := map[string]bool{}
	for i := range c.states {
		for _, u := range c.states[i].uses {
			for _, ef := range u.effects {
				if x, ok := ef.(incEffect); ok {
					inc[x.c.name] = true
				}
			}
		}
	}
	var out []Problem
	for _, k := range c.d.counters {
		if k.scope == "" {
			continue
		}
		if !inc[k.name] {
			out = append(out, defProblem(c.d, RuleCounterScope, "scoped counter %s is never incremented on a declared edge", k.label()))
		}
		if !inc[k.scope] {
			out = append(out, defProblem(c.d, RuleCounterScope, "scope counter %s of %s is never incremented on a declared edge", k.scope, k.label()))
		}
	}
	return out
}

// ruleCounterScopeDef flags a scoped counter whose scope counter is declared
// on another Def: an instance of this Def never sees that counter rise, so the
// scoped counter would never be zeroed.
func ruleCounterScopeDef(c *lintCtx) []Problem {
	var out []Problem
	for _, k := range c.d.counters {
		if k.scope != "" && k.scopeDef != c.d {
			out = append(out, defProblem(c.d, RuleCounterScopeDef, "scoped counter %s: scope counter %s is declared on definition %s, not on this one", k.label(), k.scope, k.scopeDef.name))
		}
	}
	return out
}

// ruleRouteCandidates flags free-form routes that declare no candidate edges
// and OnRoute rows that declare none, plus zero-Edge OnRoute candidates (a
// zero Edge passed to Route is already a build misuse).
func ruleRouteCandidates(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		if n.route != nil && len(n.route.cands) == 0 {
			out = append(out, n.problem(RuleRouteNoCandidates, "free-form Route declares no candidate edges"))
		}
		if n.cases == nil {
			continue
		}
		for _, cs := range n.cases.cases {
			if !cs.hasRoute {
				continue
			}
			if len(cs.cands) == 0 {
				out = append(out, n.problem(RuleRouteNoCandidates, "route case %q declares no candidate edges", cs.value))
			}
			for _, u := range cs.cands {
				if u.e == nil {
					out = append(out, n.problem(RuleBuild, "route case %q: route candidate is a zero Edge", cs.value))
				}
			}
		}
	}
	return out
}

// producedUpstream reports whether a's producer (or, for a child artifact, the
// Sub/Map state that exposes it) reaches n by declared edges.
func producedUpstream(a *artifactData, n *node) bool {
	src := a.producer
	if a.via != nil {
		src = a.via
	}
	return src != nil && reach(src)[n]
}

func ruleArtifactNotUpstream(c *lintCtx) []Problem {
	var out []Problem
	for i := range c.states {
		sv := &c.states[i]
		n := sv.node
		for _, a := range n.consumes {
			if !producedUpstream(a, n) {
				if a.producer != nil && a.producer.def != c.d && a.via == nil {
					out = append(out, n.problem(RuleArtifactNotUpstream, "consumes artifact %s from another definition: pass wf.Ref through the child input instead", a.name))
				} else {
					out = append(out, n.problem(RuleArtifactNotUpstream, "consumes artifact %s, which is not produced upstream", a.name))
				}
			}
		}
		for _, a := range sv.presents {
			if !producedUpstream(a, n) {
				out = append(out, n.problem(RuleArtifactNotUpstream, "presents artifact %s, which is not produced upstream", a.name))
			}
		}
	}
	return out
}

func ruleArtifactType(c *lintCtx) []Problem {
	var out []Problem
	byName := map[string]*artifactData{}
	for _, a := range c.d.artifacts {
		if prev, ok := byName[a.name]; ok && prev.typ != a.typ {
			out = append(out, a.producer.problem(RuleArtifactType, "artifact %s is declared as %s and as %s", a.name, prev.typ, a.typ))
		}
		byName[a.name] = a
	}
	for _, n := range c.d.nodes {
		for _, a := range n.consumes {
			if a.via == nil {
				continue
			}
			if ex := childExport(a); ex != nil && ex.typ != a.typ {
				out = append(out, n.problem(RuleArtifactType, "child artifact %s is %s in the child but consumed as %s", a.name, ex.typ, a.typ))
			}
		}
	}
	return out
}

// childExport finds the artifact the child definition exports under a's name.
func childExport(a *artifactData) *artifactData {
	ch, ok := a.via.kind.(interface{ childDef() *Def })
	if !ok || ch.childDef() == nil {
		return nil
	}
	for _, e := range ch.childDef().exports {
		if e.name == a.name {
			return e
		}
	}
	return nil
}

func ruleChildNotExported(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		for _, a := range n.consumes {
			if a.via != nil && childExport(a) == nil {
				out = append(out, n.problem(RuleChildNotExported, "artifact %s is not exported by the child of %s", a.name, a.via.name))
			}
		}
	}
	return out
}

func ruleEffectKey(c *lintCtx) []Problem {
	var out []Problem
	for i := range c.states {
		sv := &c.states[i]
		for _, u := range sv.uses {
			for _, ef := range u.effects {
				if x, ok := ef.(ExternalEffect); ok && x.Key == nil {
					out = append(out, sv.node.problem(RuleEffectKey, "external effect %s on edge %q has no idempotency key", x.effect(), useLabel(u)))
				}
			}
		}
	}
	return out
}

// ruleCommandKey: an Idempotent command needs a Key, and so does a detached
// one, which also needs either Idempotent or a DoneWhen check to recover from.
// A detached command cannot take Stdin.
func ruleCommandKey(c *lintCtx) []Problem {
	var out []Problem
	for i := range c.states {
		n := c.states[i].node
		k, ok := n.kind.(*commandKind)
		if !ok {
			continue
		}
		switch {
		case k.idempotent && len(k.key) == 0:
			out = append(out, n.problem(RuleCommandKey, "command is Idempotent but has no Key"))
		case k.detached > 0 && k.stdin != "":
			out = append(out, n.problem(RuleCommandKey, "a Detached command cannot take Stdin: the detached runner passes no input"))
		case k.detached > 0 && (len(k.key) == 0 || !k.idempotent && n.doneWhen == nil):
			out = append(out, n.problem(RuleCommandKey, "a detached command needs a Key and either Idempotent or a DoneWhen check"))
		}
	}
	return out
}

// effectIdentity names an effect by what makes two uses "the same effect":
// adapter.op|key for an external effect, ignoring args. Other effects are
// identified by their rendered form.
func effectIdentity(e Effect) string {
	x, ok := e.(ExternalEffect)
	if !ok {
		return e.effect()
	}
	k := "?"
	switch {
	case x.Key == nil:
	case x.Key.key() == "":
		k = "non-idempotent"
	default:
		k = x.Key.key()
	}
	return x.Adapter + "." + x.Op + "|" + k
}

// ruleHoldEffectOutside enforces the Hold rule: an external effect that rides
// an edge into a state holding lock L runs under L, so the same effect (same
// adapter.op and key) must not also ride an edge into a state without L.
func ruleHoldEffectOutside(c *lintCtx) []Problem {
	type site struct {
		sv    *StateView
		label string
		to    *node
	}
	byEffect := map[string][]site{}
	var order []string
	for i := range c.states {
		sv := &c.states[i]
		for _, u := range sv.uses {
			if u.e == nil || u.e.to == nil {
				continue
			}
			for _, ef := range u.effects {
				if _, ok := ef.(ExternalEffect); !ok {
					continue
				}
				k := effectIdentity(ef)
				if _, seen := byEffect[k]; !seen {
					order = append(order, k)
				}
				byEffect[k] = append(byEffect[k], site{sv, useLabel(u), u.e.to})
			}
		}
	}
	var out []Problem
	for _, k := range order {
		sites := byEffect[k]
		reported := map[site]bool{}
		for _, held := range sites {
			for _, l := range held.to.locks {
				for _, other := range sites {
					if reported[other] || slices.Contains(other.to.locks, l) {
						continue
					}
					reported[other] = true
					out = append(out, other.sv.node.problem(RuleHoldEffectOutside,
						"effect %s on edge %q runs outside lock %q, but the same effect on edge %q runs under it",
						k, other.label, l.name, held.label))
				}
			}
		}
	}
	return out
}

func ruleReservedPolicy(c *lintCtx) []Problem {
	var out []Problem
	for _, r := range c.d.policy.rules {
		switch {
		case r.gate == nil || r.gate.def != c.d:
			out = append(out, defProblem(c.d, RuleReservedPolicy, "policy rule answers %q for a gate outside this definition", r.answer))
		case r.gate.kind.kindName() == "gate" && gateReservation(r.gate) == Reserved:
			out = append(out, r.gate.problem(RuleReservedPolicy, "reserved gate has a policy rule (answer %q)", r.answer))
		}
	}
	return out
}

func gateReservation(n *node) Reservation {
	if g, ok := n.kind.(*gateKind); ok {
		return g.reservation
	}
	return 0
}

func ruleInputUnresolved(c *lintCtx) []Problem {
	var out []Problem
	params := map[string]bool{}
	for _, p := range c.d.params {
		params[p.name] = true
	}
	for i := range c.states {
		sv := &c.states[i]
		n := sv.node
		for _, in := range sv.inputs {
			switch in.kind {
			case "param":
				if !params[in.name] {
					out = append(out, n.problem(RuleInputUnresolved, "prompt input param %s is not declared on %s", in.name, c.d.name))
				}
			case "out":
				if in.node != nil && in.node.kind.kindName() == "map" {
					out = append(out, n.problem(RuleInputUnresolved, "prompt input Out(%s) is a Map: read its results with wf.Joined in a Compute, then feed that Compute's output to the prompt with wf.Out", in.name))
				} else if in.node == nil || in.node.def != c.d || !(in.node.kind.kindName() == "agent" || in.node.kind.kindName() == "compute" || in.node.kind.kindName() == "command") || !reach(in.node)[n] {
					out = append(out, n.problem(RuleInputUnresolved, "prompt input Out(%s) is not an agent, compute or command state upstream", in.name))
				}
			case "artifact":
				// a state's own artifact is a valid input: its path tells the agent where to write.
				if in.art.producer != n && !producedUpstream(in.art, n) {
					out = append(out, n.problem(RuleInputUnresolved, "prompt input artifact %s is not produced upstream", in.name))
				}
			case "child-input":
				if c.d.input == nil {
					out = append(out, n.problem(RuleInputUnresolved, "ChildInput() in %s, which is not a child definition", c.d.name))
				}
			}
		}
	}
	return out
}

func ruleSubRecursive(c *lintCtx) []Problem {
	var path []string
	var found bool
	seen := map[*Def]bool{}
	var walk func(x *Def)
	walk = func(x *Def) {
		for _, ch := range x.children {
			if found || ch == nil {
				continue
			}
			if ch == c.d {
				found = true
				path = append(path, ch.name)
				return
			}
			if seen[ch] {
				continue
			}
			seen[ch] = true
			path = append(path, ch.name)
			walk(ch)
			if !found {
				path = path[:len(path)-1]
			}
		}
	}
	walk(c.d)
	if !found {
		return nil
	}
	return []Problem{defProblem(c.d, RuleSubRecursive, "definition includes itself: %s -> %s", c.d.name, strings.Join(path, " -> "))}
}

// ruleChildNameDuplicate is RuleChildNameDuplicate, checked once at the root:
// two definitions in one tree share a name but differ. A name identifies one
// definition where the engine keys by it (status paths, the migration shape,
// adoption's lookup), so definitions sharing a name must be identical; the
// fold tells identical siblings apart by the state that spawned each.
func ruleChildNameDuplicate(c *lintCtx) []Problem {
	if len(c.ancestors) > 0 {
		return nil
	}
	var out []Problem
	for _, x := range childNameClashes(c.d) {
		out = append(out, defProblem(c.d, RuleChildNameDuplicate, "%s", x))
	}
	return out
}

// nameClash is one name two differing definitions of a tree share, with where
// each is used: the root's name, or the chain of Sub and Map states that runs
// it (root/State/State).
type nameClash struct{ name, first, second string }

func (x nameClash) String() string {
	return fmt.Sprintf("two definitions named %q differ, at %s and at %s: definitions that share a name must be identical, since the engine keys status paths and migration shapes by name. Give the child definitions distinct names", x.name, x.first, x.second)
}

// childNameClashes is each name that two differing definitions in d's tree
// (d and its Sub and Map children, recursively) share, in first-use order,
// with the first two places they are used. Definitions are compared by their
// structural hash and their resolved asset root (own or inherited), which the
// run also keys by name: a helper built twice by one constructor is one
// definition here, and closures, which no hash sees, are kept apart by the
// fold.
func childNameClashes(d *Def) []nameClash {
	type first struct {
		identity string
		where    string
	}
	byName := map[string]first{}
	hashes := map[*Def]string{}
	identity := func(x *Def, root string) string {
		h, ok := hashes[x]
		if !ok {
			h = hashView(inspectStructure(x))
			hashes[x] = h
		}
		return h + "\x00" + root
	}
	clashed := map[string]bool{}
	type visit struct {
		d    *Def
		root string
	}
	seen := map[visit]bool{}
	var out []nameClash
	var walk func(x *Def, where, inherited string)
	walk = func(x *Def, where, inherited string) {
		root := inherited
		if x.assets != "" {
			root = effectiveAssets(x, "")
		}
		id := identity(x, root)
		if f, ok := byName[x.name]; !ok {
			byName[x.name] = first{id, where}
		} else if f.identity != id && !clashed[x.name] {
			clashed[x.name] = true
			out = append(out, nameClash{x.name, f.where, where})
		}
		if seen[visit{x, root}] {
			return
		}
		seen[visit{x, root}] = true
		for _, n := range x.nodes {
			if k, ok := n.kind.(interface{ childDef() *Def }); ok && k.childDef() != nil {
				walk(k.childDef(), where+"/"+n.name, root)
			}
		}
	}
	walk(d, d.name, "")
	return out
}

// ruleMapParallelReview notes a Map capped by Parallel(n) with n > 1: the cap
// is a semantic limit, not a throughput knob.
func ruleMapParallelReview(c *lintCtx) []Problem {
	var out []Problem
	for i := range c.states {
		sv := &c.states[i]
		if sv.Kind != "map" || sv.Parallel <= 1 {
			continue
		}
		p := sv.node.problem(RuleMapParallelReview, "Parallel(%d) caps this Map; keep it only as a semantic limit (one merge at a time, a serial hot file): throughput is the n the coordinator passes to wf next", sv.Parallel)
		p.Severity = SeverityNote
		out = append(out, p)
	}
	return out
}
