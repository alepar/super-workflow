package wf

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

type (
	cniFirst  struct{}
	cniSecond struct{}
	cniFin    struct{}
	cniAsk    struct{}
	cniDone   struct{}
	cniItem   struct{}
	cniRes    struct{}
	cniOut    struct{ OK bool }
)

func cniChild(name, prompt string) Child[cniItem, cniRes] {
	d := New(name)
	ask := NewAgent[cniAsk, cniOut](d)
	done := NewTerminal[cniDone](d, Success)
	d.OnFailure(Blocked, done)
	ask.Spec(PromptText(prompt), Retry[cniAsk](Const(1), ask.To(done, "gave up")))
	toDone := ask.To(done, "asked")
	ask.Route(func(cniOut, Ctx) Edge[cniAsk] { return toDone }, toDone)
	Yield(done, func(Ctx) cniRes { return cniRes{} })
	return Typed[cniItem, cniRes](d)
}

func cniDupDef() *Def {
	d := New("dup-children")
	in := func(Ctx) cniItem { return cniItem{} }
	a := NewSub[cniFirst](d, cniChild("retrieval", "first"), in)
	b := NewSub[cniSecond](d, cniChild("retrieval", "second"), in)
	fin := NewTerminal[cniFin](d, Success)
	a.Exits(OnExit[cniDone](a.To(b, "first done")))
	b.Exits(OnExit[cniDone](b.To(fin, "second done")))
	return d
}

// A run journaled before the duplicate-name refusal keeps resuming through
// Next: the refusal is at start (Engine.Run) only, never in NewEngine, the
// fold or runAt, so it strands no live run. Re-attaching through Run is
// refused like a start.
func TestDuplicateChildNamePreFixRunResumes(t *testing.T) {
	d := cniDupDef()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	j := MemJournal()
	opts := EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return now }}
	old := NewEngine(j, NewAdapters(), opts).(*Engine)
	id, err := old.runID(d, nil)
	if err != nil {
		t.Fatal(err)
	}
	// What an engine before the refusal did: start the run as given.
	if _, _, err := old.runAt(d, nil, nil, id, nil); err != nil {
		t.Fatalf("pre-fix start: %v", err)
	}

	e := NewEngine(j, NewAdapters(), opts).(*Engine)
	ins, err := e.Next(id, "")
	if err != nil {
		t.Fatalf("resume of a pre-fix run: %v", err)
	}
	if len(ins) != 1 || ins[0].Kind != InstructionAgent || !strings.Contains(ins[0].Prompt, "first") {
		t.Fatalf("next = %+v, want the first child's agent", ins)
	}
	if _, err := e.Run(d, nil, nil); err == nil || !strings.Contains(err.Error(), "re-attach or replay refused") {
		t.Fatalf("Run on the live pre-fix run = %v, want the re-attach refusal", err)
	}
}

// Replaying a pre-fix journal with duplicate names would only reproduce the
// shadowing: it is refused, and the message says so and names the fix.
func TestDuplicateChildNameReplayRefused(t *testing.T) {
	d := cniDupDef()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	j := MemJournal()
	old := NewEngine(j, NewAdapters(), EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return now }}).(*Engine)
	id, err := old.runID(d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.runAt(d, nil, nil, id, nil); err != nil {
		t.Fatalf("pre-fix start: %v", err)
	}
	unlock, err := j.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := j.Records(id)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Replay(recs, d, t.TempDir())
	if err == nil {
		t.Fatal("Replay accepted a duplicate child name")
	}
	for _, want := range []string{"replay refused", "give the child definitions distinct names"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("Replay error %q lacks %q", err, want)
		}
	}
}

// The refusal names where the two definitions are used and how to fix it.
func TestDuplicateChildNameMessage(t *testing.T) {
	d := cniDupDef()
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{Defs: []*Def{d}}).(*Engine)
	_, err := e.Run(d, nil, nil)
	if err == nil {
		t.Fatal("Run accepted a duplicate child name")
	}
	lint := ""
	for _, p := range d.Lint() {
		if p.Rule == RuleChildNameDuplicate {
			lint = p.Msg
		}
	}
	for _, want := range []string{`"retrieval"`, "dup-children/cniFirst", "dup-children/cniSecond", "give the child definitions distinct names", "must be identical"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("Run error %q lacks %q", err, want)
		}
		if !strings.Contains(strings.ToLower(lint), strings.ToLower(want)) {
			t.Errorf("lint message %q lacks %q", lint, want)
		}
	}
	for _, want := range []string{"start, re-attach or replay refused", "existing runs resume through next/resume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run error %q lacks %q", err, want)
		}
	}
}

type cniWork struct{}

// cniClosureChild is a child named "retrieval" whose compute appends tag to
// hits: two of them differ only in a closure, which the hash cannot see.
func cniClosureChild(tag int, hits *[]int) Child[cniItem, cniRes] {
	d := New("retrieval")
	work := NewCompute[cniWork](d, func(Ctx) (int, error) { *hits = append(*hits, tag); return tag, nil })
	done := NewTerminal[cniDone](d, Success)
	work.ThenTo(done, "computed")
	Yield(done, func(Ctx) cniRes { return cniRes{} })
	return Typed[cniItem, cniRes](d)
}

func cniSubs(name string, first, second Child[cniItem, cniRes]) *Def {
	d := New(name)
	in := func(Ctx) cniItem { return cniItem{} }
	a := NewSub[cniFirst](d, first, in)
	b := NewSub[cniSecond](d, second, in)
	fin := NewTerminal[cniFin](d, Success)
	a.Exits(OnExit[cniDone](a.To(b, "first done")))
	b.Exits(OnExit[cniDone](b.To(fin, "second done")))
	return d
}

// Siblings that share a name and a hash but differ in a closure are legal,
// and each runs its own definition: the fold resolves a child by the state
// that spawned it, not by its name.
func TestEqualHashSiblingsRunTheirOwnClosures(t *testing.T) {
	var hits []int
	one, two := cniClosureChild(1, &hits), cniClosureChild(2, &hits)
	if one.Def().Hash() != two.Def().Hash() {
		t.Fatal("fixture: the two children should hash equal")
	}
	d := cniSubs("closure-siblings", one, two)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint = %v, want none", ps)
	}
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{Defs: []*Def{d}}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
	}
	st, err := e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != RunEnded || st.Terminal != "cniFin" {
		t.Fatalf("status = %s at %q, want ended at cniFin", st.Phase, st.Terminal)
	}
	if !slices.Contains(hits, 2) || !slices.Contains(hits, 1) {
		t.Fatalf("computes ran %v, want both children's own (1 and 2)", hits)
	}
}

// spawnedChild resolves by the spawning state, and by name when that state
// is gone or now runs another definition (a journal from an older shape).
func TestSpawnedChildResolution(t *testing.T) {
	var hits []int
	one, two := cniClosureChild(1, &hits), cniClosureChild(2, &hits)
	d := cniSubs("resolve", one, two)
	cases := []struct {
		state, name string
		want        *Def
	}{
		{"cniSecond", "retrieval", two.Def()},
		{"cniFirst", "retrieval", one.Def()},
		{"Gone", "retrieval", one.Def()},   // state removed: first by name
		{"cniFin", "retrieval", one.Def()}, // not a Sub or Map: by name
		{"cniSecond", "other", nil},        // the state runs another definition
	}
	for _, c := range cases {
		if got := d.spawnedChild(c.state, c.name); got != c.want {
			t.Errorf("spawnedChild(%s, %s) = %p, want %p", c.state, c.name, got, c.want)
		}
	}
}

// A journal whose child_spawned names a spawning state the definition no
// longer has still folds: the child is found by name, and the run resumes.
func TestOldJournalSpawningStateGoneFallsBackToName(t *testing.T) {
	for _, state := range []string{"GoneState", ""} { // "" is a journal from before State
		t.Run("state="+state, func(t *testing.T) { oldJournalResumes(t, state) })
	}
}

func oldJournalResumes(t *testing.T, state string) {
	d := cniDupDef()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	src := MemJournal()
	opts := EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return now }}
	old := NewEngine(src, NewAdapters(), opts).(*Engine)
	id, err := old.runID(d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.runAt(d, nil, nil, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Next(id, ""); err != nil { // spawns the first child
		t.Fatal(err)
	}
	unlock, err := src.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := src.Records(id)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	dst := MemJournal()
	unlock, err = dst.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	spawned := 0
	for _, r := range recs {
		if r.Kind == JournalChildSpawned {
			var c ChildSpawnedData
			if err := json.Unmarshal(r.Data, &c); err != nil {
				t.Fatal(err)
			}
			c.State = state
			r.Data, _ = json.Marshal(c)
			spawned++
		}
		if err := dst.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	unlock()
	if spawned == 0 {
		t.Fatal("fixture: no child_spawned record")
	}
	e := NewEngine(dst, NewAdapters(), opts).(*Engine)
	ins, err := e.Next(id, "")
	if err != nil {
		t.Fatalf("resume of an old journal: %v", err)
	}
	if len(ins) != 1 || !strings.Contains(ins[0].Prompt, "first") {
		t.Fatalf("next = %+v, want the first child's agent, found by name", ins)
	}
}

type cniInner struct{}

// Locations are chains of host states, so two different parents that share a
// name do not print one place twice.
func TestDuplicateChildNameLocationsAreHostStates(t *testing.T) {
	parent := func(prompt string) Child[cniItem, cniRes] {
		d := New("p")
		s := NewSub[cniInner](d, cniChild("kid", prompt), func(Ctx) cniItem { return cniItem{} })
		done := NewTerminal[cniDone](d, Success)
		s.Exits(OnExit[cniDone](s.To(done, "inner done")))
		Yield(done, func(Ctx) cniRes { return cniRes{} })
		return Typed[cniItem, cniRes](d)
	}
	d := cniSubs("root", parent("a"), parent("b"))
	got := map[string][2]string{}
	for _, x := range childNameClashes(d) {
		got[x.name] = [2]string{x.first, x.second}
	}
	want := map[string][2]string{
		"p":   {"root/cniFirst", "root/cniSecond"},
		"kid": {"root/cniFirst/cniInner", "root/cniSecond/cniInner"},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("clash %q at %v, want %v", name, got[name], w)
		}
	}
}

// Two definitions that share a name and a structural hash but resolve
// different asset roots differ: the run keys asset roots by the chain of
// definition names, so one would read the other's prompts.
func TestDuplicateChildNameDifferentAssetRootsRefused(t *testing.T) {
	reviewer := func(dir string) Child[cniItem, cniRes] {
		c := cniChild("reviewer", "review")
		c.Def().Assets(dir)
		return c
	}
	a, b := t.TempDir(), t.TempDir()
	d := cniSubs("asset-roots", reviewer(a), reviewer(b))
	if reviewer(a).Def().Hash() != reviewer(b).Def().Hash() {
		t.Fatal("fixture: the two reviewers should hash equal")
	}
	found := false
	for _, p := range d.Lint() {
		found = found || (p.Rule == RuleChildNameDuplicate && strings.Contains(p.Msg, `"reviewer"`))
	}
	if !found {
		t.Errorf("lint = %v, want %s for reviewer", d.Lint(), RuleChildNameDuplicate)
	}
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{Defs: []*Def{d}}).(*Engine)
	if _, err := e.Run(d, nil, nil); err == nil {
		t.Error("Run accepted reviewers with different asset roots")
	}
	same := cniSubs("asset-roots-same", reviewer(a), reviewer(a))
	if ps := same.Lint(); len(ps) != 0 {
		t.Errorf("same asset root lint = %v, want none", ps)
	}
	// An inherited root counts: a child without its own root under parents
	// with different roots differs too.
	inherit := func(dir string) Child[cniItem, cniRes] {
		p := New("holder").Assets(dir)
		s := NewSub[cniInner](p, cniChild("kid", "same"), func(Ctx) cniItem { return cniItem{} })
		done := NewTerminal[cniDone](p, Success)
		s.Exits(OnExit[cniDone](s.To(done, "inner done")))
		Yield(done, func(Ctx) cniRes { return cniRes{} })
		return Typed[cniItem, cniRes](p)
	}
	got := map[string]bool{}
	for _, x := range childNameClashes(cniSubs("inherited", inherit(a), inherit(b))) {
		got[x.name] = true
	}
	if !got["holder"] || !got["kid"] {
		t.Errorf("inherited-root clashes = %v, want holder and kid", got)
	}
}

// cniAgentClosureChild waits at an agent, then runs a compute that appends
// tag to hits: its closure runs only after the agent's answer.
func cniAgentClosureChild(tag int, hits *[]int) Child[cniItem, cniRes] {
	d := New("retrieval")
	ask := NewAgent[cniAsk, cniOut](d)
	work := NewCompute[cniWork](d, func(Ctx) (int, error) { *hits = append(*hits, tag); return tag, nil })
	done := NewTerminal[cniDone](d, Success)
	d.OnFailure(Blocked, done)
	ask.Spec(PromptText("ask"), Retry[cniAsk](Const(1), ask.To(done, "gave up")))
	toWork := ask.To(work, "asked")
	ask.Route(func(cniOut, Ctx) Edge[cniAsk] { return toWork }, toWork)
	work.ThenTo(done, "computed")
	Yield(done, func(Ctx) cniRes { return cniRes{} })
	return Typed[cniItem, cniRes](d)
}

// A migration that swaps two host states' names: a child spawned before it
// keeps its own definition, re-resolved at its spawner's new name, not the
// sibling that now holds the old name.
func TestMigrationSwapKeepsChildDefinition(t *testing.T) {
	var hits []int
	in := func(Ctx) cniItem { return cniItem{} }
	v1 := New("swap")
	a1 := NewSub[cniFirst](v1, cniAgentClosureChild(1, &hits), in)
	b1 := NewSub[cniSecond](v1, cniAgentClosureChild(2, &hits), in)
	f1 := NewTerminal[cniFin](v1, Success)
	a1.Exits(OnExit[cniDone](a1.To(b1, "first done")))
	b1.Exits(OnExit[cniDone](b1.To(f1, "second done")))

	// v2: the state that ran child 1 is now cniSecond, and it runs first.
	v2 := New("swap").Version(2)
	b2 := NewSub[cniSecond](v2, cniAgentClosureChild(1, &hits), in)
	a2 := NewSub[cniFirst](v2, cniAgentClosureChild(2, &hits), in)
	f2 := NewTerminal[cniFin](v2, Success)
	b2.Exits(OnExit[cniDone](b2.To(a2, "was first done")))
	a2.Exits(OnExit[cniDone](a2.To(f2, "was second done")))
	v2.Migrations(Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: map[string]string{"cniFirst": "cniSecond", "cniSecond": "cniFirst"}})

	j := MemJournal()
	e1 := NewEngine(j, NewAdapters(), EngineOptions{Defs: []*Def{v1}, RunID: "swap-run"}).(*Engine)
	run, err := e1.Run(v1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ins, err := e1.Next(run, ""); err != nil || len(ins) != 1 {
		t.Fatalf("v1 next = %+v, %v", ins, err)
	}
	e2 := NewEngine(j, NewAdapters(), EngineOptions{Defs: []*Def{v2}, RunID: "swap-run"}).(*Engine)
	if _, err := e2.Run(v2, nil, nil); err != nil {
		t.Fatalf("adopting v2: %v", err)
	}
	ins, err := e2.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("v2 next = %+v, %v", ins, err)
	}
	if _, err := e2.Submit(run, Submission{Token: ins[0].Token, Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0] != 1 {
		t.Fatalf("computes ran %v, want child 1's own closure first", hits)
	}
}

type cniKeyed struct{ N int }
type (
	cniMapA struct{}
	cniMapB struct{}
)

func cniMapChild(tag int, hits *[]int) Child[cniKeyed, cniRes] {
	d := New("retrieval")
	work := NewCompute[cniWork](d, func(Ctx) (int, error) { *hits = append(*hits, tag); return tag, nil })
	done := NewTerminal[cniDone](d, Success)
	work.ThenTo(done, "computed")
	Yield(done, func(Ctx) cniRes { return cniRes{} })
	return Typed[cniKeyed, cniRes](d)
}

// Two Maps whose equal-hash children differ in closures: every item instance
// of each runs its own Map's child.
func TestEqualHashMapSiblingsRunTheirOwnClosures(t *testing.T) {
	var hits []int
	d := New("map-siblings")
	key := func(i cniKeyed) int { return i.N }
	a := NewMap[cniMapA](d, FromValues(cniKeyed{1}, cniKeyed{2}), key, cniMapChild(1, &hits))
	b := NewMap[cniMapB](d, FromValues(cniKeyed{1}, cniKeyed{2}), key, cniMapChild(2, &hits))
	fin := NewTerminal[cniFin](d, Success)
	a.Parallel(2).Join(AllOK(), a.To(b, "a met"), a.To(fin, "a partial"))
	b.Parallel(2).Join(AllOK(), b.To(fin, "b met"), b.To(fin, "b partial"))
	if ps := d.Lint(); slices.ContainsFunc(ps, func(p Problem) bool { return p.Severity == SeverityError }) {
		t.Fatalf("lint = %v", ps)
	}
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{Defs: []*Def{d}}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
	}
	count := map[int]int{}
	for _, h := range hits {
		count[h]++
	}
	if count[1] != 2 || count[2] != 2 {
		t.Fatalf("computes ran %v, want two of each Map's own", hits)
	}
}

// The journaled name exists under another state: when the recorded state
// now runs a definition of another name, the name wins.
func TestSpawnedChildNameUnderAnotherState(t *testing.T) {
	var hits []int
	d := New("moved")
	in := func(Ctx) cniItem { return cniItem{} }
	alpha := cniClosureChild(1, &hits)
	alpha.Def().name = "alpha"
	beta := cniClosureChild(2, &hits)
	beta.Def().name = "beta"
	a := NewSub[cniFirst](d, alpha, in)
	b := NewSub[cniSecond](d, beta, in)
	fin := NewTerminal[cniFin](d, Success)
	a.Exits(OnExit[cniDone](a.To(b, "a")))
	b.Exits(OnExit[cniDone](b.To(fin, "b")))
	if got := d.spawnedChild("cniFirst", "beta"); got != beta.Def() {
		t.Fatalf("spawnedChild(cniFirst, beta) = %p, want beta %p", got, beta.Def())
	}
}
