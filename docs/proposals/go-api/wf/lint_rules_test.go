package wf_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Marker types for the per-rule fixtures.
type (
	rA      struct{}
	rB      struct{}
	rEnd    struct{}
	rOrphan struct{}
	rGate   struct{}
	rSub    struct{}
	rMap    struct{}
	rChild  struct{}
	rItem   struct{ ID string }
	rRes    struct{}
	rOpts   struct{ On bool }
	rCmd    struct{}
)

// allRules lists every RuleID constant in wf/types.go; TestEveryRuleIDHasACase
// fails when a constant is added without extending it and the table below.
var allRules = []wf.RuleID{
	wf.RuleBuild, wf.RuleDispatchHint, wf.RuleNoStates, wf.RuleUnrouted, wf.RuleNoEdges, wf.RuleUnreachable,
	wf.RuleNoTerminal, wf.RuleForeignEdge, wf.RuleDuplicateName, wf.RuleBudgetUnwired,
	wf.RuleArtifactNotUpstream, wf.RuleArtifactType, wf.RuleEffectKey, wf.RuleReservedPolicy,
	wf.RuleInputUnresolved, wf.RuleAgentRetry, wf.RuleGateAnswers, wf.RuleCaseUncovered,
	wf.RuleCaseUnknownValue, wf.RuleCaseNoOtherwise, wf.RuleCaseDuplicate, wf.RuleMapItemKey, wf.RuleMapJoin,
	wf.RuleMapPartialFailure, wf.RuleMapWriteOverlap, wf.RuleSubExitUnmapped,
	wf.RuleSubResultType, wf.RuleSubRecursive, wf.RuleChildNameDuplicate, wf.RuleChildNotExported,
	wf.RuleHoldEffectOutside, wf.RuleComputeUnavailable, wf.RuleRouteNoCandidates,
	wf.RuleCounterScope, wf.RuleCounterScopeDef, wf.RulePromptCandidates, wf.RuleMapParallelReview, wf.RuleArtifactName, wf.RuleArtifactTemplated, wf.RuleCommandKey, wf.RuleLiteralModel, wf.RuleAgentSelfEdge, wf.RuleSendBackTarget, wf.RuleSendBackFallback, wf.RuleSendBackUnread,
	wf.RuleAgentBlockedRoute, wf.RuleAgentOutputPath, wf.RuleMaxlenTag, wf.RuleFetchCoverage,
}

// okDef is a valid agent -> terminal definition; fixtures break one thing in it.
func okDef(name string) (d *wf.Def, a *wf.Agent[rA, string], end *wf.Terminal[rEnd], done wf.Edge[rA]) {
	d = wf.New(name)
	a = wf.NewAgent[rA, string](d)
	end = wf.NewTerminal[rEnd](d, wf.Success)
	done = a.To(end, "done")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), done).OnClass(wf.Blocked, a.To(end, "blocked")))
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return done }, done)
	return
}

// childOf returns a typed child definition holding a single terminal.
func childOf(name string) wf.Child[rItem, rRes] {
	d := wf.New(name)
	end := wf.NewTerminal[rChild](d, wf.Success)
	wf.Yield(end, func(wf.Ctx) rRes { return rRes{} })
	return wf.Typed[rItem, rRes](d)
}

func strField(s string) string { return s }

type (
	rJudge struct{}
	rStop  struct{}
)

// sendBackRuleDef is a producer, a judge that sends back to it and a stop.
// toSelf aims the send-back at the judge, binds has the producer bind
// wf.Returned(), and reasoned gives the fallback its Ends.
func sendBackRuleDef(name string, toSelf, binds, reasoned bool) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[rA, string](d)
	j := wf.NewAgent[rJudge, string](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	stop := wf.NewTerminal[rStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	p := wf.PromptText("p")
	if binds {
		p = wf.PromptText("p {{r}}", wf.As("r", wf.Returned()))
	}
	a.Spec(p, wf.Retry(wf.Const(1), a.To(stop, "a gave up")))
	a.Then(a.To(j, "made"))
	j.Spec(wf.PromptText("judge"), wf.Retry(wf.Const(1), j.To(stop, "j gave up")))
	fallback := j.To(stop, "unfixed")
	if reasoned {
		fallback = fallback.Ends(wf.Stopped, "the work was not fixed")
	}
	var producer wf.State = a
	if toSelf {
		producer = j
	}
	back := j.SendBack(producer, "back", func(s string) []string { return []string{s} }, wf.Const(1), fallback)
	ok := j.To(end, "ok")
	j.Route(func(s string, _ wf.Ctx) wf.Edge[rJudge] {
		if s == "" {
			return ok
		}
		return back
	}, ok, back)
	return d
}

var ruleCases = []struct {
	rule wf.RuleID
	def  func() *wf.Def
}{
	{wf.RuleAgentBlockedRoute, func() *wf.Def {
		d := wf.New("no-blocked-route")
		a := wf.NewAgent[rA, string](d)
		end := wf.NewTerminal[rEnd](d, wf.Success)
		done := a.To(end, "done")
		a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), done))
		a.Route(func(string, wf.Ctx) wf.Edge[rA] { return done }, done)
		return d
	}},
	{wf.RuleLiteralModel, func() *wf.Def {
		d, a, _, _ := okDef("literal-model")
		a.Dispatch(wf.Model("opus"))
		return d
	}},
	{wf.RuleAgentSelfEdge, func() *wf.Def {
		d := wf.New("self-edge")
		a := wf.NewAgent[rA, string](d)
		end := wf.NewTerminal[rEnd](d, wf.Success)
		done := a.To(end, "done")
		again := a.To(a, "again")
		a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), done))
		a.Route(func(string, wf.Ctx) wf.Edge[rA] { return again }, done, again)
		return d
	}},
	{wf.RuleSendBackTarget, func() *wf.Def { return sendBackRuleDef("send-back-target", true, true, true) }},
	{wf.RuleSendBackFallback, func() *wf.Def { return sendBackRuleDef("send-back-fallback", false, true, false) }},
	{wf.RuleSendBackUnread, func() *wf.Def { return sendBackRuleDef("send-back-unread", false, false, true) }},
	{wf.RuleDispatchHint, func() *wf.Def {
		d, a, _, _ := okDef("dispatch-hint")
		a.Dispatch(wf.Effort("huge"))
		return d
	}},
	{wf.RuleBuild, func() *wf.Def {
		d, a, _, _ := okDef("build")
		a.To(nil, "nowhere")
		return d
	}},
	{wf.RuleRouteNoCandidates, func() *wf.Def {
		d, a, _, _ := okDef("nocand")
		a.Route(func(string, wf.Ctx) wf.Edge[rA] { return wf.Edge[rA]{} })
		return d
	}},
	{wf.RulePromptCandidates, func() *wf.Def {
		d, a, _, done := okDef("prompt-candidates")
		a.Spec(wf.PromptFor(func(rItem) string { return "" }, nil), wf.Retry(wf.Const(1), done))
		return d
	}},
	{wf.RuleCounterScope, func() *wf.Def {
		d, _ := scopedDef(true, false, true)
		return d
	}},
	{wf.RuleCounterScopeDef, func() *wf.Def {
		p := wf.New("scopeowner")
		round := p.Counter("round")
		d := wf.New("scopedef")
		x := d.Counter("x", wf.ScopedTo(round))
		a := wf.NewCommand[rA](d, "x")
		end := wf.NewTerminal[rEnd](d, wf.Success)
		e := a.To(end, "go").With(x.Inc(), round.Inc())
		a.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rA] { return e }, e)
		return d
	}},
	{wf.RuleNoStates, func() *wf.Def { return wf.New("empty") }},
	{wf.RuleHoldEffectOutside, func() *wf.Def {
		// the same keyed write on an edge into a held state and on an edge
		// into an unheld state: it would run both inside and outside the lock.
		d := wf.New("holdoutside")
		lock := d.Lock("merge", wf.LockRunKey)
		a := wf.NewCommand[rA](d, "x")
		held := wf.NewCommand[rB](d, "y")
		wf.Hold(held, lock)
		free := wf.NewCommand[rItem](d, "z")
		end := wf.NewTerminal[rEnd](d, wf.Success)
		ef := wf.ExternalEffect{Adapter: "git", Op: "merge", Key: wf.KeyOf("m")}
		toHeld := a.To(held, "held").With(ef)
		toFree := a.To(free, "free").With(ef)
		a.RouteCases(wf.CasesOpen[rA, wf.RunResult, int](func(r wf.RunResult) int { return r.Exit },
			wf.On(0, toHeld),
		).Otherwise(toFree))
		heldDone, freeDone := held.To(end, "done"), free.To(end, "done")
		held.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rB] { return heldDone }, heldDone)
		free.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rItem] { return freeDone }, freeDone)
		return d
	}},
	{wf.RuleUnrouted, func() *wf.Def {
		d, a, _, _ := okDef("unrouted")
		b := wf.NewCommand[rB](d, "x")
		next := a.To(b, "next")
		a.Route(func(string, wf.Ctx) wf.Edge[rA] { return next }, next)
		return d
	}},
	{wf.RuleNoEdges, func() *wf.Def {
		d, _, _, _ := okDef("noedges")
		c := wf.NewCommand[rB](d, "x")
		c.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rB] { return wf.Edge[rB]{} })
		return d
	}},
	{wf.RuleUnreachable, func() *wf.Def {
		d, _, _, _ := okDef("unreachable")
		wf.NewTerminal[rOrphan](d, wf.Stopped)
		return d
	}},
	{wf.RuleNoTerminal, func() *wf.Def {
		d := wf.New("loop")
		a, b := wf.NewCommand[rA](d, "x"), wf.NewCommand[rB](d, "y")
		ab, ba := a.To(b, "ab"), b.To(a, "ba")
		a.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rA] { return ab }, ab)
		b.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rB] { return ba }, ba)
		return d
	}},
	{wf.RuleForeignEdge, func() *wf.Def {
		d, a, _, _ := okDef("foreign")
		other := wf.New("other")
		away := a.To(wf.NewTerminal[rOrphan](other, wf.Stopped), "away")
		a.Route(func(string, wf.Ctx) wf.Edge[rA] { return away }, away)
		return d
	}},
	{wf.RuleDuplicateName, func() *wf.Def {
		d, _, _, _ := okDef("dup")
		wf.NewTerminal[rEnd](d, wf.Stopped)
		return d
	}},
	{wf.RuleBudgetUnwired, func() *wf.Def {
		d, a, _, done := okDef("budget")
		// declared-data routing, and nothing increments the counter
		a.RouteCases(wf.Cases(strField, wf.NewEnum(d, "v", "ok"), wf.On("ok", done)))
		wf.MaxVisits(wf.Const(2).Plus(wf.Count(d.Counter("never"))), done)
		return d
	}},
	{wf.RuleArtifactNotUpstream, func() *wf.Def {
		d, a, end, _ := okDef("artifact-upstream")
		wf.Consumes(a, wf.NewArtifact[string](end, "late", "late.md"))
		return d
	}},
	{wf.RuleAgentOutputPath, func() *wf.Def { return outputPathDef[wf.RunPath]("") }},
	{wf.RuleMaxlenTag, func() *wf.Def {
		type capped struct {
			N int `json:"n" maxlen:"3"`
		}
		d := wf.New("maxlen-tag")
		a := wf.NewAgent[rA, capped](d)
		end := wf.NewTerminal[rEnd](d, wf.Success)
		done := a.To(end, "done")
		a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), done).OnClass(wf.Blocked, a.To(end, "blocked")))
		a.Route(func(capped, wf.Ctx) wf.Edge[rA] { return done }, done)
		return d
	}},
	{wf.RuleArtifactName, func() *wf.Def {
		d, a, _, _ := okDef("artifact-name")
		wf.NewArtifact[string](a, "sub/x.md")
		return d
	}},
	{wf.RuleArtifactTemplated, func() *wf.Def {
		d, a, _, _ := okDef("artifact-templated")
		wf.NewArtifact[string](a, "x", "{run}/x.md")
		return d
	}},
	{wf.RuleArtifactType, func() *wf.Def {
		d, a, _, _ := okDef("artifact-type")
		wf.NewArtifact[string](a, "x", "x.md")
		wf.NewArtifact[int](a, "x", "x.md")
		return d
	}},
	{wf.RuleEffectKey, func() *wf.Def {
		d, a, end, _ := okDef("effect-key")
		enum := wf.NewEnum(d, "v", "ok")
		keyless := a.To(end, "create").With(wf.ExternalEffect{Adapter: "bd", Op: "create"})
		a.RouteCases(wf.Cases(strField, enum, wf.On("ok", keyless)))
		return d
	}},
	{wf.RuleCommandKey, func() *wf.Def {
		d, a, end, _ := okDef("command-key")
		enum := wf.NewEnum(d, "v", "ok")
		cmd := wf.NewCommand[rCmd](d, "make")
		cmd.Idempotent() // no Key
		cmd.RouteCases(wf.Cases(func(r wf.RunResult) string { return "ok" }, enum, wf.On("ok", cmd.To(end, "ok"))))
		a.RouteCases(wf.Cases(strField, enum, wf.On("ok", a.To(cmd, "build"))))
		return d
	}},
	{wf.RuleReservedPolicy, func() *wf.Def {
		d, a, end, _ := okDef("reserved")
		opts := wf.Options[rOpts](d)
		answers := wf.NewEnum(d, "answer", "go")
		g := wf.NewGate[rGate](d, "Sure?", answers, wf.Reserved)
		g.Answers(wf.On("go", g.To(end, "go")))
		ask := a.To(g, "ask")
		a.Route(func(string, wf.Ctx) wf.Edge[rA] { return ask }, ask)
		d.SetPolicy(wf.Policy(wf.PolicyAnswer(g, "go", wf.Field(opts, func(o *rOpts) *bool { return &o.On }).Is(true))))
		return d
	}},
	{wf.RuleInputUnresolved, func() *wf.Def {
		d, a, _, done := okDef("input")
		a.Spec(wf.PromptText("p {{item}}", wf.ChildInput()), wf.Retry(wf.Const(1), done))
		return d
	}},
	{wf.RuleAgentRetry, func() *wf.Def {
		d, _, end, _ := okDef("agent-retry")
		b := wf.NewAgent[rB, string](d)
		to := b.To(end, "done")
		b.Route(func(string, wf.Ctx) wf.Edge[rB] { return to }, to)
		return d
	}},
	{wf.RuleGateAnswers, func() *wf.Def {
		d, _, end, _ := okDef("gate")
		g := wf.NewGate[rGate](d, "Go?", wf.NewEnum(d, "answer", "go"), wf.PolicyAnswerable)
		g.To(end, "go")
		return d
	}},
	{wf.RuleCaseUncovered, func() *wf.Def {
		d, a, _, done := okDef("uncovered")
		a.RouteCases(wf.Cases(strField, wf.NewEnum(d, "v", "x", "y"), wf.On("x", done)))
		return d
	}},
	{wf.RuleCaseUnknownValue, func() *wf.Def {
		d, a, _, done := okDef("unknown")
		a.RouteCases(wf.Cases(strField, wf.NewEnum(d, "v", "x"), wf.On("x", done), wf.On("zzz", done)))
		return d
	}},
	{wf.RuleCaseNoOtherwise, func() *wf.Def {
		d, a, _, done := okDef("otherwise")
		a.RouteCases(wf.CasesOpen(strField, wf.On("x", done)))
		return d
	}},
	{wf.RuleCaseDuplicate, func() *wf.Def {
		d, a, _, done := okDef("duplicate")
		a.RouteCases(wf.Cases(strField, wf.NewEnum(d, "v", "x"), wf.On("x", done), wf.On("x", done)))
		return d
	}},
	{wf.RuleMapItemKey, func() *wf.Def {
		d, _, _, _ := okDef("mapkey")
		wf.NewMap[rMap, rItem, string, rRes](d, wf.FromObservation(wf.Observe[[]rItem]("bd", "ready", nil)), nil, childOf("kid"))
		return d
	}},
	{wf.RuleMapJoin, func() *wf.Def {
		d, _, _, _ := okDef("mapjoin")
		wf.NewMap[rMap](d, wf.FromObservation(wf.Observe[[]rItem]("bd", "ready", nil)), func(i rItem) string { return i.ID }, childOf("kid"))
		return d
	}},
	{wf.RuleMapPartialFailure, func() *wf.Def {
		d, _, end, _ := okDef("mappartial")
		m := wf.NewMap[rMap](d, wf.FromObservation(wf.Observe[[]rItem]("bd", "ready", nil)), func(i rItem) string { return i.ID }, childOf("kid"))
		both := m.To(end, "both")
		m.Join(wf.AllOK(), both, both)
		return d
	}},
	{wf.RuleMapWriteOverlap, func() *wf.Def {
		d, _, end, _ := okDef("mapwrites")
		m := wf.NewMap[rMap](d, wf.FromObservation(wf.Observe[[]rItem]("bd", "ready", nil)), func(i rItem) string { return i.ID }, childOf("kid"))
		m.Parallel(2).Writes("docs/**").Join(wf.AllOK(), m.To(end, "ok"), m.To(end, "partial"))
		return d
	}},
	{wf.RuleMapParallelReview, func() *wf.Def { return parallelMapDef("mapparallel", 2) }},
	{wf.RuleFetchCoverage, func() *wf.Def {
		d, a, _, _ := okDef("fetchcoverage")
		a.ReadsFetchCoverage()
		return d
	}},
	{wf.RuleSubExitUnmapped, func() *wf.Def {
		d, _, _, _ := okDef("subexit")
		s := wf.NewSub[rSub](d, childOf("kid"), func(wf.Ctx) rItem { return rItem{} })
		s.Exits()
		return d
	}},
	{wf.RuleSubResultType, func() *wf.Def {
		d, _, end, _ := okDef("subresult")
		wf.Yield(end, func(wf.Ctx) rRes { return rRes{} })
		return d
	}},
	{wf.RuleSubRecursive, func() *wf.Def {
		d := wf.New("recursive")
		wf.NewSub[rSub](d, wf.Typed[rItem, rRes](d), func(wf.Ctx) rItem { return rItem{} })
		return d
	}},
	{wf.RuleChildNameDuplicate, func() *wf.Def {
		d, _, end, _ := okDef("dupchild")
		in := func(wf.Ctx) rItem { return rItem{} }
		a := wf.NewSub[rSub](d, childOf("kid"), in)
		other := childOf("kid")
		other.Def().Version(2) // differs from the Sub's "kid"
		b := wf.NewMap[rMap](d, wf.FromObservation(wf.Observe[[]rItem]("bd", "ready", nil)), func(i rItem) string { return i.ID }, other)
		a.Exits(wf.OnExit[rChild](a.To(b, "sub done")))
		b.Join(wf.AllOK(), b.To(end, "ok"), b.To(end, "partial"))
		return d
	}},
	{wf.RuleChildNotExported, func() *wf.Def {
		d, _, _, _ := okDef("notexported")
		kid := wf.New("kid")
		ka := wf.NewAgent[rB, string](kid)
		art := wf.NewArtifact[string](ka, "report", "report.md")
		s := wf.NewSub[rSub](d, wf.Typed[rItem, rRes](kid), func(wf.Ctx) rItem { return rItem{} })
		wf.Consumes(s, wf.FromChild(s, art))
		return d
	}},
	{wf.RuleComputeUnavailable, func() *wf.Def {
		d, _, end, _ := okDef("compute-unavailable")
		c := wf.NewCompute[rB, int](d, func(wf.Ctx) (int, error) { return 0, nil })
		to := c.To(end, "done")
		c.Observes(wf.Observe[int]("bd", "show", map[string]any{"id": "{epic}"}))
		c.Route(func(int, wf.Ctx) wf.Edge[rB] { return to }, to)
		return d
	}},
}

// TestCaseDuplicateInGateAnswers: Gate.Answers goes through the same table, so
// a repeated answer is reported on the gate's path.
func TestCaseDuplicateInGateAnswers(t *testing.T) {
	d, _, end, _ := okDef("gate-dup")
	g := wf.NewGate[rGate](d, "Go?", wf.NewEnum(d, "answer", "go"), wf.PolicyAnswerable)
	g.Answers(wf.On("go", g.To(end, "first")), wf.On("go", g.To(end, "second")))
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleCaseDuplicate {
			if p.Path != "gate-dup/rGate" {
				t.Errorf("path = %q, want gate-dup/rGate", p.Path)
			}
			return
		}
	}
	t.Fatalf("case-duplicate did not fire; problems: %v", d.Lint())
}

// TestUnyieldedTypedTerminal: a typed child whose terminal does not Yield its
// result type fails lint at the terminal, while the yielding terminal does not.
func TestUnyieldedTypedTerminal(t *testing.T) {
	kid := wf.New("kid")
	okEnd := wf.NewTerminal[rChild](kid, wf.Success)
	wf.NewTerminal[rEnd](kid, wf.Stopped)
	wf.Yield(okEnd, func(wf.Ctx) rRes { return rRes{} })
	d, _, _, _ := okDef("parent")
	wf.NewSub[rSub](d, wf.Typed[rItem, rRes](kid), func(wf.Ctx) rItem { return rItem{} })
	var paths []string
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleSubResultType {
			paths = append(paths, p.Path)
		}
	}
	if len(paths) != 1 || paths[0] != "parent/kid/rEnd" {
		t.Fatalf("sub-result-type paths = %v, want [parent/kid/rEnd]", paths)
	}
}

func TestEachRuleFires(t *testing.T) {
	for _, tc := range ruleCases {
		t.Run(string(tc.rule), func(t *testing.T) {
			d := tc.def()
			ps := append(append(append(d.Lint(), d.LintServed()...), d.Notes()...), d.LintStaged()...)
			for _, p := range ps {
				if p.Rule == tc.rule {
					return
				}
			}
			t.Fatalf("rule %s did not fire; problems: %v", tc.rule, ps)
		})
	}
}

// A Detached command cannot take Stdin: the detached runner passes no input.
func TestCommandKeyRejectsDetachedStdin(t *testing.T) {
	d := detDef(func(c *wf.Command[dRun]) { c.Idempotent().Detached().Stdin("x") })
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleCommandKey && strings.Contains(p.Msg, "cannot take Stdin") {
			return
		}
	}
	t.Fatalf("no Detached+Stdin command-key problem; problems: %v", d.Lint())
}

// TestEveryRuleIDHasACase compares the RuleID constants declared in types.go
// with allRules and the fixture table, so a new rule without a test fails.
func TestEveryRuleIDHasACase(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "RuleID" {
			for _, name := range vs.Names {
				if strings.HasPrefix(name.Name, "Rule") {
					declared = append(declared, name.Name)
				}
			}
		}
		return true
	})
	if len(declared) != len(allRules) {
		t.Fatalf("types.go declares %d RuleID constants (%v), allRules lists %d", len(declared), declared, len(allRules))
	}
	have := map[wf.RuleID]bool{}
	for _, tc := range ruleCases {
		have[tc.rule] = true
	}
	seen := map[wf.RuleID]bool{}
	for _, r := range allRules {
		if seen[r] {
			t.Errorf("allRules lists %s twice", r)
		}
		seen[r] = true
		if !have[r] {
			t.Errorf("rule %s has no failing-definition case", r)
		}
	}
}

// parallelMapDef is a valid definition with one Map; parallel <= 0 leaves
// Parallel unset.
func parallelMapDef(name string, parallel int) *wf.Def {
	d := wf.New(name)
	m := wf.NewMap[rMap](d, wf.FromObservation(wf.Observe[[]rItem]("bd", "ready", nil)), func(i rItem) string { return i.ID }, childOf("kid"))
	end := wf.NewTerminal[rEnd](d, wf.Success)
	if parallel > 0 {
		m.Parallel(parallel)
	}
	m.Join(wf.AllOK(), m.To(end, "ok"), m.To(end, "partial"))
	return d
}

// TestNotesDoNotFailLint: a note prints but is not a lint failure.
func TestNotesDoNotFailLint(t *testing.T) {
	d := parallelMapDef("notes", 2)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("Lint = %v, want none", ps)
	}
	ns := d.Notes()
	if len(ns) != 1 || ns[0].Rule != wf.RuleMapParallelReview || ns[0].Severity != wf.SeverityNote {
		t.Fatalf("Notes = %v, want one map-parallel-review note", ns)
	}
	if !strings.HasPrefix(ns[0].String(), "note map-parallel-review notes/") {
		t.Errorf("String = %q", ns[0].String())
	}
	for _, n := range []int{0, 1} {
		if ns := parallelMapDef("quiet", n).Notes(); len(ns) != 0 {
			t.Errorf("Parallel(%d) notes = %v, want none", n, ns)
		}
	}
}

// holdEffectDef builds the RuleHoldEffectOutside fixture with per-edge effects.
func holdEffectDef(held, free wf.ExternalEffect) *wf.Def {
	d := wf.New("holdoutside")
	lock := d.Lock("merge", wf.LockRunKey)
	a := wf.NewCommand[rA](d, "x")
	h := wf.NewCommand[rB](d, "y")
	wf.Hold(h, lock)
	f := wf.NewCommand[rItem](d, "z")
	end := wf.NewTerminal[rEnd](d, wf.Success)
	toHeld := a.To(h, "held").With(held)
	toFree := a.To(f, "free").With(free)
	a.RouteCases(wf.CasesOpen[rA, wf.RunResult, int](func(r wf.RunResult) int { return r.Exit },
		wf.On(0, toHeld),
	).Otherwise(toFree))
	hDone := h.To(end, "done")
	fDone := f.To(end, "done")
	h.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rB] { return hDone }, hDone)
	f.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rItem] { return fDone }, fDone)
	return d
}

func firesHold(d *wf.Def) bool {
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleHoldEffectOutside {
			return true
		}
	}
	return false
}

func TestHoldEffectOutsideIgnoresArgs(t *testing.T) {
	d := holdEffectDef(
		wf.ExternalEffect{Adapter: "git", Op: "merge", Key: wf.KeyOf("m"), Args: map[string]any{"m": 1}},
		wf.ExternalEffect{Adapter: "git", Op: "merge", Key: wf.KeyOf("m"), Args: map[string]any{"m": 2}})
	if !firesHold(d) {
		t.Error("same adapter.op|key with differing args did not fire hold-effect-outside")
	}
}

func TestHoldEffectOutsideDistinctKeys(t *testing.T) {
	d := holdEffectDef(
		wf.ExternalEffect{Adapter: "git", Op: "merge", Key: wf.KeyOf("m1")},
		wf.ExternalEffect{Adapter: "git", Op: "merge", Key: wf.KeyOf("m2")})
	if firesHold(d) {
		t.Error("distinct keys fired hold-effect-outside")
	}
}

// The literal-model rule is served-only: Def.Lint accepts wf.Model, LintServed
// rejects it, in a child definition too, and a tiered definition passes both.
func TestLiteralModelIsServedOnly(t *testing.T) {
	has := func(ps []wf.Problem) bool {
		for _, p := range ps {
			if p.Rule == wf.RuleLiteralModel {
				return true
			}
		}
		return false
	}
	d, a, _, _ := okDef("lm")
	a.Dispatch(wf.Model("opus"))
	if has(d.Lint()) {
		t.Errorf("Def.Lint reports literal-model: %v", d.Lint())
	}
	if !has(d.LintServed()) {
		t.Errorf("LintServed misses literal-model: %v", d.LintServed())
	}
	tiered, ta, _, _ := okDef("tiered")
	ta.Dispatch(wf.ModelTier(wf.TierLight), wf.Effort("low"))
	if ps := tiered.LintServed(); len(ps) != 0 {
		t.Errorf("tiered LintServed = %v, want none", ps)
	}

	kid := wf.New("kid")
	ka := wf.NewAgent[rA, string](kid)
	kend := wf.NewTerminal[rEnd](kid, wf.Success)
	kdone := ka.To(kend, "done")
	ka.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), kdone))
	ka.Route(func(string, wf.Ctx) wf.Edge[rA] { return kdone }, kdone)
	ka.Dispatch(wf.Model("haiku"))
	wf.Yield(kend, func(wf.Ctx) rRes { return rRes{} })
	parent := wf.New("parent")
	wf.NewSub[rB](parent, wf.Typed[rItem, rRes](kid), func(wf.Ctx) rItem { return rItem{} })
	ps := parent.LintServed()
	if !has(ps) {
		t.Fatalf("child literal model not reported: %v", ps)
	}
	for _, p := range ps {
		if p.Rule == wf.RuleLiteralModel && !strings.HasPrefix(p.Path, "parent/kid/") {
			t.Errorf("path %q is not qualified through the parent", p.Path)
		}
	}
	if has(parent.Lint()) {
		t.Errorf("Def.Lint reports literal-model on the child")
	}
}

// The agent-self-edge rule is served-only: Def.Lint accepts an agent edge to
// itself (wf-package fixtures use them), LintServed rejects it and names the
// state, and a command self-route is unaffected.
func TestAgentSelfEdgeIsServedOnly(t *testing.T) {
	d := wf.New("self-edge")
	a := wf.NewAgent[rA, string](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	done := a.To(end, "done")
	again := a.To(a, "again")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), done))
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return again }, done, again)
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleAgentSelfEdge {
			t.Errorf("Def.Lint reports agent-self-edge: %v", p)
		}
	}
	var got []wf.Problem
	for _, p := range d.LintServed() {
		if p.Rule == wf.RuleAgentSelfEdge {
			got = append(got, p)
		}
	}
	if len(got) != 1 || got[0].Path != "self-edge/rA" {
		t.Fatalf("LintServed agent-self-edge problems = %v, want one at self-edge/rA", got)
	}

	c := wf.New("cmd-self")
	cmd := wf.NewCommand[rA](c, "true")
	cend := wf.NewTerminal[rEnd](c, wf.Success)
	again2, done2 := cmd.To(cmd, "again"), cmd.To(cend, "done")
	cmd.Route(func(wf.RunResult, wf.Ctx) wf.Edge[rA] { return again2 }, again2, done2)
	for _, p := range c.LintServed() {
		if p.Rule == wf.RuleAgentSelfEdge {
			t.Errorf("command self-edge reported: %v", p)
		}
	}
}
