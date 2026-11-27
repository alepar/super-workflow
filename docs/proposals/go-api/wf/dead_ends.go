package wf

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// DeadEnd is one route into a Stopped or Stalled terminal: the edge a
// deciding state takes, any states that only pass the run on to the
// terminal, the outcome that makes the deciding state take that edge, and
// whether one answer alone can take it.
//
// Owns: a static listing from the definition; it runs no closure and reads
// no journal.
// Does not: judge whether a route should end the run; that is the
// definition's choice, and Ends documents it.
type DeadEnd struct {
	Path    string   // the deciding state, "def/State" (child states keep their def/child prefix)
	Edge    string   // label of the edge the deciding state takes
	Via     []string // states between that edge and the terminal that pass every result on
	To      string   // the terminal
	Outcome Outcome
	Trigger string // what selects the edge: "case verdict=stop", "attempts exhausted (≤2)", ...
	// Single is "yes" when one answer alone takes the route (one agent
	// output, one class report, one Map child not ending in success), "no"
	// when it needs several (repeated failures, a visit cap, every child) or
	// is not an answer at all (a person, an exit code, a timeout), and
	// "unknown" when the definition decides in a closure lint cannot read.
	Single      string
	Why         string
	Ends        string // the edge's declared Ends reason; "" when unmarked
	Unreachable string // the edge's declared Unreachable reason
	// Open lists the variants under which every When guard on the route
	// holds; nil when no variants were given.
	Open []string
	// Policy lists the variants under which the definition's autonomy policy
	// answers this gate case itself.
	Policy []string
	// Causes are the child's routes that produce this one: the routes into
	// the mapped terminal for a Sub exit, the routes into a terminal that is
	// not success for a Map join's partial edge.
	Causes []DeadEnd
}

// DeadEnds lists every route that ends a run of d at a Stopped or Stalled
// terminal, in declaration order. variants names run options values, as
// wftest.CheckConfig.Variants does; with any, each route lists the variants
// it is open under, and each policy-answerable gate case the variants whose
// policy answers it. Sub and Map routes carry their child's routes as Causes.
func (d *Def) DeadEnds(variants map[string]any) ([]DeadEnd, error) {
	d.seal()
	vs, err := decodeVariants(d, variants)
	if err != nil {
		return nil, err
	}
	w := deadEndWalk{variants: vs, onPath: map[*Def]bool{}}
	return w.routesInto(Inspect(d), func(n *node) bool {
		k, ok := n.kind.(*terminalKind)
		return ok && (k.outcome == Stopped || k.outcome == Stalled)
	}), nil
}

type namedOptions struct {
	name  string
	value any
}

func decodeVariants(d *Def, variants map[string]any) ([]namedOptions, error) {
	var out []namedOptions
	for _, name := range slices.Sorted(maps.Keys(variants)) {
		var value any
		if d.options != nil {
			raw, err := json.Marshal(variants[name])
			if err != nil {
				return nil, fmt.Errorf("variant %s: marshalling options: %w", name, err)
			}
			v := reflect.New(d.options.typ)
			if err := json.Unmarshal(raw, v.Interface()); err != nil {
				return nil, fmt.Errorf("variant %s: decoding options as %s: %w", name, d.options.typ, err)
			}
			value = v.Elem().Interface()
		}
		out = append(out, namedOptions{name: name, value: value})
	}
	return out, nil
}

type deadEndWalk struct {
	variants []namedOptions
	onPath   map[*Def]bool
}

// trigger is one way a state selects an edge.
type trigger struct {
	u       edgeUse
	what    string
	single  string
	why     string
	answer  string // a gate case's answer, for the policy check
	failure bool   // a fallback, class route, timeout or visit cap: a forwarding state's own way out
	causes  func(w *deadEndWalk, parent DefView) []DeadEnd
}

// routesInto is every route of the definition viewed by v whose edge leads,
// directly or through forwarding states, into a terminal end accepts.
func (w *deadEndWalk) routesInto(v DefView, end func(*node) bool) []DeadEnd {
	if w.onPath[v.def] {
		return nil
	}
	w.onPath[v.def] = true
	defer delete(w.onPath, v.def)
	// hop is a forwarding state's one next state: every result that is not
	// a failure goes there. forward is the terminal a chain of hops reaches.
	hop := func(n *node) *node {
		var to *node
		for _, t := range triggersOf(n) {
			if t.failure {
				continue
			}
			if t.u.e.to == nil || (to != nil && t.u.e.to != to) {
				return nil
			}
			to = t.u.e.to
		}
		return to
	}
	forward := func(n *node) *node {
		seen := map[*node]bool{}
		for n != nil && !n.kind.terminal() && !seen[n] {
			seen[n] = true
			n = hop(n)
		}
		if n != nil && n.kind.terminal() {
			return n
		}
		return nil
	}
	var out []DeadEnd
	for i, sv := range v.States {
		n := sv.node
		if n.kind.terminal() {
			continue
		}
		forwarding := i > 0 && forward(n) != nil // the start state has no route into it to show it as Via
		for _, t := range triggersOf(n) {
			if forwarding && !t.failure {
				continue // shown as Via of the routes into this state
			}
			e := t.u.e
			if e == nil || e.to == nil {
				continue
			}
			guards := []*Pred{e.when}
			var via []string
			to := e.to
			if !to.kind.terminal() && forward(to) != nil {
				for !to.kind.terminal() {
					via = append(via, to.name)
					for _, x := range triggersOf(to) {
						if !x.failure {
							guards = append(guards, x.u.e.when)
						}
					}
					to = hop(to)
				}
			}
			if !to.kind.terminal() || !end(to) {
				continue
			}
			de := DeadEnd{
				Path: sv.Path, Edge: e.label, Via: via, To: to.name,
				Outcome: to.kind.(*terminalKind).outcome,
				Trigger: t.what, Single: t.single, Why: t.why,
				Ends: e.ends, Unreachable: e.unreachableReason,
			}
			if len(w.variants) > 0 {
				de.Open = []string{}
				for _, o := range w.variants {
					if guardsHold(guards, o.value) {
						de.Open = append(de.Open, o.name)
					}
				}
				if t.answer != "" {
					de.Policy = policyAnswers(n, t.answer, w.variants)
				}
			}
			if t.causes != nil {
				de.Causes = t.causes(w, v)
				de.Single, de.Why = singleFromCauses(de.Single, de.Why, de.Causes)
			}
			out = append(out, de)
		}
	}
	return out
}

// childView is child's view under parent, so its paths keep parent's prefix.
func childView(parent DefView, child *Def) DefView {
	for _, c := range parent.Children {
		if c.def == child {
			return c
		}
	}
	return Inspect(child)
}

func guardsHold(guards []*Pred, opts any) bool {
	for _, g := range guards {
		if g != nil && !predMatch(*g, opts) {
			return false
		}
	}
	return true
}

// policyAnswers is the variants whose first matching policy rule for gate n
// answers it with answer.
func policyAnswers(n *node, answer string, variants []namedOptions) []string {
	if k, ok := n.kind.(*gateKind); !ok || k.reservation != PolicyAnswerable {
		return nil
	}
	var out []string
	for _, o := range variants {
		for _, r := range n.def.policy.rules {
			if r.gate == n && predMatch(r.when, o.value) {
				if r.answer == answer {
					out = append(out, o.name)
				}
				break
			}
		}
	}
	return out
}

// singleFromCauses refines a Sub exit's or a Map partial's reachability by
// its child's routes. single is the parent's own: "yes" when one child ending
// there takes the edge, "unknown" when the join's need depends on the item
// count. One answer takes the route only when it takes the parent edge and
// some child route alone.
func singleFromCauses(single, why string, causes []DeadEnd) (string, string) {
	if single == "no" || len(causes) == 0 {
		return single, why
	}
	best := "no"
	var by DeadEnd
	for _, c := range causes {
		if c.Single == "yes" || (c.Single == "unknown" && best == "no") {
			best, by = c.Single, c
		}
	}
	switch {
	case best == "no":
		return "no", why + "; no child route is taken by one answer"
	case best == "yes" && single == "yes":
		return "yes", why + "; one answer in the child takes it (" + by.Path + ` "` + by.Edge + `")`
	case best == "yes":
		return "unknown", why + "; one answer in the child can end that child (" + by.Path + ` "` + by.Edge + `")`
	}
	return "unknown", why + "; a child route is decided in a closure"
}

// triggersOf lists the ways state n selects each of its edges, in the order
// render shows them: routing, then retry fallbacks and class routes, then
// timeouts and visit caps, then route-arounds.
func triggersOf(n *node) []trigger {
	var out []trigger
	routed := map[*edgeData]bool{}
	add := func(t trigger) {
		if t.u.e != nil {
			routed[t.u.e] = true
			out = append(out, t)
		}
	}
	answerBy := func() (string, string) {
		switch n.kind.(type) {
		case *agentKind:
			return "yes", "one accepted output of " + n.name
		case *gateKind:
			return "no", "a person's answer"
		case *commandKind:
			return "no", "the command's result"
		}
		return "unknown", n.name + " decides in a closure"
	}
	if cs := n.cases; cs != nil {
		what := "case"
		if _, ok := n.kind.(*gateKind); ok {
			what = "answer"
		} else if cs.enum != nil {
			what = "case " + cs.enum.name
		}
		single, why := answerBy()
		for _, c := range cs.cases {
			if c.hasRoute {
				for _, u := range c.cands {
					add(trigger{u: u, what: fmt.Sprintf("%s=%s, routed", what, c.value), single: single, why: why, answer: c.value})
				}
				continue
			}
			add(trigger{u: c.e, what: fmt.Sprintf("%s=%s", what, c.value), single: single, why: why, answer: c.value})
		}
		if cs.otherwise != nil {
			add(trigger{u: *cs.otherwise, what: what + " otherwise", single: single, why: why})
		}
	}
	if r := n.route; r != nil {
		single, why := answerBy()
		what := "route selects it"
		if len(r.cands) == 1 {
			what = "every result"
		}
		for _, u := range r.cands {
			add(trigger{u: u, what: what, single: single, why: why})
		}
	}
	switch k := n.kind.(type) {
	case *waitKind:
		if k.readyTo != nil {
			add(trigger{u: *k.readyTo, what: "condition " + k.cond + " ready", single: "no", why: "an observation"})
		}
		if k.timeout != nil {
			add(trigger{u: *k.timeout, what: fmt.Sprintf("not ready within %s", k.timeout.e.timeout), single: "no", why: "a timeout", failure: true})
		}
	case *gateKind:
		if k.timeout != nil {
			add(trigger{u: *k.timeout, what: fmt.Sprintf("no answer within %s", k.timeout.e.timeout), single: "no", why: "a timeout", failure: true})
		}
	case *subKind:
		for _, x := range k.exits {
			child, terminal := k.child, x.terminal
			add(trigger{u: x.e, what: fmt.Sprintf("child %s ends at %s", child.name, terminal), single: "yes", why: "the child ending there takes it",
				causes: func(w *deadEndWalk, parent DefView) []DeadEnd {
					return w.routesInto(childView(parent, child), func(t *node) bool { return t.name == terminal })
				}})
		}
	case *mapKind:
		if k.met != nil {
			add(trigger{u: *k.met, what: "join met (" + k.join.String() + ")", single: "no", why: "the join over every child"})
		}
		if k.partial != nil {
			single, why := "unknown", "how many children the join needs depends on the item count"
			switch k.join.kind {
			case "all_ok", "all_settled":
				single, why = "yes", "one child not ending in success takes it"
			case "predicate":
				why = "the join predicate decides"
			}
			child := k.child
			add(trigger{u: *k.partial, what: "join partial (" + k.join.String() + ")", single: single, why: why,
				causes: func(w *deadEndWalk, parent DefView) []DeadEnd {
					return w.routesInto(childView(parent, child), func(t *node) bool { return t.kind.(*terminalKind).outcome != Success })
				}})
		}
		for _, class := range sortedClasses(k.byClass) {
			add(trigger{u: k.byClass[class], what: "class " + class.String(), single: "no", why: "the Map's own " + class.String() + " result", failure: true})
		}
	}
	if r := retryOf(n); r != nil {
		_, agent := n.kind.(*agentKind)
		single, why := "no", fmt.Sprintf("after %s failed attempts", r.max)
		if agent && constPart(r.max) <= 1 && len(r.max.Counters()) == 0 {
			single, why = "yes", "one rejected attempt spends the budget"
		}
		if offersRecovery(n) {
			why += "; the coordinator is offered a restart first"
		}
		add(trigger{u: r.exhausted, what: fmt.Sprintf("attempts exhausted (≤%s)", r.max), single: single, why: why, failure: true})
		if r.stalled != nil {
			add(trigger{u: *r.stalled, what: fmt.Sprintf("stalled after %d unchanged attempts", r.noProgress), single: "no", why: "repeated attempts", failure: true})
		}
		for _, class := range sortedClasses(r.byClass) {
			add(classTrigger(n, r.byClass[class], class, len(r.backoff[class]), ""))
		}
	}
	for _, class := range sortedClasses(n.defaults) {
		add(classTrigger(n, n.defaults[class], class, 0, " (definition default)"))
	}
	if n.maxVisits != nil {
		add(trigger{u: n.maxVisits.over, what: fmt.Sprintf("visit cap %s reached", n.maxVisits.max), single: "no", why: "repeated visits", failure: true})
	}
	for _, e := range n.edges {
		if e.when != nil && !routed[e] {
			add(trigger{u: edgeUse{e: e, effects: e.effects, optional: e.optional}, what: "route-around when " + e.when.String(), single: "no", why: "a run option"})
		}
	}
	return out
}

func classTrigger(n *node, u edgeUse, class OutcomeClass, retries int, suffix string) trigger {
	t := trigger{u: u, what: "class " + class.String() + suffix, single: "no", why: "a result of class " + class.String(), failure: true}
	if _, agent := n.kind.(*agentKind); agent {
		t.single, t.why = "yes", "one "+class.String()+" report from "+n.name
	}
	if retries > 0 {
		t.what = fmt.Sprintf("class %s after %d retries%s", class, retries, suffix)
		t.single, t.why = "no", "repeated "+class.String()+" results"
	}
	return t
}

func sortedClasses[V any](m map[OutcomeClass]V) []OutcomeClass {
	return slices.Sorted(maps.Keys(m))
}

// FormatDeadEnds renders DeadEnds output as an indented text report.
func FormatDeadEnds(name string, ds []DeadEnd) string {
	var b strings.Builder
	single := 0
	for _, d := range ds {
		if d.Single == "yes" {
			single++
		}
	}
	fmt.Fprintf(&b, "dead ends of %s: %d route(s) into a stopped or stalled terminal, %d taken by one answer\n", name, len(ds), single)
	var write func(ds []DeadEnd, indent string)
	write = func(ds []DeadEnd, indent string) {
		for _, d := range ds {
			path := d.Path + ` -"` + d.Edge + `"-> `
			for _, v := range d.Via {
				path += v + " -> "
			}
			fmt.Fprintf(&b, "%s%s%s (%s)\n", indent, path, d.To, d.Outcome)
			fmt.Fprintf(&b, "%s  trigger: %s\n", indent, d.Trigger)
			fmt.Fprintf(&b, "%s  single answer: %s, %s\n", indent, d.Single, d.Why)
			if d.Ends != "" {
				fmt.Fprintf(&b, "%s  ends: %s\n", indent, d.Ends)
			}
			if d.Unreachable != "" {
				fmt.Fprintf(&b, "%s  declared unreachable: %s\n", indent, d.Unreachable)
			}
			if d.Open != nil {
				open := strings.Join(d.Open, ", ")
				if open == "" {
					open = "no variant"
				}
				fmt.Fprintf(&b, "%s  open under: %s\n", indent, open)
			}
			if len(d.Policy) > 0 {
				fmt.Fprintf(&b, "%s  answered by policy under: %s\n", indent, strings.Join(d.Policy, ", "))
			}
			if len(d.Causes) > 0 {
				fmt.Fprintf(&b, "%s  caused by:\n", indent)
				write(d.Causes, indent+"    ")
			}
		}
	}
	write(ds, "  ")
	return b.String()
}

func init() {
	Subcommands = append(Subcommands, Subcommand{Name: "dead-ends", Flags: []string{"--variant name=JSON", "--json"},
		Summary: "list every route of <definition> into a stopped or stalled terminal: its trigger and whether one answer takes it (--variant: per options value)",
		handle:  deadEndsCommand})
}

type variantFlags map[string]any

func (v variantFlags) String() string { return "" }

func (v variantFlags) Set(s string) error {
	name, raw, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return fmt.Errorf("want name=JSON, got %q", s)
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return fmt.Errorf("variant %s: %w", name, err)
	}
	v[name] = value
	return nil
}

func deadEndsCommand(c *cli, row Subcommand, args []string) int {
	fs := flag.NewFlagSet(row.Name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	variants := variantFlags{}
	fs.Var(variants, "variant", "a named run options value, name=JSON (repeatable)")
	asJSON := fs.Bool("json", false, "print the routes as JSON")
	usage := "dead-ends: usage: dead-ends <definition> [--variant name=JSON]... [--json]"
	name, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, rest = args[0], args[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return c.failf(exitUsage, "%s", usage)
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return c.failf(exitUsage, "%s", usage)
	}
	if name == "" {
		return c.failf(exitUsage, "dead-ends: expected <definition>")
	}
	reg := c.registration(name)
	if reg == nil {
		return c.failf(exitUsage, "dead-ends: unknown definition %s", name)
	}
	ds, err := reg.Def.DeadEnds(variants)
	if err != nil {
		return c.failf(exitUsage, "dead-ends %s: %v", name, err)
	}
	if *asJSON {
		if ds == nil {
			ds = []DeadEnd{}
		}
		return c.printJSON(ds)
	}
	fmt.Fprint(c.stdout, FormatDeadEnds(name, ds))
	return exitOK
}
