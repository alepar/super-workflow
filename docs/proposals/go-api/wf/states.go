package wf

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------- kinds

// base gives every kind its name and the defaults a kind need not override.
type base struct{ name string }

func (b base) kindName() string              { return b.name }
func (b base) renderHead(n *node) string     { return n.name + " " + b.name }
func (b base) hashInto(n *node, w io.Writer) { io.WriteString(w, b.name) }
func (b base) terminal() bool                { return false }
func (b base) view(n *node, v *StateView)    {}

type agentKind struct {
	base
	outType    string
	outT       reflect.Type // the output type, for the exec lint
	prompt     Prompt
	retry      *retryData
	checkNames []string
	observes   []ObservationDesc
	inputs     []ObservationDesc // Agent.Observes: the observations the staleness token covers
	route      any
	dispatch   []DispatchOption
	gitAuth    *GitAuthority
	coverage   bool // ReadsFetchCoverage: the definition reads its fetch coverage
	ops        kindOps
}

type commandKind struct {
	base
	argv  []string
	dir   string // "" runs in the bound {workdir}, else the workspace root
	retry *retryData
	route any
	ops   kindOps

	key        []string      // idempotency key parts ({name} placeholders); empty: no key
	idempotent bool          // safe to run again: recovery re-runs it with no Check
	detached   time.Duration // zero: inline; set by .Detached
	stdin      string        // template of the text the command reads on stdin; empty: none
}

type computeKind struct {
	base
	outType  string
	outT     reflect.Type // the output type, for the exec lint
	fn       any
	retry    *retryData
	observes []ObservationDesc
	route    any
	ops      kindOps
}

type gateKind struct {
	base
	intValues         map[string]*gateIntSpec
	intErrors         []string
	equivalentAnswers string
	question          string
	answers           *enumData
	reservation       Reservation
	presents          []*artifactData
	timeout           *edgeUse
	ops               kindOps
}

type waitKind struct {
	base
	obs     ObservationDesc
	cond    string
	ready   any
	readyTo *edgeUse
	poll    time.Duration
	timeout *edgeUse
	ops     kindOps
}

type subKind struct {
	base
	child    *Def
	input    any
	exits    []exitData
	retry    *retryData
	observes []ObservationDesc
	ops      kindOps
}

type mapKind struct {
	base
	src        sourceData
	key        any
	keyType    string
	child      *Def
	parallel   int
	writes     []string
	itemWrites any
	lease      time.Duration
	sibling    SiblingPolicy
	requery    bool
	join       *JoinPolicy
	met        *edgeUse
	partial    *edgeUse
	byClass    map[OutcomeClass]edgeUse
	ops        kindOps
}

type terminalKind struct {
	base
	outcome Outcome
	partial func(Ctx) bool // PartialWhen: a Success terminal ends Partial when it holds
	intent  string
	result  reflect.Type
	yield   any
	ops     kindOps
}

func (terminalKind) terminal() bool { return true }

func edgeTo[S any](n *node, target State, label string) Edge[S] {
	return Edge[S]{e: n.to(target, label)}
}

// ---------------------------------------------------------------- agent

// Agent is a step performed by the calling agent; O is its typed output,
// decoded and validated from the submission. S is the state's marker type (an
// empty struct; its name is the state name).
//
// Owns: a prompt template with its typed input references, the mandatory retry
// policy, named output checks and the routing out of the accepted output.
// Does not: bind prompt values (executor territory).
// Extend by: a new output check is a Checkf; a new route is a Cases table.
type Agent[S, O any] struct {
	n      *node
	k      *agentKind
	checks []Check[O]
	route  func(O, Ctx) Edge[S]
}

// NewAgent declares an agent step.
func NewAgent[S, O any](d *Def) *Agent[S, O] {
	k := &agentKind{base: base{"agent"}, outType: typeName[O](), outT: reflect.TypeFor[O]()}
	a := &Agent[S, O]{n: addMarker[S](d, k), k: k}
	k.ops = agentOps[S, O]{a}
	return a
}

func (a *Agent[S, O]) node() *node { return a.n }

// To declares an edge from this state.
func (a *Agent[S, O]) To(target State, label string) Edge[S] { return edgeTo[S](a.n, target, label) }

// Spec sets the prompt template with typed inputs, the mandatory retry policy
// and the named checks.
func (a *Agent[S, O]) Spec(p Prompt, retry RetryPolicy[S], checks ...Check[O]) {
	a.k.prompt, a.k.retry, a.checks = p, retry.d, checks
	// An observed prompt input is an input observation: the staleness token.
	for _, in := range p.inputs {
		if d := in.inputRef(); d.obs != nil {
			a.k.inputs = append(a.k.inputs, *d.obs)
		}
	}
	for _, c := range checks {
		a.k.checkNames = append(a.k.checkNames, c.Name)
		for _, o := range c.observes {
			a.k.observes = append(a.k.observes, o.Desc())
		}
	}
}

// Observes declares the agent's input observations: what the instruction is
// built on (a bead's status, a branch head), read when it is issued and again
// when its result comes in. When one changed, the work was done against a
// world that moved: the step is re-issued with nothing spent (at most
// StaleReissues times per state visit, then the attempt is spent), or takes
// its OnClass(Stale) route. Check observations never feed this token: they
// read what the agent itself changes. An agent with no Observes is never
// stale unless it routes Stale itself. Inputs show in the state view and the
// hash.
func (a *Agent[S, O]) Observes(os ...Reader) *Agent[S, O] {
	for _, o := range os {
		a.k.inputs = append(a.k.inputs, o.Desc())
	}
	return a
}

// StaleReissues bounds the free re-issues of one state visit on changed input
// observations: a world that keeps changing then spends attempts.
const StaleReissues = 3

// Dispatch sets the agent's dispatch hints (wf.Model, wf.Effort, wf.AgentType).
// They are definition data: rendered, hashed, linted (RuleDispatchHint) and
// copied into every Instruction issued for this state.
func (a *Agent[S, O]) Dispatch(opts ...DispatchOption) *Agent[S, O] {
	a.k.dispatch = append(a.k.dispatch, opts...)
	return a
}

// GitAuthority declares the git authority of the agent's units. It is
// definition data the client sets; the Engine copies it, unchanged, into every
// Instruction issued for this state and never computes it.
func (a *Agent[S, O]) GitAuthority(g GitAuthority) *Agent[S, O] {
	g.Refs, g.Allowed, g.Forbidden = slices.Clone(g.Refs), slices.Clone(g.Allowed), slices.Clone(g.Forbidden)
	a.k.gitAuth = &g
	return a
}

// Route chooses the next edge from the accepted output (free-form). The
// candidates must list every edge fn may return: the walk never runs fn, so
// render, hash and lint see the candidates and their effects, and the executor
// refuses a returned edge outside them (see Executor, Declared obligations).
func (a *Agent[S, O]) Route(fn func(out O, c Ctx) Edge[S], candidates ...Edge[S]) {
	a.route, a.n.routed = fn, true
	a.n.route = &routeData{cands: candidateUses(a.n.def, a.n.name, candidates)}
}

// Then routes every accepted output along e, the state's one way out: a.Then(e)
// is a.Route(func(O, Ctx) Edge[S] { return e }, e). e keeps its effects.
func (a *Agent[S, O]) Then(e Edge[S]) {
	a.Route(func(O, Ctx) Edge[S] { return e }, e)
}

// RouteCases routes through a declared exhaustive case table.
func (a *Agent[S, O]) RouteCases(t CaseTable[S, O]) { a.n.cases, a.n.routed = t.view(), true }

// ---------------------------------------------------------------- command

// RunResult is what a command step observed.
type RunResult struct {
	Exit           int
	Stdout, Stderr string
	// Output is the path of the command's full output when a stream was cut
	// (artifact-identity C): Stdout and Stderr then keep their tails, where
	// failures print, after an "... <N bytes elided> ..." line. The cut copy
	// is a view; Output is the evidence.
	Output string `json:",omitempty"`
}

// OutputTail is how many bytes of each stream a RunResult keeps; a longer
// stream keeps its last OutputTail bytes behind ElisionLine.
const OutputTail = 64 << 10

// ElisionLine is the line a cut stream starts with.
func ElisionLine(n int) string { return fmt.Sprintf("... <%d bytes elided> ...\n", n) }

// CutStream is s kept to its last OutputTail bytes, marked, and whether it was
// cut.
func CutStream(s string) (string, bool) {
	if len(s) <= OutputTail {
		return s, false
	}
	return ElisionLine(len(s)-OutputTail) + s[len(s)-OutputTail:], true
}

// FullOutput is the text of a command's full output file: each stream under
// its own header.
func FullOutput(stdout, stderr string) []byte {
	return []byte("==> stdout <==\n" + stdout + "\n==> stderr <==\n" + stderr)
}

// ExitAs extracts the exit code as an Enum-typed value for Cases:
// wf.ExitAs[MyExit].
func ExitAs[V ~int](r RunResult) V { return V(r.Exit) }

// Command is a deterministic step the executor runs itself (no agent).
//
// Owns: its argv and the routing of its result (usually Cases over an exit-code
// Enum).
// Does not: run anything at lint or render time.
// Extend by: a new script is one contract declaration in a client package.
type Command[S any] struct {
	n     *node
	k     *commandKind
	route func(RunResult, Ctx) Edge[S]
}

// NewCommand declares a command step.
func NewCommand[S any](d *Def, argv ...string) *Command[S] {
	k := &commandKind{base: base{"command"}, argv: argv}
	c := &Command[S]{n: addMarker[S](d, k), k: k}
	k.ops = commandOps[S]{c}
	return c
}

func (c *Command[S]) node() *node { return c.n }

// To declares an edge from this state.
func (c *Command[S]) To(target State, label string) Edge[S] { return edgeTo[S](c.n, target, label) }

// Route chooses the next edge from the result (free-form). The candidates must
// list every edge fn may return (see Agent.Route).
func (c *Command[S]) Route(fn func(r RunResult, c Ctx) Edge[S], candidates ...Edge[S]) {
	c.route, c.n.routed = fn, true
	c.n.route = &routeData{cands: candidateUses(c.n.def, c.n.name, candidates)}
}

// Then routes every result along e, the state's one way out (see Agent.Then).
func (c *Command[S]) Then(e Edge[S]) {
	c.Route(func(RunResult, Ctx) Edge[S] { return e }, e)
}

// RouteExit takes zero when the command exits with code 0, and nonzero for
// every other exit code. Both edges are declared route candidates. Execution
// errors still follow the command's failure routes and retry policy.
func (c *Command[S]) RouteExit(zero, nonzero Edge[S]) {
	c.Route(func(r RunResult, _ Ctx) Edge[S] {
		if r.Exit == 0 {
			return zero
		}
		return nonzero
	}, zero, nonzero)
}

// RouteCases routes through a declared exhaustive case table.
func (c *Command[S]) RouteCases(t CaseTable[S, RunResult]) { c.n.cases, c.n.routed = t.view(), true }

// Retry sets an optional retry policy (unavailable and transient classes).
func (c *Command[S]) Retry(p RetryPolicy[S]) { c.k.retry = p.d }

// In runs the command in dir (relative to the workspace root, {name}
// placeholders bound) instead of the bound {workdir}. In(".") is the explicit
// base-checkout operation. It returns c.
func (c *Command[S]) In(dir string) *Command[S] {
	c.k.dir = dir
	return c
}

// Key gives the command an idempotency key, built from parts ({name}
// placeholders bound at run time) like wf.KeyOf. Recovery then asks the shell
// runtime's Check instead of falling back to manual verification. It returns c.
func (c *Command[S]) Key(parts ...string) *Command[S] {
	c.k.key = parts
	return c
}

// Idempotent declares the command safe to run again: recovery re-runs it with
// no Check. It needs a Key (lint command-key). It returns c.
func (c *Command[S]) Idempotent() *Command[S] {
	c.k.idempotent = true
	return c
}

// Detached runs the command under the binary's detached runner (it survives
// the CLI call that started it); max bounds it (default 60 min). It needs a
// Key and Idempotent or a DoneWhen (lint command-key). Render and hash show it
// only when set. It returns c.
func (c *Command[S]) Detached(max ...time.Duration) *Command[S] {
	c.k.detached = 60 * time.Minute
	if len(max) > 0 {
		c.k.detached = max[0]
	}
	return c
}

// Stdin gives the command the bound tmpl ({name} placeholders, as in an argv
// element) on its standard input. The bound text is journaled with the effect
// intent, so recovery re-runs the command with the same input. Inline only: a
// Detached command with Stdin is a command-key lint error. Render and hash show
// it only when set. It returns c.
func (c *Command[S]) Stdin(tmpl string) *Command[S] {
	c.k.stdin = tmpl
	return c
}

// ---------------------------------------------------------------- compute

// Compute is a deterministic computed output (no agent, no shell).
//
// Owns: a pure function of Ctx, the routing out of its output, the
// Observations its closure reads (Observes) and the per-OutcomeClass routes
// taken when such a read is unavailable (Retry).
// Does not: shell out or read the world except through Ctx, or read an
// Observation it did not declare.
// Extend by: Route, RouteCases, Observes or Retry.
type Compute[S, O any] struct {
	n     *node
	k     *computeKind
	route func(O, Ctx) Edge[S]
}

// NewCompute declares a compute step. fn must be pure: the engine may run it
// again after a crash.
func NewCompute[S, O any](d *Def, fn func(c Ctx) (O, error)) *Compute[S, O] {
	k := &computeKind{base: base{"compute"}, outType: typeName[O](), outT: reflect.TypeFor[O](), fn: fn}
	c := &Compute[S, O]{n: addMarker[S](d, k), k: k}
	k.ops = computeOps[S, O]{c}
	return c
}

func (c *Compute[S, O]) node() *node { return c.n }

// To declares an edge from this state.
func (c *Compute[S, O]) To(target State, label string) Edge[S] {
	return edgeTo[S](c.n, target, label)
}

// Route chooses the next edge from the computed output (free-form). The
// candidates must list every edge fn may return (see Agent.Route).
func (c *Compute[S, O]) Route(fn func(out O, c Ctx) Edge[S], candidates ...Edge[S]) {
	c.route, c.n.routed = fn, true
	c.n.route = &routeData{cands: candidateUses(c.n.def, c.n.name, candidates)}
}

// Then routes every computed output along e, the state's one way out (see
// Agent.Then).
func (c *Compute[S, O]) Then(e Edge[S]) {
	c.Route(func(O, Ctx) Edge[S] { return e }, e)
}

// Retry sets the compute's per-OutcomeClass routes. An error returned by the
// closure (an unavailable Observed read) is classed Unavailable and takes the
// policy's Unavailable route (OnClass or OnClassBackoff); any other failed
// attempt consumes the attempt budget, then the exhausted edge.
func (c *Compute[S, O]) Retry(p RetryPolicy[S]) { c.k.retry = p.d }

// Observes declares the Observations the closure reads (argument templates,
// rendered and hashed like Check.Observes). Reading an undeclared Observation
// is a run-time Blocked outcome.
func (c *Compute[S, O]) Observes(os ...Reader) *Compute[S, O] {
	for _, o := range os {
		c.k.observes = append(c.k.observes, o.Desc())
	}
	return c
}

// RouteCases routes through a declared exhaustive case table.
func (c *Compute[S, O]) RouteCases(t CaseTable[S, O]) { c.n.cases, c.n.routed = t.view(), true }

// ---------------------------------------------------------------- gate

// Reservation is the closed set of gate reservations.
type Reservation int

// Gate reservations.
const (
	Reserved Reservation = iota + 1
	PolicyAnswerable
)

// Gate is a human (or policy) decision whose answer set is a registered
// Enum[A] and whose answers are its routes. A is the answer type, which the
// no-generic-methods rule needs so Answers can be a method.
//
// Owns: the question, the Enum answer set, presented Artifacts, a timeout edge
// carrying a declared time.Duration (rendered beside the edge) and the
// reservation (Reserved or PolicyAnswerable).
// Does not: carry a policy answer itself (the Def's AutonomyPolicy does), and a
// reserved gate never gets one.
// Extend by: a new answer is a new Enum value plus a Case.
type Gate[S any, A comparable] struct {
	n *node
	k *gateKind
}

// NewGate declares a gate.
func NewGate[S any, A comparable](d *Def, question string, answers Enum[A], r Reservation) *Gate[S, A] {
	k := &gateKind{base: base{"gate"}, question: question, answers: answers.e, reservation: r}
	g := &Gate[S, A]{n: addMarker[S](d, k), k: k}
	k.ops = gateOps[S, A]{g}
	return g
}

func (g *Gate[S, A]) node() *node { return g.n }

// To declares an edge from this state.
func (g *Gate[S, A]) To(target State, label string) Edge[S] { return edgeTo[S](g.n, target, label) }

// Answers maps each answer to an edge.
func (g *Gate[S, A]) Answers(cases ...Case[S, A]) *Gate[S, A] {
	t := newTable[S, A](func(a A) A { return a }, g.k.answers, cases)
	g.n.cases, g.n.routed = t.view(), true
	return g
}

// Presents declares the artifacts shown to the decider.
func (g *Gate[S, A]) Presents(as ...AnyArtifact) *Gate[S, A] {
	for _, a := range as {
		g.k.presents = append(g.k.presents, a.artifact())
	}
	return g
}

// Timeout sets the edge taken after the duration; the duration is stored on
// the edge and rendered beside it.
func (g *Gate[S, A]) Timeout(after time.Duration, e Edge[S]) *Gate[S, A] {
	if e.e != nil {
		e.e.timeout = after
	}
	u := e.use()
	g.k.timeout = &u
	return g
}

// ---------------------------------------------------------------- wait

// Wait waits on an external condition read through an Observation.
//
// Owns: the observation, the rendered condition, the poll interval and the
// ready and timeout edges.
// Does not: run the predicate at lint or render time.
// Extend by: a new condition is a new Observation constructor.
type Wait[S any] struct {
	n *node
	k *waitKind
}

// NewWait declares a wait. cond is the rendered description; ready is never
// run by lint or render.
func NewWait[S, T any](d *Def, o Observation[T], cond string, ready func(T) bool) *Wait[S] {
	k := &waitKind{base: base{"wait"}, obs: o.desc, cond: cond, ready: ready}
	w := &Wait[S]{n: addMarker[S](d, k), k: k}
	k.ops = waitOps[S, T]{k: k, ready: ready}
	return w
}

func (w *Wait[S]) node() *node { return w.n }

// To declares an edge from this state.
func (w *Wait[S]) To(target State, label string) Edge[S] { return edgeTo[S](w.n, target, label) }

// Ready sets the edge taken when the condition holds.
func (w *Wait[S]) Ready(e Edge[S]) *Wait[S] {
	u := e.use()
	w.k.readyTo, w.n.routed = &u, true
	return w
}

// Poll sets the poll interval.
func (w *Wait[S]) Poll(every time.Duration) *Wait[S] {
	w.k.poll = every
	return w
}

// Timeout sets the edge taken after the duration.
func (w *Wait[S]) Timeout(after time.Duration, e Edge[S]) *Wait[S] {
	if e.e != nil {
		e.e.timeout = after
	}
	u := e.use()
	w.k.timeout = &u
	return w
}

// ---------------------------------------------------------------- child seam

// Child is a typed handle on a child definition: input I, result R.
//
// Owns: the declared input and result types of a child *Def, the only things
// that cross the parent/child seam.
// Does not: expose the child's states to the parent.
// Extend by: a nested workflow is a new constructor returning wf.Typed[In, Res](d).
type Child[I, R any] struct{ d *Def }

// Typed declares d's input and result types and returns the child handle.
func Typed[I, R any](d *Def) Child[I, R] {
	if d.input != nil || d.result != nil {
		d.misuse("Typed called on a definition that is already typed")
	}
	d.input, d.result = reflect.TypeFor[I](), reflect.TypeFor[R]()
	return Child[I, R]{d: d}
}

// Def returns the child definition.
func (c Child[I, R]) Def() *Def { return c.d }

// Yield declares the child result this terminal produces; lint checks R
// matches the definition's declared result (RuleSubResultType).
func Yield[S, R any](t *Terminal[S], fn func(c Ctx) R) {
	t.k.result, t.k.yield = reflect.TypeFor[R](), fn
}

// Composite is a state that includes a child definition (Sub or Map).
type Composite interface {
	State
	child() *Def
}

// ---------------------------------------------------------------- sub

// ExitCase maps one child terminal to a parent edge.
type ExitCase[S any] struct {
	terminal string
	e        edgeUse
}

// exitData is the non-generic record of an ExitCase held by the kind.
type exitData struct {
	terminal string
	e        edgeUse
}

// OnExit maps the child terminal whose marker type is T to a parent edge:
// wf.OnExit[roast.Converged](next); S is inferred.
func OnExit[T, S any](e Edge[S]) ExitCase[S] {
	return ExitCase[S]{terminal: stateName[T](), e: e.use()}
}

// Sub is a nested definition run as one parent state.
//
// Owns: the parent/child seam: the child *Def, the typed input I the child
// receives (it may carry Artifact refs and the run options) and the mapping
// from each child terminal to a parent Edge[S]. The child's typed result R is
// the only thing that crosses back.
// Does not: let parent edges target child states or the reverse, or re-decide
// child routes on resume (executor territory).
// Extend by: a nested workflow is a new *Def constructor; a new exit is an
// OnExit case.
type Sub[S, I, R any] struct {
	n *node
	k *subKind
}

// NewSub declares a Sub and records the child in d.children.
func NewSub[S, I, R any](d *Def, child Child[I, R], input func(c Ctx) I) *Sub[S, I, R] {
	k := &subKind{base: base{"sub"}, child: child.d, input: input}
	if child.d == nil {
		d.misuse("NewSub with a nil child definition")
	} else {
		d.children = append(d.children, child.d)
	}
	s := &Sub[S, I, R]{n: addMarker[S](d, k), k: k}
	k.ops = subOps[S, I, R]{s}
	return s
}

func (s *Sub[S, I, R]) node() *node { return s.n }
func (s *Sub[S, I, R]) child() *Def { return s.k.child }

// To declares an edge from this state.
func (s *Sub[S, I, R]) To(target State, label string) Edge[S] { return edgeTo[S](s.n, target, label) }

// Exits maps child terminals to parent edges.
func (s *Sub[S, I, R]) Exits(cases ...ExitCase[S]) *Sub[S, I, R] {
	for _, c := range cases {
		s.k.exits = append(s.k.exits, exitData{terminal: c.terminal, e: c.e})
	}
	s.n.routed = true
	return s
}

// Observes declares the Observations the input closure reads (argument
// templates, rendered and hashed like Compute.Observes). Reading an undeclared
// Observation in the input closure is a run-time Blocked outcome.
func (s *Sub[S, I, R]) Observes(os ...Reader) *Sub[S, I, R] {
	for _, o := range os {
		s.k.observes = append(s.k.observes, o.Desc())
	}
	return s
}

// Retry sets the Sub's per-OutcomeClass routes. A declared observation that is
// unavailable when the child would start is classed Unavailable and takes the
// policy's Unavailable route (OnClass or OnClassBackoff); a child that halts
// takes its Blocked route.
func (s *Sub[S, I, R]) Retry(p RetryPolicy[S]) { s.k.retry = p.d }

// ---------------------------------------------------------------- map

// SiblingPolicy is the closed set of sibling-failure policies.
type SiblingPolicy int

// Sibling-failure policies.
const (
	ContinueSiblings SiblingPolicy = iota + 1
	CancelSiblings
)

// sourceData is the erased item source of a Map. Exactly one of obs, from,
// comp or static is set; items turns the JSON of the earlier state's value into
// the item list (nil for obs and static).
type sourceData struct {
	projectionName string
	projection     func(json.RawMessage) (json.RawMessage, error)
	obs            *ObservationDesc
	from           *node
	comp           *node
	static         bool
	values         []json.RawMessage
	err            error // invalid source data or projection declaration
	items          func(raw json.RawMessage) ([]json.RawMessage, error)
}

// marshalItems encodes each item as JSON.
func marshalItems[I any](xs []I) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(xs))
	for _, x := range xs {
		b, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// eraseItems erases a typed item closure over an output O to JSON.
func eraseItems[O, I any](items func(out O) []I) func(json.RawMessage) ([]json.RawMessage, error) {
	return func(raw json.RawMessage) ([]json.RawMessage, error) {
		var o O
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &o); err != nil {
				return nil, err
			}
		}
		return marshalItems(items(o))
	}
}

// Source is the runtime item source of a Map.
type Source[I any] struct{ s sourceData }

// Project applies a named, pure projection to each source item before Map
// keys, write sets, and child inputs are derived. The source descriptor remains
// unchanged; the name is part of definition identity. Only one projection may
// be declared on a source.
func (source Source[I]) Project(name string, project func(I) I) Source[I] {
	if source.s.err != nil {
		return source
	}
	switch {
	case source.s.projection != nil || source.s.projectionName != "":
		source.s.err = fmt.Errorf("source projection may be declared only once")
	case strings.TrimSpace(name) == "":
		source.s.err = fmt.Errorf("source projection needs a nonempty name")
	case project == nil:
		source.s.err = fmt.Errorf("source projection %q has a nil function", name)
	default:
		source.s.projectionName = name
		source.s.projection = func(raw json.RawMessage) (json.RawMessage, error) {
			var item I
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			return json.Marshal(project(item))
		}
	}
	return source
}

func (source sourceData) projectItem(raw json.RawMessage) (json.RawMessage, error) {
	if source.err != nil {
		return nil, source.err
	}
	if source.projection == nil {
		return raw, nil
	}
	return source.projection(raw)
}

// FromObservation sources items from a read-only observation.
func FromObservation[I any](o Observation[[]I]) Source[I] {
	d := o.desc
	return Source[I]{s: sourceData{obs: &d}}
}

// FromOutput sources items from an earlier agent's output.
func FromOutput[S, O, I any](a *Agent[S, O], items func(out O) []I) Source[I] {
	return Source[I]{s: sourceData{from: a.n, items: eraseItems(items)}}
}

// FromCompute sources items from a Compute's computed value.
func FromCompute[S, O, I any](c *Compute[S, O], items func(out O) []I) Source[I] {
	return Source[I]{s: sourceData{comp: c.n, items: eraseItems(items)}}
}

// FromValues sources items from a declared static list: definition data,
// rendered as "values <json>" and hashed with the definition.
func FromValues[I any](items ...I) Source[I] {
	vals, err := marshalItems(items)
	return Source[I]{s: sourceData{static: true, values: vals, err: err}}
}

// JoinPolicy is a sealed, closed set of join policies: all_settled, all_ok,
// quorum(k), predicate. Each join outcome is an Edge[S] of the map state.
type JoinPolicy struct {
	kind string
	k    int
	name string
	pred any
}

// AllSettled joins when every instance has settled.
func AllSettled() JoinPolicy { return JoinPolicy{kind: "all_settled"} }

// AllOK joins when every instance succeeded.
func AllOK() JoinPolicy { return JoinPolicy{kind: "all_ok"} }

// Quorum joins when k instances succeeded.
func Quorum(k int) JoinPolicy { return JoinPolicy{kind: "quorum", k: k} }

// JoinWhen joins when the named predicate over the keyed result holds.
func JoinWhen[K comparable, R any](name string, pred func(MapResult[K, R]) bool) JoinPolicy {
	return JoinPolicy{kind: "predicate", name: name, pred: pred}
}

// MapResult is the keyed join result: each child instance's typed result R
// keyed by the stable item key, plus per-instance outcome. The parent reads
// child results and artifacts only through it. A budget-stopped instance
// counts as settled and not successful for every JoinPolicy.
//
// SourceError is set when the Map's source read failed (unavailable or
// errored) and the Join took its partial edge for that reason; Items then
// holds only the children that had already settled.
type MapResult[K comparable, R any] struct {
	Items       map[K]Instance[R]
	SourceError string `json:",omitempty"`
}

// Instance is one child instance's typed result, outcome class and terminal.
// BudgetStopped is set when the executor stopped the instance on the run
// budget (turns, tokens, wall clock) before it reached a terminal; Result and
// Terminal are then zero, and the stop is never classed Refused (journaled
// budget_stopped). BudgetStoppedBelow is set when the budget stopped an
// instance below this one, at any depth (an inner Map's item, a Sub's child):
// the instance may still have reached a terminal, on less work than it
// planned. Whether that terminal stands is the definition's choice.
type Instance[R any] struct {
	Result             R
	Class              OutcomeClass
	Terminal           string
	BudgetStopped      bool
	BudgetStoppedBelow bool `json:",omitempty"`
}

// Map is dynamic fan-out over runtime items with a join.
//
// Owns: the item source, the stable item key, the child template, parallelism,
// write sets, lease, the sibling-failure policy and the join.
// The graph shows one map node with a child template.
// Does not: mutate the graph or invent states per item. Dynamic work is a
// re-query of the source (Requery). Parallel is the definition's only
// concurrency limit and is workflow semantics (a cost or lock limit); how many
// ready units run at once is otherwise the coordinator's own take.
// Extend by: a new join policy is a sealed constructor in this package; a
// nested template is a new child *Def constructor.
type Map[S, I any, K comparable, R any] struct {
	n *node
	k *mapKind
}

// NewMap declares a map over a source with an item key and a child template.
func NewMap[S, I any, K comparable, R any](d *Def, src Source[I], key func(item I) K, child Child[I, R]) *Map[S, I, K, R] {
	var keyFn any // stays a nil interface for a nil key so lint can detect it
	if key != nil {
		keyFn = key
	}
	k := &mapKind{base: base{"map"}, src: src.s, key: keyFn, keyType: reflect.TypeFor[K]().String(), child: child.d}
	if child.d == nil {
		d.misuse("NewMap with a nil child definition")
	} else {
		d.children = append(d.children, child.d)
	}
	m := &Map[S, I, K, R]{n: addMarker[S](d, k), k: k}
	k.ops = mapOps[S, I, K, R]{m}
	return m
}

func (m *Map[S, I, K, R]) node() *node { return m.n }
func (m *Map[S, I, K, R]) child() *Def { return m.k.child }

// To declares an edge from this state.
func (m *Map[S, I, K, R]) To(target State, label string) Edge[S] {
	return edgeTo[S](m.n, target, label)
}

// OnClass declares an unreachable-by-design fallback for a failure class.
// The edge must belong to this Map and be marked Unreachable; ordinary live
// class routes are not supported. This overrides the definition default and
// remains executable so coverage can reject a false unreachability claim.
// Declare it before the definition is inspected, hashed, linted or run.
func (m *Map[S, I, K, R]) OnClass(class OutcomeClass, e Edge[S]) *Map[S, I, K, R] {
	var problem string
	switch {
	case m.n.def.sealed:
		problem = "after the definition was used"
	case class == Accepted:
		problem = "accepted is not a failure"
	case class < Rejected || class > Blocked:
		problem = "unknown failure class"
	case e.e == nil:
		problem = "zero Edge"
	case e.e.from != m.n:
		problem = "edge belongs to another state"
	case e.e.to == nil || e.e.to.def != m.n.def:
		problem = "edge target must belong to the same definition"
	case !e.e.unreachable:
		problem = "edge must be marked Unreachable"
	}
	if problem != "" {
		m.n.def.misuse(fmt.Sprintf("%s.OnClass(%s): %s", m.n.name, class, problem))
		return m
	}
	if m.k.byClass == nil {
		m.k.byClass = map[OutcomeClass]edgeUse{}
	}
	m.k.byClass[class] = e.use()
	e.e.class = class
	return m
}

// Parallel sets the maximum parallel instances.
func (m *Map[S, I, K, R]) Parallel(n int) *Map[S, I, K, R] { m.k.parallel = n; return m }

// Writes declares the static write set (globs).
func (m *Map[S, I, K, R]) Writes(globs ...string) *Map[S, I, K, R] {
	m.k.writes = append(m.k.writes, globs...)
	return m
}

// ItemWrites declares the per-item write set.
func (m *Map[S, I, K, R]) ItemWrites(fn func(item I) []string) *Map[S, I, K, R] {
	m.k.itemWrites = fn
	return m
}

// Lease sets the instance lease time-to-live.
func (m *Map[S, I, K, R]) Lease(ttl time.Duration) *Map[S, I, K, R] { m.k.lease = ttl; return m }

// OnSiblingFailure sets the sibling-failure policy.
func (m *Map[S, I, K, R]) OnSiblingFailure(p SiblingPolicy) *Map[S, I, K, R] {
	m.k.sibling = p
	return m
}

// Requery re-queries the source as children settle and at every hand-out;
// dynamic work is a re-query, never new states.
//
// A child that has not started (no unit yet, pending or handed out, and no
// earlier unit run) whose key left the source is withdrawn: a withdrawn
// record, then the child settles Cancelled, so the join counts it neither
// way. A started child is left to finish, and so is one that ran an earlier
// unit: a task whose review waits for hand-out is under way. An item type that has a method WaitingOnDeps() bool reporting true
// is not spawned yet: it stays outstanding, so the join waits for it, and it
// is counted as waiting on dependencies.
func (m *Map[S, I, K, R]) Requery() *Map[S, I, K, R] { m.k.requery = true; return m }

// Join sets the join policy, the edge taken when it is met and the
// partial-failure edge.
func (m *Map[S, I, K, R]) Join(p JoinPolicy, met, partial Edge[S]) *Map[S, I, K, R] {
	mu, pu := met.use(), partial.use()
	m.k.join, m.k.met, m.k.partial = &p, &mu, &pu
	m.n.routed = true
	return m
}

// ---------------------------------------------------------------- terminal

// Terminal is an end state.
//
// Owns: the Outcome and, in a child definition, the typed result it yields.
// Does not: have out-edges.
// Extend by: a definition-specific stop reason is a new marker type.
type Terminal[S any] struct {
	n *node
	k *terminalKind
}

// NewTerminal declares a terminal state.
func NewTerminal[S any](d *Def, o Outcome) *Terminal[S] {
	k := &terminalKind{base: base{"terminal"}, outcome: o}
	t := &Terminal[S]{n: addMarker[S](d, k), k: k}
	k.ops = terminalOps[S]{t}
	t.n.routed = true
	return t
}

func (t *Terminal[S]) node() *node { return t.n }

// ---------------------------------------------------------------- walk bodies
//
// Every kind contributes its own part of the shared walk (Inspect) through
// view; Render, Mermaid, Hash and the kind-agnostic lint rules read only the
// result. Kind-specific lint rules live next to it.

func (v *StateView) retry(n *node, r *retryData) {
	if r == nil {
		return
	}
	rv := &RetryView{
		Max:             r.max.String(),
		MaxConst:        constPart(r.max),
		NoProgressAfter: r.noProgress,
		Exhausted:       useLabel(r.exhausted),
		Stalled:         useLabel(r.exhausted),
		Fingerprint:     r.fingerprint,
		Recovery:        offersRecovery(n),
	}
	v.budgets = append(v.budgets, r.max)
	v.useAs(r.exhausted, failedUse)
	v.fallback(r.exhausted.e, "exhausted")
	if r.stalled != nil {
		rv.Stalled = useLabel(*r.stalled)
		v.useAs(*r.stalled, failedUse)
		v.fallback(r.stalled.e, "stalled")
	}
	classes := make([]OutcomeClass, 0, len(r.byClass))
	for c := range r.byClass {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	for _, c := range classes {
		if rv.ByClass == nil {
			rv.ByClass = map[string]string{}
		}
		u := r.byClass[c]
		rv.ByClass[c.String()] = useLabel(u)
		if b := r.backoff[c]; len(b) > 0 {
			if rv.Backoff == nil {
				rv.Backoff = map[string][]time.Duration{}
			}
			rv.Backoff[c.String()] = b
		}
		v.useAs(u, failedUse)
	}
	v.Retry = rv
}

func (k *agentKind) view(n *node, v *StateView) {
	v.Output = k.outType
	v.Dispatch = hintsOf(k.dispatch).String()
	switch {
	case k.prompt.item != nil:
		v.Prompt = "one of " + strings.Join(k.prompt.candidates, " | ") + " (by item)"
		v.PromptCandidates = append([]string{}, k.prompt.candidates...)
	case k.prompt.path != "":
		v.Prompt = k.prompt.path
	default:
		v.Prompt = k.prompt.text
	}
	for _, in := range k.prompt.inputs {
		d := in.inputRef()
		v.inputs = append(v.inputs, d)
		v.Inputs = append(v.Inputs, inputString(d))
	}
	v.Checks = append(v.Checks, k.checkNames...)
	v.FetchCoverage = k.coverage
	for _, o := range k.observes {
		v.Observes = append(v.Observes, obsString(o))
	}
	for _, o := range k.inputs {
		v.Observes = append(v.Observes, "input "+obsString(o))
	}
	v.retry(n, k.retry)
}

func (k *commandKind) view(n *node, v *StateView) {
	v.Argv = k.argv
	v.Dir = k.dir
	v.CommandKey = k.key
	v.Idempotent = k.idempotent
	v.Detached = k.detached
	v.Stdin = k.stdin
	v.retry(n, k.retry)
}

func (k *computeKind) view(n *node, v *StateView) {
	v.Output = k.outType
	for _, o := range k.observes {
		v.Observes = append(v.Observes, obsString(o))
	}
	v.retry(n, k.retry)
}

func (k *gateKind) view(n *node, v *StateView) {
	v.IntValues = k.intValueViews()
	v.Question = k.question
	v.EquivalentAnswers = k.equivalentAnswers
	switch k.reservation {
	case Reserved:
		v.Reservation = "reserved"
	case PolicyAnswerable:
		v.Reservation = "policy-answerable"
	}
	for _, a := range k.presents {
		v.Presents = append(v.Presents, a.name)
		v.presents = append(v.presents, a)
	}
	if k.timeout != nil {
		v.useAs(*k.timeout, failedUse)
	}
}

func (k *waitKind) view(n *node, v *StateView) {
	v.Cond, v.Poll = k.cond, k.poll
	v.Observes = append(v.Observes, obsString(k.obs))
	if k.readyTo != nil {
		v.use(*k.readyTo)
	}
	if k.timeout != nil {
		v.useAs(*k.timeout, failedUse)
	}
}

func (k *subKind) childDef() *Def { return k.child }

func (k *subKind) view(n *node, v *StateView) {
	if k.child != nil {
		v.Child, v.child = k.child.name, k.child
	}
	for _, o := range k.observes {
		v.Observes = append(v.Observes, obsString(o))
	}
	v.retry(n, k.retry)
	for _, x := range k.exits {
		if v.Exits == nil {
			v.Exits = map[string]string{}
		}
		v.Exits[x.terminal] = useLabel(x.e)
		v.use(x.e)
	}
}

func (k *mapKind) childDef() *Def { return k.child }

func (k *mapKind) view(n *node, v *StateView) {
	classes := make([]OutcomeClass, 0, len(k.byClass))
	for class := range k.byClass {
		classes = append(classes, class)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	for _, class := range classes {
		v.useAs(k.byClass[class], failedUse)
	}
	if k.child != nil {
		v.Child, v.child = k.child.name, k.child
	}
	switch {
	case k.src.obs != nil:
		v.Source = obsString(*k.src.obs)
		v.Observes = append(v.Observes, v.Source)
	case k.src.from != nil:
		v.Source = "output of " + k.src.from.name
	case k.src.comp != nil:
		v.Source = "computed value of " + k.src.comp.name
	case k.src.static:
		v.Source = "values [" + joinRaw(k.src.values) + "]"
	}
	v.Projection = k.src.projectionName
	if v.Projection != "" {
		v.Source += " projected by " + v.Projection
	}
	v.Key, v.Parallel = k.keyType, k.parallel
	v.Writes = append(v.Writes, k.writes...)
	v.ItemWrites = k.itemWrites != nil
	v.Lease, v.Requery = k.lease, k.requery
	switch k.sibling {
	case ContinueSiblings:
		v.Sibling = "continue"
	case CancelSiblings:
		v.Sibling = "cancel"
	}
	if k.join != nil {
		v.Join = k.join.String()
	}
	if k.met != nil {
		v.use(*k.met)
	}
	if k.partial != nil {
		v.use(*k.partial)
	}
}

func joinRaw(vs []json.RawMessage) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = string(v)
	}
	return strings.Join(parts, ",")
}

// String renders the join policy: all_ok, quorum(2), predicate <name>.
func (j JoinPolicy) String() string {
	switch j.kind {
	case "quorum":
		return fmt.Sprintf("quorum(%d)", j.k)
	case "predicate":
		return "predicate " + j.name
	}
	return j.kind
}

func (k *terminalKind) view(n *node, v *StateView) {
	v.Outcome = string(k.outcome)
	v.PartialWhen = k.partial != nil
	v.Intent = k.intent
	if k.result != nil {
		v.Result = k.result.String()
	}
}

// ---------------------------------------------------------------- kind lint

func (n *node) problem(r RuleID, format string, args ...any) Problem {
	return Problem{Rule: r, Path: n.def.name + "/" + n.name, Msg: fmt.Sprintf(format, args...)}
}

// lintCases checks a declared case table: values mapped twice, coverage over
// its Enum, unknown values, and the Otherwise an open table needs.
func lintCases(n *node) []Problem {
	t := n.cases
	if t == nil {
		return nil
	}
	var out []Problem
	have := map[string]bool{}
	reported := map[string]bool{}
	for _, c := range t.cases {
		if have[c.value] && !reported[c.value] {
			reported[c.value] = true
			out = append(out, n.problem(RuleCaseDuplicate, "case value %q is mapped more than once", c.value))
		}
		have[c.value] = true
	}
	if t.enum == nil {
		if t.otherwise == nil {
			out = append(out, n.problem(RuleCaseNoOtherwise, "case table over an open key has no Otherwise"))
		}
		return out
	}
	known := map[string]bool{}
	var missing []string
	for _, v := range t.enum.values {
		known[v] = true
		if !have[v] && t.otherwise == nil {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		out = append(out, n.problem(RuleCaseUncovered, "case table over %s does not cover %s", t.enum.name, strings.Join(missing, ", ")))
	}
	for _, c := range t.cases {
		if !known[c.value] {
			out = append(out, n.problem(RuleCaseUnknownValue, "case value %q is not in Enum %s", c.value, t.enum.name))
		}
	}
	return out
}

func (k *agentKind) lint(n *node) []Problem {
	var out []Problem
	if k.retry == nil {
		out = append(out, n.problem(RuleAgentRetry, "agent has no retry policy (call Spec with a Retry)"))
	}
	out = append(out, lintPromptFor(n, k.prompt)...)
	out = append(out, lintDispatch(n, k.dispatch)...)
	return append(out, lintCases(n)...)
}

func (k *commandKind) lint(n *node) []Problem { return lintCases(n) }

func (k *computeKind) lint(n *node) []Problem {
	var out []Problem
	if len(k.observes) > 0 && k.retry == nil {
		out = append(out, n.problem(RuleComputeUnavailable,
			"compute observes %s but declares no unavailable route (Retry, OnClass or OnClassBackoff)",
			obsString(k.observes[0])))
	}
	return append(out, lintCases(n)...)
}

func (k *gateKind) lint(n *node) []Problem {
	out := k.lintIntValues(n)
	if n.cases == nil {
		out = append(out, n.problem(RuleGateAnswers, "gate has no Answers: every answer must map to an edge"))
	}
	if k.answers == nil {
		return out
	}
	for _, r := range n.def.policy.rules {
		if r.gate != n {
			continue
		}
		if !contains(k.answers.values, r.answer) {
			out = append(out, n.problem(RuleGateAnswers, "policy answer %q is not in the answer set %s", r.answer, k.answers.name))
		}
	}
	return append(out, lintCases(n)...)
}

func (k *waitKind) lint(n *node) []Problem { return nil }

func (k *subKind) lint(n *node) []Problem {
	var out []Problem
	if len(k.observes) > 0 && k.retry == nil {
		out = append(out, n.problem(RuleComputeUnavailable,
			"sub observes %s but declares no unavailable route (Retry, OnClass or OnClassBackoff)",
			obsString(k.observes[0])))
	}
	if k.child == nil {
		return out
	}
	mapped := map[string]bool{}
	for _, x := range k.exits {
		mapped[x.terminal] = true
	}
	own := map[string]bool{}
	for _, c := range k.child.nodes {
		if !c.kind.terminal() {
			continue
		}
		own[c.name] = true
		if !mapped[c.name] {
			out = append(out, n.problem(RuleSubExitUnmapped, "child %s terminal %s has no parent edge", k.child.name, c.name))
		}
	}
	for _, x := range k.exits {
		if !own[x.terminal] {
			out = append(out, n.problem(RuleSubExitUnmapped, "exit maps %s, which is not a terminal of child %s", x.terminal, k.child.name))
		}
	}
	if k.child.input == nil || k.child.result == nil {
		out = append(out, n.problem(RuleSubResultType, "child %s does not declare its input and result types (wf.Typed)", k.child.name))
	}
	return out
}

func (k *mapKind) lint(n *node) []Problem {
	var out []Problem
	if k.src.err != nil {
		out = append(out, n.problem(RuleBuild, "map source: %v", k.src.err))
	}
	if k.key == nil {
		out = append(out, n.problem(RuleMapItemKey, "map has no item key function"))
	}
	if k.join == nil {
		out = append(out, n.problem(RuleMapJoin, "map has no join policy (call Join)"))
	} else if k.partial == nil || k.partial.e == nil || (k.met != nil && k.partial.e == k.met.e) {
		out = append(out, n.problem(RuleMapPartialFailure, "map needs a partial-failure edge distinct from the join-met edge"))
	}
	if k.parallel != 1 && len(k.writes) > 0 && k.itemWrites == nil {
		out = append(out, n.problem(RuleMapWriteOverlap, "parallel instances share the static write set %v and declare no per-item write set", k.writes))
	}
	return out
}

func (k *terminalKind) lint(n *node) []Problem {
	d := n.def
	switch {
	case k.result == nil && d.result != nil:
		return []Problem{n.problem(RuleSubResultType, "terminal does not Yield %s, the result type definition %s declares (wf.Typed)", d.result, d.name)}
	case k.result == nil:
	case d.result == nil:
		return []Problem{n.problem(RuleSubResultType, "terminal yields %s but definition %s is untyped (not a child: wf.Typed)", k.result, d.name)}
	case k.result != d.result:
		return []Problem{n.problem(RuleSubResultType, "terminal yields %s but definition %s declares result %s", k.result, d.name, d.result)}
	}
	return nil
}
