package wf

import (
	"encoding/json"
	"fmt"
	"sort"
)

// FetchCoverage is what paged delivery recorded of one agent attempt: per
// page set, how many pages it has and how many the child fetched. A page
// fetched twice counts once. It records fetches, never reading: a fetched
// page says the engine delivered it, not that the child read or used it.
//
// Owns: the data a route or compute reads with FetchCoverageOf.
// Does not: judge coverage. What enough coverage is, and where a short one
// routes, is the definition's policy.
type FetchCoverage struct {
	Unit string // the attempt's unit name
	// Prompt is the prompt's own pages. They are always required.
	Prompt PageCoverage
	// References are the prompt inputs delivered as reference pages, by name.
	References []ReferenceCoverage `json:",omitempty"`
}

// PageCoverage counts one page set.
type PageCoverage struct{ Pages, Fetched int }

// Complete reports whether every page was fetched.
func (p PageCoverage) Complete() bool { return p.Fetched == p.Pages }

// ReferenceCoverage is one reference input's pages: Name is the input's name
// (its As alias if it has one), ID the reference id the read command takes.
type ReferenceCoverage struct {
	Name, ID string
	Required bool
	PageCoverage
}

// RequiredComplete reports whether the prompt and every required reference
// were fetched in full.
func (f FetchCoverage) RequiredComplete() bool {
	if !f.Prompt.Complete() {
		return false
	}
	for _, r := range f.References {
		if r.Required && !r.Complete() {
			return false
		}
	}
	return true
}

// ReadsFetchCoverage declares that the definition reads this state's fetch
// coverage with FetchCoverageOf. Lint notes it, because coverage is known
// only in a run with paged delivery (--paged-prompts, or a Registration with
// PagedPrompts). A read of a state that did not declare it blocks the step.
func (a *Agent[S, O]) ReadsFetchCoverage() *Agent[S, O] {
	a.k.coverage = true
	return a
}

// FetchCoverageOf reads the fetch coverage of the attempt whose output agent
// state a last accepted in this instance, as paged delivery recorded it. Like
// OutputOf it is the latest accepted visit's, not this round's; a's own
// checks and route read the attempt they decide. ok is false
// when it is unknown: the run has no paged delivery, a has not accepted an
// output, or the accepted attempt never fetched its prompt. Unknown is never
// zero coverage. A retried attempt starts its own count: the record is the
// accepted attempt's alone.
//
// a must declare ReadsFetchCoverage; a read of a state that did not blocks
// the step.
func FetchCoverageOf[S, O any](c Ctx, a *Agent[S, O]) (FetchCoverage, bool) {
	f := read[*FetchCoverage](c, "fetchCoverage", a.n)
	if f == nil {
		return FetchCoverage{}, false
	}
	return *f, true
}

// unitCoverage counts what u fetched of its packet; nil without a packet.
func unitCoverage(u *unit) *FetchCoverage {
	if u == nil || u.Packet == nil {
		return nil
	}
	count := func(ref string, pages []string) PageCoverage {
		p := PageCoverage{Pages: len(pages)}
		for i := range pages {
			if u.PagesFetched[pageKey(ref, i+1)] {
				p.Fetched++
			}
		}
		return p
	}
	f := &FetchCoverage{Unit: u.Name, Prompt: count("", u.Packet.Pages)}
	for id, r := range u.Packet.References {
		f.References = append(f.References, ReferenceCoverage{Name: r.Name, ID: id, Required: r.Required, PageCoverage: count(id, r.Pages)})
	}
	sort.Slice(f.References, func(i, j int) bool {
		a, b := f.References[i], f.References[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return f
}

// fetchCoverageValue serves the fetchCoverage read: the accepted attempt's
// record, or JSON null when it is unknown.
func (x *runCtx) fetchCoverageValue(n *node) (any, error) {
	if k, ok := n.kind.(*agentKind); !ok || !k.coverage {
		x.blocked, x.why = true, fmt.Sprintf("FetchCoverageOf(%s): the state does not declare ReadsFetchCoverage", n.name)
		return nil, fmt.Errorf("%w: %s", ErrRejected, x.why)
	}
	f := x.in.Coverage[n.name]
	if u := x.in.Unit; x.in.Cur == n.name && x.in.Phase == phaseIssued && u != nil && !u.State.terminal() {
		// The state's own checks and route read the attempt being decided.
		f = unitCoverage(u)
	}
	b, err := json.Marshal(f)
	return json.RawMessage(b), err
}

// UnitCoverage is the fetch coverage of the live or settled unit called name,
// as status counts it; ok is false when the unit has fetched nothing yet or
// the run has no paged delivery.
func (e *Engine) UnitCoverage(run RunID, name string) (f FetchCoverage, ok bool, err error) {
	err = e.withDryRun(run, func(c *cycle) error {
		for _, id := range c.st.order {
			in := c.st.insts[id]
			if in.Unit != nil && in.Unit.Name == name {
				if p := unitCoverage(in.Unit); p != nil {
					f, ok = *p, true
				}
				return nil
			}
		}
		return fmt.Errorf("%w: no unit %q", ErrRejected, name)
	})
	return f, ok, err
}

// ruleFetchCoverage notes an agent state whose fetch coverage the definition
// reads: it is known only in a run with paged delivery.
func ruleFetchCoverage(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		if k, ok := n.kind.(*agentKind); ok && k.coverage {
			p := n.problem(RuleFetchCoverage, "the definition reads this state's fetch coverage; it is known only in a run with paged delivery (--paged-prompts or Registration.PagedPrompts), and records pages fetched, never read")
			p.Severity = SeverityNote
			out = append(out, p)
		}
	}
	return out
}
