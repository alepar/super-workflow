package wf

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Inspect returns a read-only view of d's tree. Render, Mermaid and Hash are
// functions of it, so everything shown is pinned and nothing is shown that is
// not in the walk. It never executes a user route, check or predicate closure.
func Inspect(d *Def) DefView { return inspectDef(d, "", "", map[*Def]bool{}, true) }

// inspectStructure is Inspect without reading any asset: Assets stay nil.
// Who reads assets (the definition hash, a confined resolver) reads them
// itself, through its own reader.
//
// A definition handed to an engine (frozen: complete, and building onto it
// is a misuse) is walked once per process. Asset roots are the exception,
// resolved again on every call, since Def.Assets may still move them. Each
// caller gets its own DefView and Children slices, so setting a view's
// fields never reaches another caller; the slices and maps inside a view are
// shared and read-only. A definition not yet handed to an engine may still
// be under construction, and is walked afresh every time.
func inspectStructure(d *Def) DefView {
	if !d.frozen.Load() {
		return inspectDef(d, "", "", map[*Def]bool{}, false)
	}
	v, ok := structures.Load(d)
	if !ok {
		if onStructureWalk != nil {
			onStructureWalk(d)
		}
		v, _ = structures.LoadOrStore(d, inspectDef(d, "", "", map[*Def]bool{}, false))
	}
	return cloneViewTree(v.(DefView), "")
}

var structures sync.Map // *Def to its structural DefView

// onStructureWalk, when set (tests), sees each structure walk.
var onStructureWalk func(*Def)

// cloneViewTree copies v's Children slices, recursively, and resolves each
// view's asset root from its definition, inheriting root, as inspectDef does.
func cloneViewTree(v DefView, root string) DefView {
	if v.def.assets != "" {
		root = v.def.assets
	}
	v.assetRoot = root
	if v.Children != nil {
		v.Children = append([]DefView(nil), v.Children...)
		for i := range v.Children {
			v.Children[i] = cloneViewTree(v.Children[i], root)
		}
	}
	return v
}

// DefView is the data-only view of one definition.
type DefView struct {
	def       *Def
	assetRoot string // resolved declaration root, excluded from definition identity

	Assets     map[string]string `json:",omitempty"`
	Name       string
	Version    int
	States     []StateView // declaration order; States[0] is the start
	Params     []string
	ParamPaths map[string]PathBase `json:",omitempty"`
	RunKey     []string
	Counters   []string
	Locks      []string
	Options    []string
	Exports    []string
	Enums      map[string][]string
	Policy     []PolicyView
	Children   []DefView // Sub/Map children, first-use order, recursively
}

// StateView is the data-only view of one state.
type StateView struct {
	IntValues         []GateIntValueView `json:",omitempty"`
	Path, Name, Kind  string             // Path = "def/child/State"
	Intent            string             `json:",omitempty"`
	EquivalentAnswers string             `json:",omitempty"`
	Outcome           string             // terminals
	PartialWhen       bool               `json:",omitempty"` // a Success terminal that ends Partial when not ready
	FetchCoverage     bool               `json:",omitempty"` // an agent whose fetch coverage the definition reads (ReadsFetchCoverage)
	Output            string             // agent and compute output type
	Edges             []EdgeView
	Retry             *RetryView
	Cases             *CasesView
	Route             []string // candidate labels of a free-form Route, declaration order
	Reservation       string   // "reserved" | "policy-answerable" (gates)
	Question          string
	Locks             []string
	Consumes          []string
	Produces          []string
	Presents          []string
	Inputs            []string
	Checks            []string
	Observes          []string
	DoneWhen          string
	Prompt            string
	// PromptFor candidates; omitted otherwise so other definitions' hashes do not change
	PromptCandidates []string `json:",omitempty"`
	Argv             []string
	Dir              string            `json:",omitempty"` // explicit command directory; omitted so other definitions' hashes do not change
	CommandKey       []string          `json:",omitempty"` // command idempotency key parts; omitted so other definitions' hashes do not change
	Idempotent       bool              `json:",omitempty"` // command declared Idempotent; omitted so other definitions' hashes do not change
	Detached         time.Duration     `json:",omitempty"` // command declared Detached(max); omitted so other definitions' hashes do not change
	Stdin            string            `json:",omitempty"` // command Stdin template; omitted so other definitions' hashes do not change
	Cond             string            // wait condition
	Poll             time.Duration     // wait poll interval
	Source           string            // map item source
	Projection       string            `json:",omitempty"` // named item projection
	Key              string            // map item key type
	Child            string            // sub/map child def name
	Exits            map[string]string // child terminal marker -> parent edge label
	Join             string            // map join policy
	Parallel         int
	Writes           []string
	ItemWrites       bool
	Lease            time.Duration
	Sibling          string
	Requery          bool
	Dispatch         string // canonical agent dispatch hints, DispatchHints.String()
	MaxVisits        string // Budget.String()
	Measure          string `json:",omitempty"` // Measure: the strict progress measure bounding its cycles; omitted so other definitions' hashes do not change
	Result           string // terminal Yield result type

	// raw data for the lint rules; not part of the exported view or the hash.
	node     *node
	uses     []edgeUse
	useKinds []edgeUseKind
	inputs   []inputDesc
	presents []*artifactData
	child    *Def
	budgets  []Budget
}

// EdgeView is one declared edge.
type EdgeView struct {
	Unreachable            bool   `json:",omitempty"`
	UnreachableReason      string `json:",omitempty"`
	Label, To, When, Class string // Class = OutcomeClass fallback ("" for normal edges)
	Fallback               string // "exhausted" | "stalled" | ""
	Timeout                time.Duration
	Effects                []string // "bd.create key=...", "inc rounds"
	Optional               bool     `json:",omitempty"` // some use is Edge.Optional: a failed effect does not stop it
	// Over marks the edge a MaxVisits budget redirects to once its state's
	// visits are spent. It is derived from the state's MaxVisits, so it stays
	// out of the JSON view and the definition hash.
	Over        bool    `json:"-"`
	EndsOutcome Outcome `json:",omitempty"`
	Ends        string  `json:",omitempty"` // Edge.Ends: why this end is intentional; "" when unmarked
	// SendBack is a send-back edge's bound and fallback, "≤<budget>, then
	// <fallback label>"; omitted on every other edge so other definitions'
	// hashes do not change.
	SendBack string `json:",omitempty"`
}

// RetryView is a retry policy.
type RetryView struct {
	Max             string
	MaxConst        int // constant term of Max: its lower bound
	NoProgressAfter int
	Exhausted       string
	Stalled         string
	ByClass         map[string]string
	Backoff         map[string][]time.Duration // class name -> retry delays before its edge is taken
	Fingerprint     string
	Recovery        bool `json:",omitempty"` // an agent state's exhaustion is offered to the coordinator first (B2)
}

// CasesView is a declared case table.
type CasesView struct {
	Enum      string              // "" for an open table
	Cases     [][2]string         // value, edge label
	Routes    map[string][]string // value -> candidate labels, for OnRoute rows
	Otherwise string
}

// PolicyView is one AutonomyPolicy rule.
type PolicyView struct{ Gate, Answer, When, Park string }

func inspectDef(d *Def, prefix, assetRoot string, onPath map[*Def]bool, withAssets bool) DefView {
	onPath[d] = true
	defer delete(onPath, d)
	v := DefView{def: d, Name: d.name, Version: d.version, Enums: map[string][]string{}}
	if len(d.pathParams) > 0 {
		v.ParamPaths = map[string]PathBase{}
		for name, base := range d.pathParams {
			v.ParamPaths[name] = base
		}
	}
	for _, p := range d.params {
		v.Params = append(v.Params, p.name)
	}
	for _, p := range d.runKey {
		v.RunKey = append(v.RunKey, p.name)
	}
	for _, c := range d.counters {
		v.Counters = append(v.Counters, c.label())
	}
	for _, l := range d.locks {
		v.Locks = append(v.Locks, l.name+" ("+l.scope.String()+")")
	}
	if d.options != nil {
		v.Options = optionFields(d.options.typ)
	}
	for _, a := range d.exports {
		v.Exports = append(v.Exports, a.name)
	}
	for _, e := range d.enums {
		v.Enums[e.name] = append([]string{}, e.values...)
	}
	for _, r := range d.policy.rules {
		g := ""
		if r.gate != nil {
			g = r.gate.name
		}
		v.Policy = append(v.Policy, PolicyView{Gate: g, Answer: r.answer, When: r.when.String(), Park: r.park})
	}
	if prefix == "" && !withAssets {
		v.States = ownStates(d) // read-only, as every structural view
	} else {
		v.States = inspectStates(d, prefix+d.name)
	}
	if d.assets != "" {
		assetRoot = d.assets
	}
	v.assetRoot = assetRoot
	if withAssets {
		v.Assets, _ = assetContents(assetRoot, v.States)
	}
	seen := map[*Def]bool{}
	for _, c := range d.children {
		if c == nil || seen[c] || onPath[c] {
			continue
		}
		seen[c] = true
		v.Children = append(v.Children, inspectDef(c, prefix+d.name+"/", assetRoot, onPath, withAssets))
	}
	return v
}

func (s LockScope) String() string {
	if s == LockRunKey {
		return "run-key"
	}
	return "run"
}

func optionFields(t reflect.Type) []string {
	if t.Kind() != reflect.Struct {
		return []string{t.String()}
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out = append(out, f.Name+" "+f.Type.String())
	}
	return out
}

// ownStates is inspectStates(d, d.name), the state views under d's own
// name, walked once per process for a frozen definition (see
// inspectStructure). The slice is shared: callers only read it.
func ownStates(d *Def) []StateView {
	if !d.frozen.Load() {
		return inspectStates(d, d.name)
	}
	if v, ok := ownStateViews.Load(d); ok {
		return v.([]StateView)
	}
	v, _ := ownStateViews.LoadOrStore(d, inspectStates(d, d.name))
	return v.([]StateView)
}

var ownStateViews sync.Map // frozen *Def to its ownStates

// inspectStates is the one walk over a definition's own states (no children).
func inspectStates(d *Def, path string) []StateView {
	d.seal()
	out := make([]StateView, 0, len(d.nodes))
	for _, n := range d.nodes {
		sv := StateView{Path: path + "/" + n.name, Name: n.name, Kind: n.kind.kindName(), node: n}
		for _, e := range n.edges {
			ev := EdgeView{Label: e.label, To: targetName(n, e.to), Timeout: e.timeout, Ends: e.ends, EndsOutcome: e.endsOutcome, Unreachable: e.unreachable, UnreachableReason: e.unreachableReason}
			if e.when != nil {
				ev.When = e.when.String()
			}
			if e.class != 0 {
				ev.Class = e.class.String()
			}
			ev.Over = e.visits != nil
			if sb := e.sendBack; sb != nil {
				ev.SendBack = "≤" + sb.max.String() + ", then " + useLabel(sb.fallback)
			}
			sv.Edges = append(sv.Edges, ev)
		}
		for _, l := range n.locks {
			sv.Locks = append(sv.Locks, l.name)
		}
		for _, a := range n.consumes {
			sv.Consumes = append(sv.Consumes, a.name)
		}
		for _, a := range d.artifacts {
			if a.producer == n {
				sv.Produces = append(sv.Produces, a.name)
			}
		}
		sv.Measure = n.measure
		if n.maxVisits != nil {
			sv.MaxVisits = n.maxVisits.max.String()
			sv.budgets = append(sv.budgets, n.maxVisits.max)
		}
		if n.doneWhen != nil {
			sv.DoneWhen = n.doneWhen.cond
			sv.Observes = append(sv.Observes, obsString(n.doneWhen.obs))
		}
		if n.cases != nil {
			cv := &CasesView{}
			if n.cases.enum != nil {
				cv.Enum = n.cases.enum.name
			}
			for _, c := range n.cases.cases {
				label := "(route)"
				if c.hasRoute {
					if cv.Routes == nil {
						cv.Routes = map[string][]string{}
					}
					labels := []string{}
					for _, u := range c.cands {
						labels = append(labels, useLabel(u))
						sv.useAs(u, candidateUse)
					}
					cv.Routes[c.value] = labels
				} else {
					label = useLabel(c.e)
					sv.use(c.e)
				}
				cv.Cases = append(cv.Cases, [2]string{c.value, label})
			}
			if o := n.cases.otherwise; o != nil {
				cv.Otherwise = useLabel(*o)
				sv.use(*o)
			}
			sv.Cases = cv
		}
		if n.route != nil {
			sv.Route = []string{}
			for _, u := range n.route.cands {
				sv.Route = append(sv.Route, useLabel(u))
				sv.useAs(u, candidateUse)
			}
		}
		n.kind.view(n, &sv)
		if n.maxVisits != nil {
			sv.use(n.maxVisits.over)
		}
		for _, e := range n.edges {
			if sb := e.sendBack; sb != nil {
				sv.budgets = append(sv.budgets, sb.max)
				sv.useAs(sb.fallback, failedUse)
			}
		}
		out = append(out, sv)
	}
	return out
}

func targetName(from *node, to *node) string {
	if to == nil {
		return "?"
	}
	if to.def != from.def && to.def != nil {
		return to.def.name + "/" + to.name
	}
	return to.name
}

func useLabel(u edgeUse) string {
	if u.e == nil {
		return "?"
	}
	return u.e.label
}

func (v *StateView) edgeIndex(e *edgeData) int {
	for i, x := range v.node.edges {
		if x == e {
			return i
		}
	}
	return -1
}

type edgeUseKind uint8

const (
	ordinaryUse edgeUseKind = iota
	candidateUse
	failedUse
)

func (v *StateView) use(u edgeUse) { v.useAs(u, ordinaryUse) }

// useAs records one use of an edge: lint reads the raw use, the view gets its
// effects.
func (v *StateView) useAs(u edgeUse, kind edgeUseKind) {
	v.uses = append(v.uses, u)
	v.useKinds = append(v.useKinds, kind)
	i := v.edgeIndex(u.e)
	if i < 0 {
		return
	}
	v.Edges[i].Optional = v.Edges[i].Optional || u.optional
	// Multiset union: one use may carry an effect several times (an edge with
	// the same Inc twice), while several uses of one edge list it once.
	seen := map[string]int{}
	for _, ef := range applyOrder(u.effects) {
		s := effectString(ef)
		seen[s]++
		if count(v.Edges[i].Effects, s) < seen[s] {
			v.Edges[i].Effects = append(v.Edges[i].Effects, s)
		}
	}
}

// fallback marks e as a retry fallback edge.
func (v *StateView) fallback(e *edgeData, which string) {
	if i := v.edgeIndex(e); i >= 0 && v.Edges[i].Fallback == "" {
		v.Edges[i].Fallback = which
	}
}

func count(xs []string, s string) int {
	n := 0
	for _, x := range xs {
		if x == s {
			n++
		}
	}
	return n
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// outsideMarker ends the rendered effect an Outside effect is.
const outsideMarker = " (outside the run: a person settles it)"

func effectString(e Effect) string {
	if x, ok := e.(ExternalEffect); ok {
		k := "?"
		switch {
		case x.Key == nil:
		case x.Key.key() == "":
			k = "non-idempotent"
		default:
			k = x.Key.key()
		}
		s := x.effect() + " key=" + k + argsString(x.Args)
		if len(x.argTemplates) != 0 {
			encoded, _ := json.Marshal(x.argTemplates)
			s += " argTemplates=" + string(encoded)
		}
		if key, ok := x.Key.(textKey); ok {
			encoded, _ := json.Marshal(key.text.spans)
			s += " keyTemplate=" + string(encoded)
		}
		if x.Outside {
			s += outsideMarker
		}
		return s
	}
	return e.effect()
}

// argsString renders adapter args sorted by key (" k=v" each), each value
// through encodeArg, so the same args always serialise the same way and
// distinct args never collide in Render or Hash.
func argsString(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for _, k := range keys {
		s += " " + k + "=" + encodeArg(reflect.ValueOf(args[k]))
	}
	return s
}

// encodeArg is an injective, quoting encoding of an arg value: strings are
// quoted, slice elements and struct fields are delimited and named, and a
// nil reference is distinct from an empty one. It reads values through
// reflect kind getters and never calls Interface or user methods.
func encodeArg(v reflect.Value) string {
	if !v.IsValid() {
		return "nil"
	}
	switch v.Kind() {
	case reflect.String:
		return strconv.Quote(v.String())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return "nil"
		}
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = encodeArg(v.Index(i))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Map:
		if v.IsNil() {
			return "nil"
		}
		parts := make([]string, 0, v.Len())
		for it := v.MapRange(); it.Next(); {
			parts = append(parts, encodeArg(it.Key())+": "+encodeArg(it.Value()))
		}
		sort.Strings(parts)
		return "{" + strings.Join(parts, ", ") + "}"
	case reflect.Struct:
		t := v.Type()
		parts := make([]string, v.NumField())
		for i := range parts {
			parts[i] = t.Field(i).Name + ": " + encodeArg(v.Field(i))
		}
		return t.String() + "{" + strings.Join(parts, ", ") + "}"
	case reflect.Pointer:
		if v.IsNil() {
			return "nil"
		}
		return "&" + encodeArg(v.Elem())
	case reflect.Interface:
		if v.IsNil() {
			return "nil"
		}
		return encodeArg(v.Elem())
	default:
		return v.Kind().String()
	}
}

func obsString(o ObservationDesc) string {
	return o.Adapter + "." + o.Op + argsString(o.Args)
}

func inputString(i inputDesc) string {
	s := i.kind + " " + i.name
	if i.reference != "" {
		s += " (" + i.reference + " reference)"
	}
	if i.kind == "bound" {
		if i.shell != "" {
			s += " (shell " + i.shell + ")"
		}
		s += " = " + i.tmpl
	}
	if i.obs != nil {
		s += " = " + obsString(*i.obs)
	}
	if i.alias != "" {
		s += " as " + i.alias
	}
	return s
}

func constPart(b Budget) int {
	n := 0
	for _, t := range b.terms {
		if t.c == nil {
			n += t.k
		}
	}
	return n
}
