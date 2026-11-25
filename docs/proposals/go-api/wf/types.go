// Package wf is the Go API for super-workflow definitions.
//
// A definition is plain Go that builds a *Def out of typed state handles. The
// package builds, lints, renders and hashes definitions and exports them as
// graphs. The executor (Run/Next/Submit and the journal) is declared in
// executor.go and implemented by Engine (engine.go). Nothing in this package
// ever executes a user route, check or predicate closure at lint or render
// time; only the Engine runs closures.
package wf

import (
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// Def is a workflow definition: the root of one lintable, renderable tree.
//
// Owns: its states, declared counters, registered Enums, Locks, run
// parameters, included child definitions (Sub and Map templates), the typed
// run options, the autonomy policy, the definition hash (computed from the
// same walk Render uses) and qualified state names across the tree.
// Does not: execute anything; build the graph's output formats (Render and
// Mermaid are plain functions over the shared walk).
// Extend by: adding states through the typed handle constructors; a new
// nested workflow is a new constructor returning a *Def.
type Def struct {
	assets     string
	work       string // Describe: what the definition's units do, for the bootstrap and briefs
	name       string
	version    int
	nodes      []*node
	counters   []Counter
	enums      []*enumData
	locks      []Lock
	params     []Param
	pathParams map[string]PathBase
	aliases    map[string]string // old param name to the param that replaced it
	runKey     []Param
	children   []*Def
	options    *optionsData
	policy     AutonomyPolicy
	artifacts  []*artifactData
	exports    []*artifactData
	input      reflect.Type
	result     reflect.Type
	migrations []Migration
	problems   []Problem
	defaults   []failureDefault // OnFailure, in declaration order
	sealed     bool             // seal ran: defaults are edges of the states
	sealOnce   sync.Once

	trackerOnce sync.Once
	hasTracker  bool
}

// failureDefault is one Def.OnFailure: the state a failure of class goes to
// from every state that declares no route of its own for it.
type failureDefault struct {
	class OutcomeClass
	to    *node
}

// OnFailure declares the definition-level route for an outcome class: every
// non-terminal state with no OnClass route of its own for class takes an edge
// to target, labelled "default <class> route", when it fails with that class
// (an attempt, a failed edge effect, an engine Blocked). The edges are part of
// each state's graph and so of the hash. The target itself is skipped. A
// later OnFailure for the same class replaces the earlier one. Declare it
// before the definition is used (rendered, linted, hashed or run).
func (d *Def) OnFailure(class OutcomeClass, target State) *Def {
	switch {
	case d.sealed:
		d.misuse(fmt.Sprintf("OnFailure(%s) after the definition was used", class))
		return d
	case target == nil:
		d.misuse(fmt.Sprintf("OnFailure(%s, nil)", class))
		return d
	case class == Accepted:
		d.misuse("OnFailure(accepted): accepted is not a failure")
		return d
	}
	for i, fd := range d.defaults {
		if fd.class == class {
			d.defaults[i].to = target.node()
			return d
		}
	}
	d.defaults = append(d.defaults, failureDefault{class: class, to: target.node()})
	return d
}

// OnFailures declares the same default target for several outcome classes.
// It is equivalent to calling OnFailure for each class in order: state-local
// routes win, and later defaults replace earlier ones for the same class.
func (d *Def) OnFailures(target State, classes ...OutcomeClass) *Def {
	for _, class := range classes {
		d.OnFailure(class, target)
	}
	return d
}

// seal turns the OnFailure defaults into edges of the states they apply to,
// once, the first time the definition is walked or run. A state's own OnClass
// route for a class wins over the default.
func (d *Def) seal() { d.sealOnce.Do(d.sealDefaults) }

func (d *Def) sealDefaults() {
	d.sealed = true
	for _, fd := range d.defaults {
		for _, n := range d.nodes {
			if n.kind.terminal() || n == fd.to {
				continue
			}
			if k, ok := n.kind.(*mapKind); ok {
				if _, own := k.byClass[fd.class]; own {
					continue
				}
			}
			if r := retryOf(n); r != nil {
				if _, own := r.byClass[fd.class]; own {
					continue
				}
			}
			e := &edgeData{from: n, to: fd.to, label: "default " + fd.class.String() + " route", class: fd.class}
			n.edges = append(n.edges, e)
			if n.defaults == nil {
				n.defaults = map[OutcomeClass]edgeUse{}
			}
			n.defaults[fd.class] = edgeUse{e: e}
		}
	}
}

// New starts a definition with the given name.
func New(name string) *Def { return &Def{name: name} }

// Assets declares the binary-resolved root of this definition's prompt and
// script files. Children inherit it unless they declare their own root.
// Only referenced files contribute to the hash; the installation path does not.
func (d *Def) Assets(dir string) *Def {
	d.assets = dir
	return d
}

// Name returns the definition name.
func (d *Def) Name() string { return d.name }

// Describe says, as one short phrase, what the definition's units do beyond
// the workflow commands ("read and write the repo's bd tracker as their
// prompts say, ... and run the project's build and tests"): the coordinator
// bootstrap and every spawn brief state it next to the commands, so a
// permission checker reading only that context sees the expected work. It is
// presentation, not routing: it is not part of the definition's hash. Every
// brief carries it, so it is short: over maxDescribe characters, or on more
// than one line, is a build misuse.
func (d *Def) Describe(work string) *Def {
	d.work = strings.TrimSuffix(strings.TrimSpace(work), ".")
	if n := utf8.RuneCountInString(d.work); n > maxDescribe || strings.Contains(d.work, "\n") {
		d.misuse(fmt.Sprintf("Describe: %d characters on one line at most, one short phrase (it is in every spawn brief); got %d", maxDescribe, n))
	}
	return d
}

// maxDescribe bounds Describe: the bootstrap stays under 3 KB and a brief
// stays short enough that next still offers many units per call.
const maxDescribe = 128

// Version declares the definition version; it is pinned together with the hash.
func (d *Def) Version(v int) *Def {
	d.version = v
	return d
}

// Param declares a run parameter that prompts and routes may read.
func (d *Def) Param(name string) Param {
	p := Param{name: name, def: d}
	d.params = append(d.params, p)
	return p
}

// RunKey names the parameters that form the run lock and resume-discovery key.
func (d *Def) RunKey(params ...Param) *Def {
	d.runKey = append(d.runKey, params...)
	return d
}

// Counter declares a monotone, journaled counter. With ScopedTo it is zeroed
// whenever its scope counter rises; a zero or already-scoped scope is a
// RuleBuild problem and leaves the counter plain.
func (d *Def) Counter(name string, opts ...CounterOption) Counter {
	c := Counter{name: name, def: d}
	for _, o := range opts {
		switch {
		case o.scope.name == "":
			d.misuse(fmt.Sprintf("counter %q: ScopedTo needs a declared counter", name))
		case o.scope.scope != "":
			d.misuse(fmt.Sprintf("counter %q: scope %q is itself scoped", name, o.scope.label()))
		default:
			c.scope = o.scope.name
			c.scopeDef = o.scope.def
		}
	}
	d.counters = append(d.counters, c)
	return c
}

// Lock declares a named lock with the given scope.
func (d *Def) Lock(name string, scope LockScope) Lock {
	l := Lock{name: name, scope: scope}
	d.locks = append(d.locks, l)
	return l
}

// Migrations declares the state mappings from older definition versions.
func (d *Def) Migrations(ms ...Migration) *Def {
	d.migrations = append(d.migrations, ms...)
	return d
}

func (d *Def) add(name string, k stepKind) *node {
	n := &node{def: d, name: name, kind: k}
	d.nodes = append(d.nodes, n)
	return n
}

// addMarker adds the state whose marker type is S: its stable name
// (stateName), and the name an engine before stable names gave it
// (legacyStateNameOf), which an adoption of a run recorded then matches.
func addMarker[S any](d *Def, k stepKind) *node {
	n := d.add(stateName[S](), k)
	n.legacy = legacyStateNameOf[S]()
	return n
}

// misuse records definition-time misuse as a RuleBuild problem; it never panics.
func (d *Def) misuse(msg string) {
	d.problems = append(d.problems, Problem{Rule: RuleBuild, Path: d.name, Msg: msg})
}

// State is any state handle (Agent, Command, Compute, Gate, Wait, Map, Sub,
// Terminal). It is sealed: only this package defines handles.
type State interface{ node() *node }

type visitLimit struct {
	max  Budget
	over edgeUse
}

type node struct {
	def       *Def
	name      string
	legacy    string // the name an engine before stable names derived (legacyStateNameOf)
	kind      stepKind
	edges     []*edgeData
	routed    bool
	locks     []Lock
	consumes  []*artifactData
	maxVisits *visitLimit
	cases     *caseView  // declared case table, when the state routes through Cases
	route     *routeData // declared candidates, when the state routes through a Route closure
	doneWhen  *doneWhenData
	defaults  map[OutcomeClass]edgeUse // Def.OnFailure edges this state takes (seal)
	measure   string                   // Measure: the strict progress measure bounding its cycles
}

// stepKind is the per-kind behaviour of one state (agent, command, compute,
// gate, wait, map, sub, terminal).
//
// Owns: everything that differs by kind: the kind's render line and Mermaid
// annotation, its contribution to the definition hash, its kind-specific
// lint rules (an agent needs a retry policy, gate answers cover its edges, a
// map has a join policy, a sub maps every child terminal) and whether it may
// have out-edges.
// Does not: hold graph structure (edges and reachability belong to the node
// and the graph walk) or run user route or check closures.
// Extend by: a new kind is one implementation of this interface, created in
// its handle constructor; callers never switch on a kind string.
type stepKind interface {
	kindName() string
	renderHead(n *node) string
	hashInto(n *node, w io.Writer)
	lint(n *node) []Problem
	terminal() bool
	// view fills the kind's part of the shared walk (Inspect); Render, Mermaid
	// and Hash are functions of that view.
	view(n *node, v *StateView)
	// execute runs the state's deterministic work for the Engine (issue an
	// agent instruction, run a compute, end at a terminal). A kind the Engine
	// does not run yet classes the step Blocked.
	execute(c *cycle, in *instance, n *node) error
}

// RuleID names one lint rule. Callers (tests, bad/ examples) match on the rule
// id, never on message text; a new rule is one id here plus one function in
// the rule slice (lint.go).
type RuleID string

// The rule-id vocabulary.
const (
	RuleBuild               RuleID = "build" // definition-time misuse recorded while building
	RuleDispatchHint        RuleID = "dispatch-hint"
	RuleNoStates            RuleID = "no-states"
	RuleUnrouted            RuleID = "unrouted"
	RuleNoEdges             RuleID = "no-edges"
	RuleUnreachable         RuleID = "unreachable"
	RuleNoTerminal          RuleID = "cannot-reach-terminal"
	RuleForeignEdge         RuleID = "foreign-edge"
	RuleDuplicateName       RuleID = "duplicate-name"
	RuleBudgetUnwired       RuleID = "budget-unwired"
	RulePromptCandidates    RuleID = "prompt-candidates"
	RuleArtifactNotUpstream RuleID = "artifact-not-upstream"
	RuleArtifactType        RuleID = "artifact-type-mismatch"
	RuleEffectKey           RuleID = "effect-key"
	RuleCommandKey          RuleID = "command-key"
	RuleLiteralModel        RuleID = "literal-model"
	RuleAgentSelfEdge       RuleID = "agent-self-edge"
	RuleReservedPolicy      RuleID = "reserved-gate-policy"
	RuleInputUnresolved     RuleID = "input-unresolved"
	RuleAgentRetry          RuleID = "agent-retry"
	RuleAgentBlockedRoute   RuleID = "agent-blocked-route" // an agent state resolves no Blocked route (own or Def.OnFailure)
	RuleComputeUnavailable  RuleID = "compute-unavailable"
	RuleGateAnswers         RuleID = "gate-answers"
	RuleCaseUncovered       RuleID = "case-uncovered"
	RuleCaseUnknownValue    RuleID = "case-unknown-value"
	RuleCaseNoOtherwise     RuleID = "case-no-otherwise"
	RuleCaseDuplicate       RuleID = "case-duplicate"
	RuleRouteNoCandidates   RuleID = "route-no-candidates"
	RuleCounterScope        RuleID = "counter-scope"
	RuleCounterScopeDef     RuleID = "counter-scope-def"
	RuleMapItemKey          RuleID = "map-item-key"
	RuleMapJoin             RuleID = "map-join"
	RuleMapPartialFailure   RuleID = "map-partial-failure"
	RuleMapWriteOverlap     RuleID = "map-write-overlap"
	RuleSubExitUnmapped     RuleID = "sub-exit-unmapped"
	RuleSubResultType       RuleID = "sub-result-type"
	RuleSubRecursive        RuleID = "sub-recursive"
	RuleChildNameDuplicate  RuleID = "child-name-duplicate"
	RuleChildNotExported    RuleID = "child-artifact-not-exported"
	RuleHoldEffectOutside   RuleID = "hold-effect-outside"
	RuleMapParallelReview   RuleID = "map-parallel-review" // a note, not an error
	RuleAgentOutputPath     RuleID = "agent-output-path"
	RuleArtifactName        RuleID = "artifact-name"
	RuleMaxlenTag           RuleID = "maxlen-tag"
	RuleArtifactTemplated   RuleID = "artifact-templated" // staged (LintStaged): a note until every client allocates
)

// Severity says whether a finding fails the lint (an error) or only prints (a note).
type Severity int

const (
	SeverityError Severity = iota // the zero value: every existing rule
	SeverityNote
)

// Problem is one lint finding: a rule id, the qualified state path
// (def/child/State) and a message.
//
// Owns: the shape every rule emits. Graph-wide rules live in the rule slice
// (lint.go); kind-specific rules live in stepKind.
// Does not: carry anything callers should match on except Rule.
// Extend by: a new rule is one function appended to the rule slice.
type Problem struct {
	Rule     RuleID
	Path     string
	Msg      string
	Severity Severity
	// Route is the shortest path from the entry to the state the finding is
	// at, as states and edge labels ("Start -[label]-> A"), through the
	// parent states that run a child definition ("... -> Host > ChildStart
	// -> ..."); "" for a finding about a definition as a whole at the root.
	Route string `json:",omitempty"`
}

// String renders "<rule> <path>: <msg> (path: <route>)", prefixed "note "
// for a note.
func (p Problem) String() string {
	s := fmt.Sprintf("%s %s: %s", p.Rule, p.Path, p.Msg)
	if p.Route != "" {
		s += " (path: " + p.Route + ")"
	}
	if p.Severity == SeverityNote {
		return "note " + s
	}
	return s
}

// Outcome is the result recorded by a terminal state. Definition-specific stop
// reasons are terminal marker types, not new Outcomes.
type Outcome string

// Terminal outcomes. Partial is a Success terminal's outcome when its
// PartialWhen predicate holds: the run delivered its result, but not ready.
const (
	Success Outcome = "success"
	Partial Outcome = "partial"
	Stopped Outcome = "stopped"
	Stalled Outcome = "stalled"
)

// Delivered reports whether o ends a run or child with its result delivered:
// Success, or Partial (delivered, not ready). Stopped and Stalled are stops.
func (o Outcome) Delivered() bool { return o == Success || o == Partial }

// Param is a declared run parameter. It is a typed prompt Input.
type Param struct {
	name string
	def  *Def
}

// Path declares that this parameter names a filesystem path. Relative values
// use base; absolute values inside the run or workspace become portable values.
func (p Param) Path(base PathBase) Param {
	if p.def == nil {
		return p
	}
	if p.def.sealed {
		p.def.misuse("Param.Path after the definition was used")
		return p
	}
	if base != WorkspaceRelative && base != RunRelative {
		p.def.misuse(fmt.Sprintf("parameter %s has invalid path base %q", p.name, base))
		return p
	}
	if old, ok := p.def.pathParams[p.name]; ok && old != base {
		p.def.misuse(fmt.Sprintf("parameter %s has conflicting path bases %q and %q", p.name, old, base))
		return p
	}
	if p.def.pathParams == nil {
		p.def.pathParams = map[string]PathBase{}
	}
	p.def.pathParams[p.name] = base
	return p
}

// Alias accepts old as another name of the param for one release: a run
// given old and not p gets old's value under p. It returns p.
func (p Param) Alias(old ...string) Param {
	if p.def.aliases == nil {
		p.def.aliases = map[string]string{}
	}
	for _, o := range old {
		p.def.aliases[o] = p.name
	}
	return p
}

// Name returns the parameter name.
func (p Param) Name() string { return p.name }

func (p Param) inputRef() inputDesc { return inputDesc{kind: "param", name: p.name} }

// LockScope is the closed set of lock scopes.
type LockScope int

// Lock scopes.
const (
	LockRun LockScope = iota + 1
	LockRunKey
)

// Lock is a named lock a state can hold while it runs.
type Lock struct {
	name  string
	scope LockScope
}

// Name returns the lock name.
func (l Lock) Name() string { return l.name }

// Hold makes the state hold the lock while it runs (single-flight merge).
//
// Lock rule: the executor acquires the state's locks before it runs the
// effects of the edge taken into the state, holds them while the state runs,
// and releases them once the state's outgoing edge is chosen, before that
// edge's effects run (unless its target holds the same lock). So effects on an
// edge into a held state run under its locks. Lint rule hold-effect-outside
// keeps that meaningful: the same external effect may not also ride an edge
// into a state that does not hold the lock.
func Hold(s State, l Lock) {
	n := s.node()
	n.locks = append(n.locks, l)
}

// stateName derives a state's name from its marker type: WritePlan ->
// "WritePlan", and a generic marker by its base name and type arguments,
// each cut to its own name (cleanTypeName): codeSnapshotWritten[DrainedSnapshot],
// never the compiler's string for a function-local type argument
// ("...example.Snapshot·19]"), whose ·N moves when the enclosing
// function's local types change and would rename the state.
func stateName[S any]() string {
	return cleanTypeName(fmt.Sprintf("%T", *new(S)))
}

// typeName is an output type's name as the definition hash and the run's
// recorded shape carry it: the %T string cut after its last '.'. It is kept
// as it was, so adopting a run recorded before stable state names compares
// the same result types; only state names (stateName) are cleaned.
func typeName[O any]() string {
	t := fmt.Sprintf("%T", *new(O))
	return t[strings.LastIndex(t, ".")+1:]
}

// cleanTypeName is a %T type string cut to a stable name: each named type
// to its last path segment with any ·N stripped, recursively through type
// arguments, pointers and slices.
func cleanTypeName(t string) string {
	switch {
	case strings.HasPrefix(t, "*"):
		return "*" + cleanTypeName(t[1:])
	case strings.HasPrefix(t, "[]"):
		return "[]" + cleanTypeName(t[2:])
	}
	head, args := t, ""
	if i := strings.IndexByte(t, '['); i > 0 && strings.HasSuffix(t, "]") {
		head, args = t[:i], t[i+1:len(t)-1]
	}
	head = head[strings.LastIndex(head, ".")+1:]
	if i := strings.Index(head, "·"); i >= 0 {
		head = head[:i]
	}
	if args == "" {
		return head
	}
	parts := splitTypeArgs(args)
	for i, p := range parts {
		parts[i] = cleanTypeName(strings.TrimSpace(p))
	}
	return head + "[" + strings.Join(parts, ",") + "]"
}

// splitTypeArgs splits a type argument list at its top-level commas.
func splitTypeArgs(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// legacyStateNameOf is the name an engine before stable names gave the state
// whose marker type is S: the %T string cut after its last '.' (so
// "Item·19]]" for a nested generic marker over a function-local type).
func legacyStateNameOf[S any]() string {
	t := fmt.Sprintf("%T", *new(S))
	return t[strings.LastIndex(t, ".")+1:]
}

// legacyOrdinal matches the compiler's ordinal of a function-local type
// (·19): it moves between builds, so legacy names are compared without it.
var legacyOrdinal = regexp.MustCompile(`·[0-9]+`)

// normalizeLegacy is a legacy state name with its ordinals made one
// wildcard, so a recorded name matches a candidate whatever build recorded it.
func normalizeLegacy(name string) string { return legacyOrdinal.ReplaceAllString(name, "·") }

// legacyStateName reports whether name is a state name an engine before
// stable names recorded for a generic marker: the %T string cut after its
// last '.', its last type argument's identifier with any ·N and the closing
// brackets ("DrainedSnapshot·19]" over a function-local type, "Snapshot]"
// over a package-level one). It returns that identifier.
func legacyStateName(name string) (ident string, ok bool) {
	if !strings.HasSuffix(name, "]") || strings.Contains(name, "[") {
		return "", false
	}
	ident = strings.TrimRight(name, "]")
	if i := strings.Index(ident, "·"); i >= 0 {
		ident = ident[:i]
	}
	return ident, ident != ""
}
