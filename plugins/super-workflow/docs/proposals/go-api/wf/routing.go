package wf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
)

type edgeData struct {
	from, to          *node
	label             string
	effects           []Effect
	when              *Pred
	timeout           time.Duration
	class             OutcomeClass
	visits            *Budget
	optional          bool // a When route-around's Optional, kept with its effects
	endsOutcome       Outcome
	ends              string // Edge.Ends: the reason this end is intentional
	unreachable       bool
	unreachableReason string
}

// edgeUse is one use of an edge: the edge plus the effects attached to this
// particular traversal (the same edge may be used with different effects).
type edgeUse struct {
	e        *edgeData
	effects  []Effect
	optional bool   // Edge.Optional: a failed effect does not stop the transition
	routedBy string // Edge.OptionalRoutedBy: what routes the failure instead
}

// Edge is a transition declared FROM one specific state. It can only be created
// by that state's To method, so every edge exists in the graph before any code
// runs: the renderer and the linter never execute user functions to find targets.
//
// S is the marker type of the source state, so a route, retry policy, case
// table, join or timeout of state A can only take Edge[A]; handing it another
// state's edge is a compile error.
//
// Owns: one declared transition, with its label, effects, option guard,
// timeout duration and outcome-class role. Covers timeout edges and
// outcome-class fallback edges, and is the only target type for Cases, join
// and Sub-exit mappings.
// Does not: choose when it is taken (routes, Cases and policies do).
// Extend by: a new way of selecting an edge takes Edge[S] as its target type.
type Edge[S any] struct {
	e        *edgeData
	effects  []Effect
	optional bool
	routedBy string // OptionalRoutedBy: what routes a failed effect
}

// Unreachable exempts this declared edge from suite reachability with a reason.
// LintReachability rejects blank reasons; coverage rejects an exemption that
// a seed actually traverses.
func (e Edge[S]) Unreachable(reason string) Edge[S] {
	if e.e != nil {
		e.e.unreachable = true
		e.e.unreachableReason = reason
	}
	return e
}

func (e Edge[S]) use() edgeUse {
	return edgeUse{e: e.e, effects: e.effects, optional: e.optional, routedBy: e.routedBy}
}

// With attaches effects (bead creation, counter increments, ...) to this
// traversal. Effects run when the transition is taken and are journaled intent
// then result. Into a state that Holds locks, they run under those locks; see
// Hold. Effects apply in declaration order, except that a scope counter's Inc
// (and the zeroing it causes) applies before any scoped Inc on the same edge.
func (e Edge[S]) With(effects ...Effect) Edge[S] {
	all := append([]Effect(nil), e.effects...)
	for _, effect := range effects {
		if external, ok := effect.(ExternalEffect); ok && e.e != nil && e.e.from.def.sealed {
			effect = runtimeEffect{external}
		}
		all = append(all, effect)
	}
	return Edge[S]{e: e.e, effects: all, optional: e.optional, routedBy: e.routedBy}
}

// Optional lets this traversal proceed past a failed or unavailable external
// effect: the transition is taken and the target reads EdgeOutcome. Without
// it a failed effect stops the transition, and the source state takes its
// failure route for the effect's class (Blocked for failed, Unavailable for
// unavailable): its own OnClass route, else the definition's OnFailure
// default, else a halt.
func (e Edge[S]) Optional() Edge[S] {
	e.effects = append([]Effect{}, e.effects...)
	e.optional = true
	return e
}

// OptionalRoutedBy is Optional for an edge whose target routes a failed
// effect itself: what says how (a containment check that sends an unmerged
// task to rebase). The degraded record a failed effect writes names it as
// what happened, not as an Optional edge going on.
func (e Edge[S]) OptionalRoutedBy(what string) Edge[S] {
	e = e.Optional()
	e.routedBy = what
	return e
}

// When guards the edge with an option predicate; render shows the predicate.
// The Engine evaluates it: a Route or Cases table that returns a guarded edge
// whose predicate does not hold is Blocked, and a guarded edge no Route or
// Cases table of its state selects is a route-around taken before the state
// runs when its predicate holds (with the effects attached before When).
func (e Edge[S]) When(p Pred) Edge[S] {
	if e.e != nil {
		e.e.when = &p
		e.e.effects, e.e.optional = e.effects, e.optional
	}
	return e
}

func (n *node) to(target State, label string) *edgeData {
	if target == nil {
		n.def.misuse(fmt.Sprintf("state %q: To(nil, %q)", n.name, label))
		return nil
	}
	if n.def.sealed {
		n.def.misuse(fmt.Sprintf("state %q: To(%q) after the definition was used: OnFailure defaults are already applied", n.name, label))
	}
	ed := &edgeData{from: n, to: target.node(), label: label}
	n.edges = append(n.edges, ed)
	return ed
}

type enumData struct {
	name   string
	values []string
	vals   any
}

// Enum is a registered closed value set (verdicts, gate answers, script exit
// codes).
//
// Owns: the closed set on the Def, so a Cases table over it is checked for
// coverage and render shows every value.
// Does not: belong to a package-global registry; Enums belong to their Def.
// Extend by: a new set is one NewEnum registration plus Cases tables.
type Enum[V comparable] struct{ e *enumData }

// NewEnum registers the closed set on d; values render with fmt.Sprint.
func NewEnum[V comparable](d *Def, name string, values ...V) Enum[V] {
	ed := &enumData{name: name, vals: append([]V{}, values...)}
	for _, v := range values {
		ed.values = append(ed.values, fmt.Sprint(v))
	}
	d.enums = append(d.enums, ed)
	return Enum[V]{e: ed}
}

// Name returns the Enum name.
func (e Enum[V]) Name() string { return e.e.name }

// Values returns the registered values.
func (e Enum[V]) Values() []V {
	vs, _ := e.e.vals.([]V)
	return append([]V{}, vs...)
}

// Case maps one value of V to an edge (or to a free-form route) of state S.
type Case[S any, V comparable] struct {
	value V
	e     edgeUse
	route func(Ctx) Edge[S]
	cands []edgeUse
}

// On maps a value to an edge of the owning state.
func On[S any, V comparable](v V, e Edge[S]) Case[S, V] {
	return Case[S, V]{value: v, e: e.use()}
}

// OnRoute hands a value off to a free-form route over context. The candidates
// must list every edge fn may return: the walk never runs fn, so render, hash
// and lint see the candidates and their effects, and the executor refuses a
// returned edge outside them (see Executor, Declared obligations).
func OnRoute[S any, V comparable](v V, fn func(c Ctx) Edge[S], candidates ...Edge[S]) Case[S, V] {
	cs := make([]edgeUse, len(candidates))
	for i, e := range candidates {
		cs[i] = e.use()
	}
	return Case[S, V]{value: v, route: fn, cands: cs}
}

// routeData is the declared candidate set of a state's free-form Route.
type routeData struct{ cands []edgeUse }

// candidateUses converts a Route's candidate edges to uses; a zero Edge is a
// build misuse on d.
func candidateUses[S any](d *Def, where string, es []Edge[S]) []edgeUse {
	out := make([]edgeUse, len(es))
	for i, e := range es {
		if e.e == nil {
			d.misuse(fmt.Sprintf("state %q: route candidate %d is a zero Edge", where, i))
		}
		out[i] = e.use()
	}
	return out
}

type caseData struct {
	value    string
	e        edgeUse
	hasRoute bool
	cands    []edgeUse
	route    func(Ctx) edgeUse // the erased OnRoute closure; only the Engine runs it
}

// CaseTable is a declared case table mapping each value of a field of X to an
// Edge[S] of the owning state.
//
// Owns: exhaustive routing. A table over a registered Enum is checked for
// coverage; a table over an open key requires an explicit Otherwise. The table
// is data, so lint checks coverage and render shows every case.
// Does not: evaluate conditions over counters or context; free-form Route
// closures still do that, and a case may hand off to one (OnRoute).
// Extend by: new closed value sets are one Enum registration plus a Cases call.
type CaseTable[S, X any] struct {
	field     func(X) any
	enum      *enumData
	cases     []caseData
	otherwise *edgeUse
}

// caseView is the non-generic view of a case table that lint and render read.
type caseView struct {
	enum      *enumData
	cases     []caseData
	otherwise *edgeUse
	field     func(any) any // the erased key selector; only the Engine runs it
}

func (t CaseTable[S, X]) view() *caseView {
	return &caseView{enum: t.enum, cases: t.cases, otherwise: t.otherwise, field: func(x any) any {
		v, _ := x.(X)
		return t.field(v)
	}}
}

func newTable[S, X any, V comparable](field func(x X) V, enum *enumData, cases []Case[S, V]) CaseTable[S, X] {
	t := CaseTable[S, X]{enum: enum, field: func(x X) any { return field(x) }}
	for _, c := range cases {
		cd := caseData{value: fmt.Sprint(c.value), e: c.e, hasRoute: c.route != nil, cands: c.cands}
		if r := c.route; r != nil {
			cd.route = func(ctx Ctx) edgeUse { return r(ctx).use() }
		}
		t.cases = append(t.cases, cd)
	}
	return t
}

// Cases builds a case table exhaustive over a registered Enum (lint checks
// coverage).
func Cases[S, X any, V comparable](field func(x X) V, key Enum[V], cases ...Case[S, V]) CaseTable[S, X] {
	return newTable[S, X](field, key.e, cases)
}

// CasesOpen builds a case table whose key is not a registered Enum; lint
// requires Otherwise.
func CasesOpen[S, X any, V comparable](field func(x X) V, cases ...Case[S, V]) CaseTable[S, X] {
	return newTable[S, X](field, nil, cases)
}

// Otherwise sets the edge taken when no case matches.
func (t CaseTable[S, X]) Otherwise(e Edge[S]) CaseTable[S, X] {
	u := e.use()
	t.otherwise = &u
	return t
}

// Ctx is what routes, checks, computes and wait predicates may read: run
// parameters, counters, attempt info, and the typed reads below.
//
// Owns: the read surface of a running definition; everything in it comes from
// the journal or from fresh adapter observations.
// Does not: allow writes, or reads that bypass the declared seams (child
// results and child artifacts are read only through the child's typed result).
// Extend by: a new read is a package function calling the sealed accessor.
type Ctx interface {
	Param(p Param) string
	Count(c Counter) int
	Eval(b Budget) int
	Attempt() int
	value(k ctxKey) (any, error)
}

type ctxKey struct {
	kind string
	ref  any
}

// decodeValue turns what Ctx.value served into a T. The Engine serves raw
// journaled JSON, which is decoded; a typed value (a test Ctx) is asserted.
func decodeValue[T any](v any) T {
	var t T
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) > 0 {
			// A workflow type's UnmarshalJSON gets its own copy: one that
			// writes into the bytes it is given must not change the
			// journal data the cycle's other reads share.
			_ = json.Unmarshal(bytes.Clone(raw), &t)
		}
		return t
	}
	t, _ = v.(T)
	return t
}

func read[T any](c Ctx, kind string, ref any) T {
	v, _ := c.value(ctxKey{kind: kind, ref: ref})
	return decodeValue[T](v)
}

// OutputOf reads the latest accepted output of agent state a in this instance
// (the top-level run, one Sub entry or one Map item). It is not scoped to the
// current visit or round: a visit whose output was rejected, or a round that
// did not visit a, leaves the previous accepted value. A route or compute that
// must act only on an output accepted this round guards on a scoped counter
// incremented on the edge that accepts it.
func OutputOf[S, O any](c Ctx, a *Agent[S, O]) O { return read[O](c, "output", a.n) }

// ComputedOf reads the latest computed output of compute state s in this
// instance. Like OutputOf it is not scoped to the current visit or round: a
// visit that failed, or a round that did not visit s, leaves the previous
// value, so a reader that needs this round's value guards on a scoped counter
// incremented on the edge out of s.
func ComputedOf[S, O any](c Ctx, s *Compute[S, O]) O { return read[O](c, "computed", s.n) }

// InputOf reads the enclosing child definition's typed input (Sub input or Map item).
func InputOf[I any](c Ctx) I { return read[I](c, "input", nil) }

// ChildResult reads a Sub's typed child result.
func ChildResult[S, I, R any](c Ctx, s *Sub[S, I, R]) R { return read[R](c, "child", s.n) }

// ChildOutcome reads the outcome the Sub's child ended with: Partial or
// Success as its terminal's PartialWhen judged it, Stopped or Stalled, and ""
// before it settled or when it ended at a Success terminal that made no
// readiness judgment (a plain one, or a settle recorded before PartialWhen):
// a parent sees a child that delivered Partial without the child's result
// type carrying it, and never mistakes an unjudged success for ready.
func ChildOutcome[S, I, R any](c Ctx, s *Sub[S, I, R]) Outcome {
	return read[Outcome](c, "childOutcome", s.n)
}

// Joined reads a Map's keyed join result.
func Joined[S, I any, K comparable, R any](c Ctx, m *Map[S, I, K, R]) MapResult[K, R] {
	return read[MapResult[K, R]](c, "joined", m.n)
}

type inputDesc struct {
	reference string
	kind      string
	name      string
	node      *node
	art       *artifactData
	alias     string // As: the name a prompt template binds this input under
	tmpl      string // Bound: the template whose scope-bound value is the input
	shell     string // ShellBound "word", ShellWords "words": how the bound value is quoted
	// ObservedAs: the observation read at issue and its render of the value.
	obs    *ObservationDesc
	render func(json.RawMessage) (string, error)
}

// Input is a sealed typed input reference for prompts: a Param, an
// Artifact[T], Out(state) or ChildInput().
//
// Owns: what a prompt template may bind; lint checks each reference resolves to
// something produced upstream (RuleInputUnresolved).
// Does not: bind values; binding at run time is executor territory.
// Extend by: a new reference shape is a new constructor returning Input.
type Input interface{ inputRef() inputDesc }

type asInput struct {
	in    Input
	alias string
}

func (a asInput) inputRef() inputDesc {
	d := a.in.inputRef()
	d.alias = a.alias
	return d
}

// As binds an input under another name in a prompt template, e.g. a 6.8
// template's {{FINDINGS_JSON}} to Out(state). Rendered as "<input> as NAME".
func As(name string, in Input) Input { return asInput{in: in, alias: name} }

type outInput struct{ n *node }

func (o outInput) inputRef() inputDesc { return inputDesc{kind: "out", name: o.n.name, node: o.n} }

// Out refers to the accepted output of an upstream Agent, Compute or Command state.
func Out(s State) Input { return outInput{n: s.node()} }

type boundInput struct{ name, tmpl, shell string }

func (b boundInput) inputRef() inputDesc {
	return inputDesc{kind: "bound", name: b.name, tmpl: b.tmpl, shell: b.shell}
}

// Bound is a declared input whose value is template bound by scope ({name}
// placeholders resolved as in any argv). It binds a prompt's {{name}} and its
// literal [name] token, so a prompt written with bracket parameters arrives
// filled, and shows in the view and the instruction's inputs; the Inputs
// block lists it only when no token consumed it. An unresolved placeholder
// blocks the state.
func Bound(name, template string) Input { return boundInput{name: name, tmpl: template} }

// ShellBound is a Bound input whose value is the bound text as exactly one
// POSIX shell word (sink.Quote, and quoted where it would start a Markdown
// block), for a prompt that places it in a command
// the agent runs ("cd [WORKTREE]"). The quoted word is the value everywhere
// the input shows: the prompt, the Inputs block and the instruction's
// inputs; paged delivery presents it as bound. Text that no prompt line can
// carry as one word blocks the state: a control character (a line break, a
// tab, NUL), a backtick, which would end an inline code span around the
// token, or invalid UTF-8. Place the token in the command as a bare word:
// inside "..." or another quoted string it is no longer one argument. The
// quoted word does not expand ~, and a word starting with - is still read as
// an option by the command, so write "cd -- [WORKTREE]" where that matters.
// In a fork, the bound text is checked like a command argument: one naming
// a root of the source run is refused. A manufacture script's
// {{input.NAME}} is the quoted word too, so a value a script passes as an
// argv element or a dir stays a wf.Bound. Reference has no effect on it: a
// shell word is always presented as bound, never paged, so keep a shell
// value small (a list of thousands of paths belongs in a file).
func ShellBound(name, template string) Input {
	return boundInput{name: name, tmpl: template, shell: shellWord}
}

// ShellWords is a ShellBound input for a list: its bound text is split at
// spaces, tabs and line breaks, and each element becomes one quoted shell
// word, so a list of pathspecs an agent wrote becomes that
// many arguments. An element cannot hold a space, and text with no elements
// binds empty. The command still reads the words: put "--" before them
// ("git add -- [FILES]") so none is taken as an option.
func ShellWords(name, template string) Input {
	return boundInput{name: name, tmpl: template, shell: shellWordList}
}

const (
	shellWord     = "word"
	shellWordList = "words"
)

// quoteBound is a Bound input's bound text as its shell mode quotes it.
func quoteBound(mode, v string) (string, error) {
	if mode == "" {
		return v, nil
	}
	words := []string{v}
	if mode == shellWordList {
		words = strings.FieldsFunc(v, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r'
		})
	}
	for _, w := range words {
		if err := shellWordText(w); err != nil {
			return "", err
		}
	}
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = promptWord(w)
	}
	return strings.Join(out, " "), nil
}

// promptWord is w as one shell word that cannot start a Markdown block where
// the template puts it at the start of a line: sink.Quote, and single quotes
// too around a word Quote leaves bare that starts with -, + or _, or with
// digits and a period ("-", "--", "___", "1."), which would read as a list
// item, a setext underline or a thematic break, or holds a # (zsh's extendedglob, common in a user's shell,
// reads a#b as a pattern). The shell reads the same argument.
func promptWord(w string) string {
	q := sink.Quote(w)
	if q != w {
		return q
	}
	digits := strings.TrimLeft(w, "0123456789")
	if w[0] == '-' || w[0] == '+' || w[0] == '_' || len(digits) < len(w) && strings.HasPrefix(digits, ".") || strings.Contains(w, "#") {
		return "'" + w + "'"
	}
	return w
}

// shellWordText refuses text a prompt line cannot carry as one shell word
// the agent copies.
func shellWordText(w string) error {
	if err := sink.CheckArg(w); err != nil {
		return err
	}
	if !utf8.ValidString(w) {
		// A root read from the filesystem can be; JSON would deliver
		// another path.
		return fmt.Errorf("shell word %q is not valid UTF-8", w)
	}
	if i := strings.IndexFunc(w, func(r rune) bool { return unicode.IsControl(r) || r == '`' }); i >= 0 {
		r, _ := utf8.DecodeRuneInString(w[i:])
		return fmt.Errorf("shell word %q holds %q, which no prompt line can carry as one word", w, r)
	}
	return nil
}

type observedInput struct {
	name   string
	desc   ObservationDesc
	render func(json.RawMessage) (string, error)
}

func (o observedInput) inputRef() inputDesc {
	d := o.desc
	return inputDesc{kind: "observed", name: o.name, obs: &d, render: o.render}
}

// ObservedAs is a prompt input whose value is an observation of the world,
// rendered by the definition: r's arguments are bound in the agent's scope,
// read when the instruction is issued and journaled (Purpose "input:NAME")
// before it, and render turns the decoded value into the text {{NAME}}
// binds. A refetch, a replay and a fork made after the issue render the
// journaled read, never the world as it is now; a fork made before the issue
// reads in its own world. A run adopted onto the definition with an attempt
// already open reads it once, at the first render, journaled against that
// issue. An unavailable read backs off with its reason; a read that does not
// decode as T, or that render refuses, blocks the state. The observation is
// one of the agent's input observations (Agent.Observes): a change under a
// live attempt makes it stale. It shows in the view and the hash.
func ObservedAs[T any](name string, r Observation[T], render func(T) (string, error)) Input {
	return observedInput{name: name, desc: r.Desc(), render: func(raw json.RawMessage) (string, error) {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("input %s: the read does not decode as %T: %w", name, v, err)
		}
		s, err := render(v)
		if err != nil {
			return "", fmt.Errorf("input %s: %w", name, err)
		}
		return s, nil
	}}
}

type childInput struct{}

func (childInput) inputRef() inputDesc { return inputDesc{kind: "child-input", name: "item"} }

// ChildInput refers to the Map item or Sub input of the enclosing child definition.
func ChildInput() Input { return childInput{} }

// Prompt is an agent prompt template plus its typed input-reference list,
// rendered beside the state.
type Prompt struct {
	path, text string
	inputs     []Input
	candidates []string              // PromptFor: declared references, as written
	item       reflect.Type          // PromptFor: the item type I; nil otherwise
	sel        func(item any) string // PromptFor: the erased selector; only the Engine runs it
}

// PromptFor selects the prompt per item from a declared candidate list. Each
// candidate is a PromptRef. Render prints "one of <a> | <b> (by item)", the hash
// covers the list and RulePromptCandidates checks it; the walk never runs sel.
// The agent's definition must be a Typed[I, R] child with the same I (lint,
// RuleBuild). The executor runs sel on the child input when it issues the
// instruction; a reference outside the candidates is not used (see Executor,
// Declared obligations).
func PromptFor[I any](sel func(item I) string, candidates []string, inputs ...Input) Prompt {
	p := Prompt{inputs: inputs, candidates: append([]string{}, candidates...), item: reflect.TypeFor[I]()}
	if sel != nil {
		p.sel = func(item any) string { return sel(item.(I)) }
	}
	return p
}

// PromptFile references a prompt template file and its typed inputs.
func PromptFile(path string, inputs ...Input) Prompt { return Prompt{path: path, inputs: inputs} }

// PromptText is an inline prompt template and its typed inputs.
func PromptText(template string, inputs ...Input) Prompt {
	return Prompt{text: template, inputs: inputs}
}

// Check validates a submitted output; its name appears in the refusal shown to
// the agent.
//
// Owns: one named output validation, plus the Observations it reads: the
// definition-time set (Observes; it feeds the observation token) and the
// submission-scoped set (ObservesFrom).
// Does not: shell out or read the world directly; reads are declared with
// Observes or ObservesFrom.
// Extend by: a new check is a Checkf call.
type Check[O any] struct {
	examples []CheckExample
	Name     string
	fn       func(O, Ctx) error
	observes []Reader
	from     []func(O) []Reader
}

// Checkf builds a named check.
func Checkf[O any](name string, fn func(out O, c Ctx) error) Check[O] {
	return Check[O]{Name: name, fn: fn}
}

// Observes declares the observations the check reads.
func (k Check[O]) Observes(os ...Reader) Check[O] {
	k.observes = append(append([]Reader{}, k.observes...), os...)
	return k
}

// ObservesFrom declares observations built from the decoded submission at
// check time, for reads whose arguments come from the output under check (a
// reported commit, say). The descriptors are bound like Observes ones, read
// and journaled with the check's reads before the check runs, and served to
// its Observed. They are not read at issue time, so they are not part of the
// observation token and never make a submission Stale.
//
// Binding is template binding: a {name} in an argument taken from the
// submission is substituted from the instance scope (an unknown name blocks
// the submission). fn must validate submission-derived values before building
// readers from them (HeadCheck accepts only full commit SHAs), so an agent
// cannot steer a read through the scope.
func (k Check[O]) ObservesFrom(fn func(out O) []Reader) Check[O] {
	k.from = append(append([]func(O) []Reader{}, k.from...), fn)
	return k
}

// fromDescs is the descriptors of the submission-scoped observations of out.
func (k Check[O]) fromDescs(out O) []ObservationDesc {
	var ds []ObservationDesc
	for _, fn := range k.from {
		for _, r := range fn(out) {
			ds = append(ds, r.Desc())
		}
	}
	return ds
}

// Reference declares a prompt input as required or optional reference
// material. It has no effect on a ShellBound or ShellWords input.
func Reference(in Input, required bool) Input { return referenceInput{in, required} }

type referenceInput struct {
	in       Input
	required bool
}

func (r referenceInput) inputRef() inputDesc {
	d := r.in.inputRef()
	if d.shell != "" {
		return d // presented as bound, never as reference pages
	}
	d.reference = "optional"
	if r.required {
		d.reference = "required"
	}
	return d
}
