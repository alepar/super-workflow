package wftest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// FuzzCoverage aggregates the seeds run against one definition. A suite of
// seeds that all stop near the start proves nothing, so reach is part of the
// property: Problems lists every agent state no seed issued (unless excused
// as unreachable by design, with a reason), every excused state a seed did
// reach, and a suite where no seed ends at success or a human gate.
type FuzzCoverage struct {
	Edges             []string          // qualified definition/state/edge[index], in declaration order
	EdgesReached      map[string]int    // edge identity to number of seeds that traversed it
	EdgeUnreachable   map[string]string // explicit unreachable-by-design reasons
	EdgeNotApplicable map[string]string // definition defaults excluded by structural class rules
	edgeViews         map[string]wf.EdgeView

	Def         string
	Seeds       int
	States      []string          // every agent state of the definition, by path
	Reached     map[string]int    // agent state path to the seeds that issued it
	Ends        map[string]int    // FuzzResult.End to seed count
	Mix         map[string]int    // summed FuzzResult.Mix
	Unreachable map[string]string // agent state path to why no seed can reach it
}

// NewFuzzCoverage lists the agent states of the factory's definition,
// children included, by the same paths FuzzResult.Reached uses.
func NewFuzzCoverage(factory func() *Sim, unreachable map[string]string) *FuzzCoverage {
	def := factory().def
	c := &FuzzCoverage{Def: def.Name(), Reached: map[string]int{}, Ends: map[string]int{}, Mix: map[string]int{}, Unreachable: unreachable}
	c.EdgesReached = map[string]int{}
	c.EdgeUnreachable = map[string]string{}
	c.EdgeNotApplicable = map[string]string{}
	c.edgeViews = map[string]wf.EdgeView{}
	seen := map[string]bool{}
	var walk func(wf.DefView)
	walk = func(v wf.DefView) {
		for _, state := range v.States {
			for i, edge := range state.Edges {
				key := fmt.Sprintf("%s/edge[%d]", state.Path, i)
				c.Edges = append(c.Edges, key)
				c.edgeViews[key] = edge
				if reason := state.DefaultEdgeInapplicable(i); reason != "" {
					c.EdgeNotApplicable[key] = reason
				}
				if edge.Unreachable {
					c.EdgeUnreachable[key] = edge.UnreachableReason
				}
			}
			path := strings.TrimPrefix(state.Path, def.Name()+"/")
			if state.Kind == "agent" && !seen[path] {
				seen[path] = true
				c.States = append(c.States, path)
			}
		}
		for _, child := range v.Children {
			walk(child)
		}
	}
	walk(wf.Inspect(def))
	sort.Strings(c.States)
	return c
}

// Add folds one seed's result in.
func (c *FuzzCoverage) Add(r *FuzzResult) {
	c.Seeds++
	c.addEdges(r)
	c.Ends[r.End]++
	for k, n := range r.Mix {
		c.Mix[k] += n
	}
	once := map[string]bool{}
	for _, state := range r.Reached {
		if !once[state] {
			once[state] = true
			c.Reached[state]++
		}
	}
}

// Problems is empty when the seeds covered the definition.
func (c *FuzzCoverage) Problems() []string {
	missing, violations := c.stateProblems()
	out := append(missing, violations...)
	sort.Strings(out)
	return out
}

func (c *FuzzCoverage) stateProblems() (missing, violations []string) {
	if c.Ends["success"]+c.Ends["partial"]+c.Ends["human gate"] == 0 {
		missing = append(missing, fmt.Sprintf("%s: no seed of %d ended at success, partial or a human gate", c.Def, c.Seeds))
	}
	known := map[string]bool{}
	for _, state := range c.States {
		known[state] = true
		_, excused := c.Unreachable[state]
		switch {
		case c.Reached[state] == 0 && !excused:
			missing = append(missing, fmt.Sprintf("%s/%s: no seed reached this agent state; reach it or mark it unreachable by design with a reason", c.Def, state))
		case c.Reached[state] > 0 && excused:
			violations = append(violations, fmt.Sprintf("%s/%s: marked unreachable (%s) but %d seeds reached it", c.Def, state, c.Unreachable[state], c.Reached[state]))
		}
	}
	for state := range c.Unreachable {
		if !known[state] {
			violations = append(violations, fmt.Sprintf("%s/%s: marked unreachable but the definition has no such agent state", c.Def, state))
		}
	}
	sort.Strings(missing)
	sort.Strings(violations)
	return missing, violations
}

func (c *FuzzCoverage) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fuzz coverage %s: %d seeds; ends %s; actions %s\n", c.Def, c.Seeds, counts(c.Ends), counts(c.Mix))
	for _, state := range c.States {
		mark := fmt.Sprintf("%d/%d", c.Reached[state], c.Seeds)
		if why, ok := c.Unreachable[state]; ok {
			mark = "unreachable: " + why
		}
		fmt.Fprintf(&b, "  %-60s %s\n", state, mark)
	}
	covered, uncovered := 0, 0
	for _, key := range c.Edges {
		if reason, ok := c.EdgeNotApplicable[key]; ok {
			fmt.Fprintf(&b, "n/a by class %s (%s): %s\n", key, c.edgeViews[key].Class, reason)
			continue
		}
		if _, ok := c.EdgeUnreachable[key]; ok {
			continue
		}
		if c.EdgesReached[key] > 0 {
			covered++
		} else {
			uncovered++
		}
	}
	fmt.Fprintf(&b, "edge coverage: covered=%d uncovered=%d unreachable=%d n/a by class=%d\n", covered, uncovered, len(c.EdgeUnreachable), len(c.EdgeNotApplicable))
	return b.String()
}

func counts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, " ")
}
