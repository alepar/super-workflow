package wf

import (
	"fmt"
	"strings"
	"testing"
)

type (
	hoS1   struct{}
	hoS2   struct{}
	hoS3   struct{}
	hoEnd  struct{}
	hoFan  struct{}
	hoRev  struct{}
	hoItem struct{ ID string }
	hoRes  struct{}
	hoOut  struct{}
	hoCA   struct{}
	hoCB   struct{}
	hoCEnd struct{}
)

// hoChain is S1 -> S2 -> S3 -> End, all agents.
func hoChain() *Def {
	d := New("ho-chain")
	s1 := NewAgent[hoS1, hoOut](d)
	s2 := NewAgent[hoS2, hoOut](d)
	s3 := NewAgent[hoS3, hoOut](d)
	end := NewTerminal[hoEnd](d, Success)
	s1.Spec(PromptText("1"), Retry[hoS1](Const(1), s1.To(end, "gave up")))
	s2.Spec(PromptText("2"), Retry[hoS2](Const(1), s2.To(end, "gave up")))
	s3.Spec(PromptText("3"), Retry[hoS3](Const(1), s3.To(end, "gave up")))
	e1, e2, e3 := s1.To(s2, "next"), s2.To(s3, "next"), s3.To(end, "next")
	s1.Route(func(hoOut, Ctx) Edge[hoS1] { return e1 }, e1)
	s2.Route(func(hoOut, Ctx) Edge[hoS2] { return e2 }, e2)
	s3.Route(func(hoOut, Ctx) Edge[hoS3] { return e3 }, e3)
	return d
}

func TestHandOutOrderLostDepthDefinition(t *testing.T) {
	d := hoChain()
	mk := func(id, cur string, pred string) *instance {
		in := &instance{ID: id, def: d, Cur: cur, Phase: phaseIssued}
		if pred != "" {
			in.Unit = &unit{State: UnitPending, Predecessor: pred}
		}
		return in
	}
	// Definition order is i1..i5. Depths: S1 3, S2 2, S3 1.
	ins := []*instance{mk("i1", "hoS3", ""), mk("i2", "hoS1", ""), mk("i3", "hoS2", ""), mk("i4", "hoS3", "x#1"), mk("i5", "hoS1", "")}
	st := &runState{insts: map[string]*instance{}}
	for _, in := range ins {
		st.insts[in.ID] = in
		st.order = append(st.order, in.ID)
	}
	c := &cycle{e: NewEngine(MemJournal(), nil, EngineOptions{}).(*Engine), st: st}

	ids := func(xs []*instance) string {
		var s []string
		for _, in := range xs {
			s = append(s, in.ID)
		}
		return strings.Join(s, ",")
	}
	// The lost reissue first, then deepest first, ties by definition order.
	if got, want := ids(c.handOutOrder(ins)), "i4,i2,i5,i3,i1"; got != want {
		t.Errorf("handOutOrder = %s, want %s", got, want)
	}
	// The input order does not matter: definition order comes from runState.order.
	rev := []*instance{ins[4], ins[3], ins[2], ins[1], ins[0]}
	if got, want := ids(c.handOutOrder(rev)), "i4,i2,i5,i3,i1"; got != want {
		t.Errorf("handOutOrder of reversed input = %s, want %s", got, want)
	}
	if ids(ins) != "i1,i2,i3,i4,i5" {
		t.Errorf("handOutOrder reordered its input: %s", ids(ins))
	}
}

func TestDepthsOfDef(t *testing.T) {
	e := NewEngine(MemJournal(), nil, EngineOptions{}).(*Engine)
	got := e.depths(hoChain())
	for state, want := range map[string]int{"hoS1": 3, "hoS2": 2, "hoS3": 1, "hoEnd": 0} {
		if got[state] != want {
			t.Errorf("depth of %s = %d, want %d", state, got[state], want)
		}
	}
	if again := e.depths(hoChain()); len(again) != 4 {
		t.Errorf("a second Def computed %v", again)
	}
}

// hoFanDef is an agent loop (a back edge), a Map over a two-agent child, then
// a review agent.
func hoFanDef() *Def {
	kid := New("ho-kid")
	child := Typed[hoItem, hoRes](kid)
	ka := NewAgent[hoCA, hoOut](kid)
	kb := NewAgent[hoCB, hoOut](kid)
	kend := NewTerminal[hoCEnd](kid, Success)
	ka.Spec(PromptText("a"), Retry[hoCA](Const(1), ka.To(kend, "gave up")))
	kb.Spec(PromptText("b"), Retry[hoCB](Const(1), kb.To(kend, "gave up")))
	ea, eb := ka.To(kb, "next"), kb.To(kend, "next")
	ka.Route(func(hoOut, Ctx) Edge[hoCA] { return ea }, ea)
	kb.Route(func(hoOut, Ctx) Edge[hoCB] { return eb }, eb)
	Yield(kend, func(Ctx) hoRes { return hoRes{} })

	d := New("ho-fan")
	m := NewMap[hoFan, hoItem, string, hoRes](d, FromValues(hoItem{"a"}, hoItem{"b"}), func(i hoItem) string { return i.ID }, child)
	rev := NewAgent[hoRev, hoOut](d)
	end := NewTerminal[hoEnd](d, Success)
	rev.Spec(PromptText("r"), Retry[hoRev](Const(1), rev.To(end, "gave up")))
	m.Parallel(2).Join(AllSettled(), m.To(rev, "met"), m.To(rev, "partial"))
	again, done := rev.To(rev, "again"), rev.To(end, "done")
	rev.Route(func(hoOut, Ctx) Edge[hoRev] { return done }, again, done)
	return d
}

func TestDepthsCountChildAndCycle(t *testing.T) {
	d := hoFanDef()
	e := NewEngine(MemJournal(), nil, EngineOptions{Defs: []*Def{d}}).(*Engine)
	got := e.depths(d)
	// The back edge of hoRev contributes 0: the review counts once. The Map
	// counts its child's two agents, then the review.
	if got["hoRev"] != 1 || got["hoFan"] != 3 {
		t.Fatalf("depths = %v, want hoRev 1 and hoFan 3", got)
	}

	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = e.withRun(run, func(c *cycle) error {
		if err := c.drive(); err != nil {
			return err
		}
		pending := c.pendingUnits()
		if len(pending) != 2 {
			return fmt.Errorf("pending units = %d, want 2", len(pending))
		}
		// A child at its first agent: its own two agents plus the parent's review.
		for _, in := range pending {
			if n := c.depth(in); n != 3 {
				return fmt.Errorf("depth of %s = %d, want 3", in.ID, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rawKind is a bare state kind for synthetic definitions: Inspect sees only
// its kind name.
type rawKind struct{ base }

func (rawKind) lint(*node) []Problem                   { return nil }
func (rawKind) execute(*cycle, *instance, *node) error { return nil }

// rawDef builds a definition from states given as name, kind and the names
// its edges go to.
func rawDef(name string, states [][]string) *Def {
	d := New(name)
	byName := map[string]*node{}
	for _, s := range states {
		n := &node{def: d, name: s[0], kind: rawKind{base{s[1]}}}
		d.nodes = append(d.nodes, n)
		byName[s[0]] = n
	}
	for i, s := range states {
		for _, to := range s[2:] {
			n := d.nodes[i]
			n.edges = append(n.edges, &edgeData{from: n, to: byName[to]})
		}
	}
	return d
}

// loopyDef is a synthetic definition shaped like a review-heavy workflow: k
// stages, each a work agent, its review agent and a compute gate. A review
// routes back to the work of its own stage and of every earlier one, a gate
// retries its stage's work or goes on to the next stage, and every work has a
// give-up edge. All the loops overlap, so the number of simple paths is
// exponential in k.
func loopyDef(k int) *Def {
	var states [][]string
	for i := range k {
		next := "Done"
		if i+1 < k {
			next = fmt.Sprintf("W%d", i+1)
		}
		review := []string{fmt.Sprintf("R%d", i), "agent", fmt.Sprintf("G%d", i)}
		for j := i; j >= 0; j-- {
			review = append(review, fmt.Sprintf("W%d", j))
		}
		states = append(states,
			[]string{fmt.Sprintf("W%d", i), "agent", fmt.Sprintf("R%d", i), "GaveUp"},
			review,
			[]string{fmt.Sprintf("G%d", i), "compute", next, fmt.Sprintf("W%d", i)},
		)
	}
	states = append(states, []string{"Done", "terminal"}, []string{"GaveUp", "terminal"})
	return rawDef("loopy", states)
}

// TestDepthsLinearOnOverlappingLoops guards the cost of computeDepths: on
// overlapping loops it examines each state once and each edge twice, where a
// longest simple path walk is exponential (about 2M steps at 12 stages). A
// walk that stops counting fails too. Every agent is on one loop, so each
// looping state has every agent ahead of it.
func TestDepthsLinearOnOverlappingLoops(t *testing.T) {
	const k = 12
	d := loopyDef(k)
	states, edges := len(d.nodes), 0
	for _, n := range d.nodes {
		edges += len(n.edges)
	}
	dd := computeDepths(d, map[*Def]bool{})
	if limit := states + 2*edges; dd.steps <= 0 || dd.steps > limit {
		t.Fatalf("computeDepths took %d steps on %d states and %d edges, want 1..%d", dd.steps, states, edges, limit)
	}
	for _, n := range d.nodes {
		want := 2 * k
		if n.kind.kindName() == "terminal" {
			want = 0
		}
		if dd.at[n.name] != want {
			t.Errorf("depth of %s = %d, want %d", n.name, dd.at[n.name], want)
		}
	}
}

// TestDepthsOfOverlappingLoopCountsItOnce: two loops share A (A <-> B, A <-> C)
// and A ends at a terminal. The loop is one component of three agents, so
// every state on it has depth 3; a longest simple path from A finds only 2.
func TestDepthsOfOverlappingLoopCountsItOnce(t *testing.T) {
	d := rawDef("two-loops", [][]string{
		{"A", "agent", "B", "C", "End"},
		{"B", "agent", "A"},
		{"C", "agent", "A"},
		{"End", "terminal"},
	})
	got := computeDepths(d, map[*Def]bool{}).at
	if s := fmt.Sprint(got); s != "map[A:3 B:3 C:3 End:0]" {
		t.Errorf("depths = %s, want map[A:3 B:3 C:3 End:0]", s)
	}
}

func BenchmarkDepthsOverlappingLoops(b *testing.B) {
	d := loopyDef(50)
	for b.Loop() {
		computeDepths(d, map[*Def]bool{})
	}
}
