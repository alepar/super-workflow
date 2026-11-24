package wf

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

type (
	wVerdict string
	wExit    int
	wAnswer  string
	wOpts    struct{ Autonomous bool }
	wItem    struct{ ID string }
	wOut     struct{ V wVerdict }
	wRes     struct{}
)

const (
	wApproved wVerdict = "approved"
	wChanges  wVerdict = "changes_requested"
)

// marker types
type (
	wWrite   struct{}
	wRun     struct{}
	wAsk     struct{}
	wNested  struct{}
	wFan     struct{}
	wFin     struct{}
	wStop    struct{}
	wOrphan  struct{}
	wWork    struct{}
	wOK      struct{}
	wBad     struct{}
	wItemRun struct{}
	wItemEnd struct{}
)

func subChild() Child[wItem, wRes] {
	d := New("nested")
	work := NewAgent[wWork, string](d)
	ok := NewTerminal[wOK](d, Success)
	bad := NewTerminal[wBad](d, Stopped)
	d.OnFailure(Blocked, bad)
	toOK, toBad := work.To(ok, "fine"), work.To(bad, "broken")
	work.Spec(PromptText("do {{item}}", ChildInput()), Retry(Const(2), toBad))
	work.Route(func(string, Ctx) Edge[wWork] { return toOK }, toOK)
	Yield(ok, func(Ctx) wRes { return wRes{} })
	Yield(bad, func(Ctx) wRes { return wRes{} })
	return Typed[wItem, wRes](d)
}

func itemChild() Child[wItem, wRes] {
	d := New("item")
	run := NewAgent[wItemRun, string](d)
	end := NewTerminal[wItemEnd](d, Success)
	d.OnFailure(Blocked, end)
	ok := run.To(end, "done")
	run.Spec(PromptText("work on {{item}}", ChildInput()), Retry(Const(2), ok))
	run.Route(func(string, Ctx) Edge[wItemRun] { return ok }, ok)
	Yield(end, func(Ctx) wRes { return wRes{} })
	return Typed[wItem, wRes](d)
}

// validDef exercises every kind; it must lint clean.
func validDef() *Def {
	d := New("valid").Version(2)
	epic := d.Param("epic")
	rounds := d.Counter("rounds")
	merge := d.Lock("merge", LockRun)
	opts := Options[wOpts](d)
	verdicts := NewEnum(d, "verdict", wApproved, wChanges)
	exits := NewEnum(d, "exit", wExit(0), wExit(2))
	answers := NewEnum(d, "answer", wAnswer("go"), wAnswer("stop"))

	write := NewAgent[wWrite, wOut](d)
	run := NewCommand[wRun](d, "scripts/check", "--json")
	ask := NewGate[wAsk](d, "Proceed?", answers, PolicyAnswerable)
	nested := NewSub[wNested](d, subChild(), func(Ctx) wItem { return wItem{} })
	items := Observe[[]wItem]("bd", "ready", nil)
	fan := NewMap[wFan](d, FromObservation(items), func(it wItem) string { return it.ID }, itemChild())
	fin := NewTerminal[wFin](d, Success)
	stop := NewTerminal[wStop](d, Stopped)
	d.OnFailure(Blocked, stop)

	toRun := write.To(run, "written")
	giveUp := write.To(stop, "gave up")
	write.Spec(PromptFile("prompts/write.md", epic),
		Retry(Const(3).Plus(Count(rounds)), giveUp),
		Checkf("has verdict", func(wOut, Ctx) error { return nil }))
	write.RouteCases(Cases(func(o wOut) wVerdict { return o.V }, verdicts,
		On(wApproved, toRun), On(wChanges, toRun.With(rounds.Inc(), ExternalEffect{Adapter: "bd", Op: "create", Key: KeyOf("fix")}))))
	MaxVisits(Const(9), giveUp)

	pass, fail := run.To(ask, "pass"), run.To(stop, "fail")
	run.RouteCases(Cases(ExitAs[wExit], exits, On(wExit(0), pass), On(wExit(2), fail)))
	Hold(run, merge)

	goOn, halt := ask.To(nested, "go"), ask.To(stop, "stop")
	ask.Answers(On(wAnswer("go"), goOn), On(wAnswer("stop"), halt)).Timeout(time.Hour, halt)

	nested.Exits(OnExit[wOK](nested.To(fan, "nested ok")), OnExit[wBad](nested.To(stop, "nested bad")))

	met, partial := fan.To(fin, "all ok"), fan.To(stop, "partial")
	fan.Parallel(2).Writes("docs/**").ItemWrites(func(wItem) []string { return nil }).Join(AllOK(), met, partial)

	d.SetPolicy(Policy(PolicyAnswer(ask, wAnswer("go"), Field(opts, func(o *wOpts) *bool { return &o.Autonomous }).Is(true)).Park("auto-approved")))
	return d
}

// invalidDef breaks four rules: an unreachable state, a reserved gate with a
// policy rule, a Cases table missing one Enum value, and an unmapped Sub exit.
func invalidDef() *Def {
	d := New("invalid")
	opts := Options[wOpts](d)
	verdicts := NewEnum(d, "verdict", wApproved, wChanges)
	answers := NewEnum(d, "answer", wAnswer("go"), wAnswer("stop"))

	write := NewAgent[wWrite, wOut](d)
	ask := NewGate[wAsk](d, "Sure?", answers, Reserved)
	nested := NewSub[wNested](d, subChild(), func(Ctx) wItem { return wItem{} })
	end := NewTerminal[wFin](d, Success)
	_ = NewTerminal[wOrphan](d, Stopped)

	toAsk := write.To(ask, "written")
	write.Spec(PromptText("write"), Retry(Const(1), toAsk))
	write.RouteCases(Cases(func(o wOut) wVerdict { return o.V }, verdicts, On(wApproved, toAsk)))

	ask.Answers(On(wAnswer("go"), ask.To(nested, "go")), On(wAnswer("stop"), ask.To(end, "stop")))
	nested.Exits(OnExit[wOK](nested.To(end, "ok")))
	d.SetPolicy(Policy(PolicyAnswer(ask, wAnswer("go"), Field(opts, func(o *wOpts) *bool { return &o.Autonomous }).Is(true))))
	return d
}

func rulesOf(ps []Problem) map[RuleID]Problem {
	m := map[RuleID]Problem{}
	for _, p := range ps {
		m[p.Rule] = p
	}
	return m
}

func TestValidLintsClean(t *testing.T) {
	if ps := validDef().Lint(); len(ps) != 0 {
		t.Fatalf("valid definition has problems: %v", ps)
	}
}

func TestInvalidReportsEveryProblem(t *testing.T) {
	got := rulesOf(invalidDef().Lint())
	for _, r := range []RuleID{RuleUnreachable, RuleReservedPolicy, RuleCaseUncovered, RuleSubExitUnmapped} {
		p, ok := got[r]
		if !ok {
			t.Errorf("missing rule %s in %v", r, got)
			continue
		}
		if !strings.Contains(p.Path, "/") {
			t.Errorf("%s path %q is not qualified", r, p.Path)
		}
	}
	if p := got[RuleUnreachable]; p.Path != "invalid/Orphan" && p.Path != "invalid/wOrphan" {
		t.Errorf("unreachable path = %q", p.Path)
	}
}

func TestChildProblemsAreQualified(t *testing.T) {
	d := New("parent")
	ch := New("kid")
	a := NewAgent[wWork, string](ch)
	_ = a // no Spec, no routing: agent-retry and unrouted
	typed := Typed[wItem, wRes](ch)
	s := NewSub[wNested](d, typed, func(Ctx) wItem { return wItem{} })
	s.Exits()
	var paths []string
	for _, p := range d.Lint() {
		if p.Rule == RuleAgentRetry {
			paths = append(paths, p.Path)
		}
	}
	if len(paths) != 1 || paths[0] != "parent/kid/wWork" {
		t.Fatalf("agent-retry paths = %v, want [parent/kid/wWork]", paths)
	}
}

func TestTerminalResultInUntypedDef(t *testing.T) {
	d := New("plain")
	end := NewTerminal[wFin](d, Success)
	Yield(end, func(Ctx) wRes { return wRes{} })
	if _, ok := rulesOf(d.Lint())[RuleSubResultType]; !ok {
		t.Fatalf("terminal with a result in an untyped def not flagged: %v", d.Lint())
	}
}

func TestSubRecursiveTerminates(t *testing.T) {
	d := New("loop")
	c := Typed[wItem, wRes](d)
	NewSub[wNested](d, c, func(Ctx) wItem { return wItem{} })
	if _, ok := rulesOf(d.Lint())[RuleSubRecursive]; !ok {
		t.Fatal("recursive sub not flagged")
	}
	_ = d.Render()
	_ = d.Mermaid()
	_ = d.Hash()
}

func TestRenderShowsStructure(t *testing.T) {
	out := validDef().Render()
	for _, want := range []string{"valid v2 — 7 states, start: wWrite", "cases verdict: approved → written", "policy: go when Autonomous=true (park: auto-approved)",
		"[policy-answerable]", "┆ child nested", "┆ child item", "retry ≤3 + rounds", "map over bd.ready by string ×2 join all_ok",
		"after 1h0m0s", "bd.create key=fix", "inc rounds", "max visits: 9", "holds: merge"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(invalidDef().Render(), "[reserved]") {
		t.Error("reserved gate not rendered")
	}
}

func retryDef(np int) *Def {
	d := New("stall")
	a := NewAgent[wWrite, string](d)
	end := NewTerminal[wFin](d, Stopped)
	out := a.To(end, "out")
	r := Retry(Const(3), out)
	if np > 0 {
		r = r.NoProgressAfter(np)
	}
	a.Spec(PromptText("p"), r)
	a.Route(func(string, Ctx) Edge[wWrite] { return out }, out)
	return d
}

func TestStallOnlyWhenSetBelowBudget(t *testing.T) {
	for _, tc := range []struct {
		np   int
		want bool
	}{{0, false}, {2, true}, {3, false}, {5, false}} {
		got := strings.Contains(retryDef(tc.np).Render(), "stall after")
		if got != tc.want {
			t.Errorf("NoProgressAfter(%d): stall shown = %v, want %v", tc.np, got, tc.want)
		}
	}
	if !strings.Contains(retryDef(2).Render(), "stall after 2") {
		t.Error("want 'stall after 2'")
	}
}

func TestHashStableAndSensitive(t *testing.T) {
	h := validDef().Hash()
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+16 {
		t.Fatalf("hash = %q", h)
	}
	if validDef().Hash() != h {
		t.Error("same definition, different hash")
	}
	d := validDef()
	d.nodes[0].edges[0].label = "renamed"
	if d.Hash() == h {
		t.Error("renamed edge label did not change the hash")
	}
}

func TestMermaidFromSameWalk(t *testing.T) {
	d := validDef()
	m := d.Mermaid()
	if !strings.HasPrefix(m, "stateDiagram-v2\n") || !strings.Contains(m, "[*] --> wWrite") {
		t.Fatalf("mermaid header:\n%s", m)
	}
	var walk func(v DefView)
	walk = func(v DefView) {
		for _, s := range v.States {
			if !strings.Contains(m, s.Name) {
				t.Errorf("state %s missing from mermaid", s.Name)
			}
			for _, e := range s.Edges {
				if !strings.Contains(m, e.Label) {
					t.Errorf("edge label %q missing from mermaid", e.Label)
				}
			}
		}
		for _, c := range v.Children {
			walk(c)
		}
	}
	walk(Inspect(d))
	if !strings.Contains(m, "state nested {") || !strings.Contains(m, "policy-answerable") {
		t.Errorf("child block or gate note missing:\n%s", m)
	}
}

func TestMainSubcommands(t *testing.T) {
	call := func(args []string, defs ...*Def) (int, string, string) {
		var o, e bytes.Buffer
		c := run(args, defs, &o, &e)
		return c, o.String(), e.String()
	}
	// validDef's Map is Parallel(2): the lint passes and prints the note.
	if c, o, _ := call([]string{"lint"}, validDef()); c != 0 || !strings.HasPrefix(o, "lint valid: ok\n  note map-parallel-review valid/wFan: Parallel(2) caps this Map") || strings.Count(o, "\n") != 2 {
		t.Errorf("lint valid = %d %q", c, o)
	}
	nd := New("noted")
	nfan := NewMap[wFan](nd, FromObservation(Observe[[]wItem]("bd", "ready", nil)), func(it wItem) string { return it.ID }, itemChild())
	nend := NewTerminal[wFin](nd, Success)
	nfan.Parallel(3).Join(AllOK(), nfan.To(nend, "ok"), nfan.To(nend, "partial"))
	wantNote := "lint noted: ok\n  note map-parallel-review noted/wFan: Parallel(3) caps this Map; keep it only as a semantic limit (one merge at a time, a serial hot file): throughput is the n the coordinator passes to wf next (path: wFan)\n"
	if c, o, _ := call([]string{"lint"}, nd); c != 0 || o != wantNote {
		t.Errorf("lint noted = %d %q, want %q", c, o, wantNote)
	}
	c, _, e := call([]string{"lint"}, validDef(), invalidDef())
	if c != 2 || !strings.Contains(e, string(RuleUnreachable)) || !strings.Contains(e, string(RuleSubExitUnmapped)) {
		t.Errorf("lint invalid = %d %q", c, e)
	}
	if c, o, _ := call([]string{"hash"}, validDef()); c != 0 || !strings.HasPrefix(o, "valid sha256:") {
		t.Errorf("hash = %d %q", c, o)
	}
	if c, o, _ := call([]string{"mermaid", "valid"}, invalidDef(), validDef()); c != 0 || !strings.HasPrefix(o, "stateDiagram-v2") {
		t.Errorf("mermaid by name = %d %q", c, o)
	}
	if c, o, _ := call(nil, validDef()); c != 0 || !strings.HasPrefix(o, "valid v2") {
		t.Errorf("default graph = %d %q", c, o)
	}
	if c, _, e := call([]string{"graph", "nope"}, validDef()); c != 2 || e != "unknown definition nope\n" {
		t.Errorf("unknown definition = %d %q", c, e)
	}
	var o, eb bytes.Buffer
	if c := CLI(MainConfig{Defs: []Registration{{Def: validDef()}}}, []string{"frobnicate"}, &o, &eb); c != 2 || !strings.Contains(eb.String(), "frobnicate: unknown subcommand") {
		t.Errorf("unknown command = %d %q", c, eb.String())
	}
	if c, _, _ := call([]string{"graph"}, invalidDef()); c != 2 {
		t.Errorf("graph on invalid def = %d, want 2 (lint first)", c)
	}
}

func TestOwnArtifactIsAValidPromptInput(t *testing.T) {
	d := New("own")
	a := NewAgent[wWrite, string](d)
	end := NewTerminal[wFin](d, Success)
	d.OnFailure(Blocked, end)
	spec := NewArtifact[string](a, "spec", "spec.md")
	out := a.To(end, "done")
	a.Spec(PromptFile("p.md", spec), Retry(Const(1), out))
	a.Route(func(string, Ctx) Edge[wWrite] { return out }, out)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("problems: %v", ps)
	}
	// and an artifact produced only downstream is still unresolved
	d2 := New("late")
	b := NewAgent[wWrite, string](d2)
	c := NewAgent[wRun, string](d2)
	e2 := NewTerminal[wFin](d2, Success)
	late := NewArtifact[string](c, "late", "late.md")
	bc, ce := b.To(c, "next"), c.To(e2, "done")
	b.Spec(PromptFile("p.md", late), Retry(Const(1), bc))
	b.Route(func(string, Ctx) Edge[wWrite] { return bc }, bc)
	c.Spec(PromptText("c"), Retry(Const(1), ce))
	c.Route(func(string, Ctx) Edge[wRun] { return ce }, ce)
	if _, ok := rulesOf(d2.Lint())[RuleInputUnresolved]; !ok {
		t.Fatal("downstream artifact input not flagged")
	}
}
