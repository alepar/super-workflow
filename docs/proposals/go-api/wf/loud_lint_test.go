package wf_test

import (
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type LoudEnum string
type LoudResult struct{ Verdict LoudEnum }

func TestPromptOutDoesNotSilentlyBindRunParamOrPosition(t *testing.T) {
	for _, text := range []string{"review {{topic}}", "review {{misspelled}}"} {
		t.Run(text, func(t *testing.T) {
			d, a, end, _ := okDef("ambiguous")
			d.Param("topic")
			b := wf.NewAgent[rB, string](d)
			toB := a.To(b, "next")
			a.Route(func(string, wf.Ctx) wf.Edge[rA] { return toB }, toB)
			b.Spec(wf.PromptText(text, wf.Out(a)), wf.Retry[rB](wf.Const(1), b.To(end, "fail")))
			done := b.To(end, "done")
			b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
			found := false
			for _, p := range d.Lint() {
				if p.Rule == wf.RuleInputUnresolved && strings.Contains(p.String(), "wf.As") {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing explicit-binding diagnostic: %+v", d.Lint())
			}
		})
	}
}

func TestForeignChildArtifactDiagnosticNamesRef(t *testing.T) {
	parent, a, end, _ := okDef("parent")
	artifact := wf.NewArtifact[string](a, "report.md")
	child, b, childEnd, _ := okDef("child")
	wf.Consumes(b, artifact)
	wf.Yield(childEnd, func(wf.Ctx) rRes { return rRes{} })
	// The same invalid reference is a runtime failure when this child is mapped.
	m := wf.NewMap[rMap, rItem, string, rRes](parent, wf.FromValues(rItem{ID: "one"}), func(i rItem) string { return i.ID }, wf.Typed[rItem, rRes](child))
	toM := a.To(m, "map")
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return toM }, toM)
	m.Parallel(1).Join(wf.AllSettled(), m.To(end, "done"), m.To(end, "partial"))
	for _, p := range parent.Lint() {
		if p.Rule == wf.RuleArtifactNotUpstream && strings.Contains(p.String(), "wf.Ref") {
			return
		}
	}
	t.Fatalf("missing cross-definition artifact fix: %+v", parent.Lint())
}

func TestFreeRouteEnumRequiresCheck(t *testing.T) {
	d := wf.New("enum-route")
	wf.NewEnum(d, "verdict", LoudEnum("yes"), LoudEnum("no"))
	a := wf.NewAgent[rA, LoudResult](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	done := a.To(end, "done")
	a.Spec(wf.PromptText("go"), wf.Retry[rA](wf.Const(1), done))
	a.Route(func(LoudResult, wf.Ctx) wf.Edge[rA] { return done }, done)
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleBuild && strings.Contains(p.String(), "enum") {
			return
		}
	}
	t.Fatalf("unchecked enum Route linted clean: %+v", d.Lint())
}

// Invalid registered values must be refused before either a free route or its checks runs.
func TestFreeRouteRejectsUnknownEnumOnDecode(t *testing.T) {
	d := wf.New("enum-decode")
	wf.NewEnum(d, "verdict", LoudEnum("yes"), LoudEnum("no"))
	a := wf.NewAgent[rA, LoudResult](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	calls := 0
	done := a.To(end, "done")
	a.Spec(wf.PromptText("go"), wf.Retry[rA](wf.Const(2), done), wf.Checkf("check", func(LoudResult, wf.Ctx) error { calls++; return nil }))
	a.Route(func(LoudResult, wf.Ctx) wf.Edge[rA] { calls++; return done }, done)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	result := h.submit(run, h.next(run)[0], `{"Verdict":"maybe"}`)
	if result.Class != wf.Refused || calls != 0 || !strings.Contains(strings.Join(result.Refusal, " "), "Verdict") {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
}

func TestOutBindingLintReadsFileAndPromptForCandidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.json")
	if err := os.WriteFile(path, []byte(`{"body":"review {{topic}}"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ref := path + "#/body"
	for _, dynamic := range []bool{false, true} {
		t.Run(fmt.Sprint(dynamic), func(t *testing.T) {
			d, a, end, _ := okDef("file-prompt")
			d.Param("topic")
			b := wf.NewAgent[rB, string](d)
			edge := a.To(b, "next")
			a.Route(func(string, wf.Ctx) wf.Edge[rA] { return edge }, edge)
			prompt := wf.PromptFile(ref, wf.Out(a))
			if dynamic {
				prompt = wf.PromptFor(func(rItem) string { return ref }, []string{ref}, wf.Out(a))
				wf.Typed[rItem, rRes](d)
				wf.Yield(end, func(wf.Ctx) rRes { return rRes{} })
			}
			done := b.To(end, "done")
			b.Spec(prompt, wf.Retry[rB](wf.Const(1), done))
			b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
			for _, p := range d.Lint() {
				if p.Rule == wf.RuleInputUnresolved && strings.Contains(p.String(), "wf.As") {
					return
				}
			}
			t.Fatalf("ambiguous file prompt linted clean: %v", d.Lint())
		})
	}
}

func TestOutBindingLintAllowsEngineBuiltins(t *testing.T) {
	d, a, end, _ := okDef("builtins")
	d.OnFailure(wf.Blocked, end)
	b := wf.NewAgent[rB, string](d)
	edge := a.To(b, "next")
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return edge }, edge)
	done := b.To(end, "done")
	b.Spec(wf.PromptText("{{run}} {{harness}} {{resolvedModels}} {{blockedCause}}", wf.Out(a)), wf.Retry[rB](wf.Const(1), done))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("valid builtin bindings rejected: %v", ps)
	}
}

type ScopeParentOutput struct {
	Summary string `json:"summary"`
}

func TestOutBindingLintPreservesAncestorDocumentFields(t *testing.T) {
	child, a, end, _ := okDef("scope-child")
	child.OnFailure(wf.Blocked, end)
	b := wf.NewAgent[rB, string](child)
	next := a.To(b, "next")
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return next }, next)
	done := b.To(end, "done")
	b.Spec(wf.PromptText("review {{summary}}", wf.Out(a)), wf.Retry[rB](wf.Const(1), done))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
	wf.Yield(end, func(wf.Ctx) rRes { return rRes{} })
	d := wf.New("scope-parent")
	parent := wf.NewAgent[rA, ScopeParentOutput](d)
	stop := wf.NewTerminal[rEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, stop)
	m := wf.NewMap[rMap, rItem, string, rRes](d, wf.FromValues(rItem{ID: "one"}), func(i rItem) string { return i.ID }, wf.Typed[rItem, rRes](child))
	m.Parallel(1).Join(wf.AllSettled(), m.To(stop, "met"), m.To(stop, "partial"))
	toMap := parent.To(m, "children")
	parent.Spec(wf.PromptText("go"), wf.Retry[rA](wf.Const(1), parent.To(stop, "failed")))
	parent.Route(func(ScopeParentOutput, wf.Ctx) wf.Edge[rA] { return toMap }, toMap)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("valid ancestor document binding rejected: %v", ps)
	}
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{"summary":"parent document"}`)
	h.submit(run, h.next(run)[0], `"child output"`)
	if ins := h.next(run); len(ins) != 1 || !strings.Contains(ins[0].Prompt, "review parent document") {
		t.Fatalf("ancestor binding=%+v", ins)
	}
}

func TestFreeRouteWithCasesAlreadyChecksEnum(t *testing.T) {
	d := wf.New("combined-route")
	values := wf.NewEnum(d, "verdict", LoudEnum("yes"), LoudEnum("no"))
	a := wf.NewAgent[rA, LoudResult](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	done := a.To(end, "done")
	a.Spec(wf.PromptText("go"), wf.Retry[rA](wf.Const(2), done))
	a.RouteCases(wf.Cases[rA, LoudResult](func(o LoudResult) LoudEnum { return o.Verdict }, values, wf.On(LoudEnum("yes"), done), wf.On(LoudEnum("no"), done)))
	calls := 0
	a.Route(func(LoudResult, wf.Ctx) wf.Edge[rA] { calls++; return done }, done)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("existing declared enum check rejected: %v", ps)
	}
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	res := h.submit(run, h.next(run)[0], `{"Verdict":"maybe"}`)
	if res.Class != wf.Refused || calls != 0 {
		t.Fatalf("enum table did not guard free route: %+v calls=%d", res, calls)
	}
}

type FutureResultState struct{}

func TestFutureOutputDoesNotMaskPositionalOutLint(t *testing.T) {
	d, a, end, _ := okDef("future-output")
	b := wf.NewAgent[rB, string](d)
	future := wf.NewAgent[FutureResultState, ScopeParentOutput](d)
	toB := a.To(b, "next")
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return toB }, toB)
	toFuture := b.To(future, "later")
	b.Spec(wf.PromptText("review {{summary}}", wf.Out(a)), wf.Retry[rB](wf.Const(1), b.To(end, "fail")))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return toFuture }, toFuture)
	done := future.To(end, "done")
	future.Spec(wf.PromptText("produce summary later"), wf.Retry[FutureResultState](wf.Const(1), done))
	future.Route(func(ScopeParentOutput, wf.Ctx) wf.Edge[FutureResultState] { return done }, done)
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleInputUnresolved && strings.Contains(p.String(), "positional") {
			return
		}
	}
	t.Fatalf("future output hid actual positional fallback: %v", d.Lint())
}

func TestFailureRouteDoesNotPromiseAnAcceptedOutput(t *testing.T) {
	d := wf.New("failed-output")
	a := wf.NewAgent[rA, ScopeParentOutput](d)
	b := wf.NewAgent[rB, string](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	toB := a.To(b, "normal")
	blocked := a.To(b, "blocked")
	a.Spec(wf.PromptText("go"), wf.Retry[rA](wf.Const(1), a.To(end, "failed")).OnClass(wf.Blocked, blocked))
	a.Route(func(ScopeParentOutput, wf.Ctx) wf.Edge[rA] { return toB }, toB)
	done := b.To(end, "done")
	b.Spec(wf.PromptText("review {{summary}}", wf.Out(a)), wf.Retry[rB](wf.Const(1), done))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleInputUnresolved && strings.Contains(p.String(), "positional") {
			return
		}
	}
	t.Fatalf("failure route has no accepted Summary, but lint passed: %v", d.Lint())
}

type OptionalScopeOutput struct {
	Summary string `json:"summary,omitempty"`
}

func TestOptionalOutputFieldDoesNotPromiseAScopeBinding(t *testing.T) {
	d := wf.New("optional-output")
	a := wf.NewAgent[rA, OptionalScopeOutput](d)
	b := wf.NewAgent[rB, string](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	toB := a.To(b, "next")
	a.Spec(wf.PromptText("go"), wf.Retry[rA](wf.Const(1), a.To(end, "fail")))
	a.Route(func(OptionalScopeOutput, wf.Ctx) wf.Edge[rA] { return toB }, toB)
	done := b.To(end, "done")
	b.Spec(wf.PromptText("review {{summary}}", wf.Out(a)), wf.Retry[rB](wf.Const(1), done))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleInputUnresolved && strings.Contains(p.String(), "positional") {
			return
		}
	}
	t.Fatalf("optional field can be absent, but lint passed: %v", d.Lint())
}

func sharedChildScopeDef(allSafe bool) *wf.Def {
	child, a, end, _ := okDef("shared-scope")
	b := wf.NewAgent[rB, string](child)
	next := a.To(b, "next")
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return next }, next)
	done := b.To(end, "done")
	b.Spec(wf.PromptText("review {{summary}}", wf.Out(a)), wf.Retry[rB](wf.Const(1), done))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
	wf.Yield(end, func(wf.Ctx) rRes { return rRes{} })
	typed := wf.Typed[rItem, rRes](child)
	d := wf.New("shared-parent")
	choose := wf.NewAgent[rA, string](d)
	producer := wf.NewAgent[FutureResultState, ScopeParentOutput](d)
	stop := wf.NewTerminal[rEnd](d, wf.Success)
	m1 := wf.NewMap[rMap, rItem, string, rRes](d, wf.FromValues(rItem{ID: "one"}), func(i rItem) string { return i.ID }, typed)
	m2 := wf.NewMap[rSub, rItem, string, rRes](d, wf.FromValues(rItem{ID: "two"}), func(i rItem) string { return i.ID }, typed)
	m1.Parallel(1).Join(wf.AllSettled(), m1.To(stop, "met"), m1.To(stop, "partial"))
	m2.Parallel(1).Join(wf.AllSettled(), m2.To(stop, "met"), m2.To(stop, "partial"))
	toProducer := choose.To(producer, "produce")
	choose.Spec(wf.PromptText("choose"), wf.Retry[rA](wf.Const(1), choose.To(stop, "failed")))
	if allSafe {
		choose.Route(func(string, wf.Ctx) wf.Edge[rA] { return toProducer }, toProducer)
	} else {
		unsafe := choose.To(m2, "without summary")
		choose.Route(func(string, wf.Ctx) wf.Edge[rA] { return unsafe }, toProducer, unsafe)
	}
	safe := producer.To(m1, "first map")
	producer.Spec(wf.PromptText("summary"), wf.Retry[FutureResultState](wf.Const(1), producer.To(stop, "failed")))
	if allSafe {
		second := producer.To(m2, "second map")
		producer.Route(func(ScopeParentOutput, wf.Ctx) wf.Edge[FutureResultState] { return second }, safe, second)
	} else {
		producer.Route(func(ScopeParentOutput, wf.Ctx) wf.Edge[FutureResultState] { return safe }, safe)
	}
	return d
}

func TestSharedChildRequiresBindingAtEveryMapEntry(t *testing.T) {
	for _, safe := range []bool{false, true} {
		t.Run(fmt.Sprint(safe), func(t *testing.T) {
			ps := sharedChildScopeDef(safe).Lint()
			found := false
			for _, p := range ps {
				found = found || p.Rule == wf.RuleInputUnresolved && strings.Contains(p.String(), "positional")
			}
			if found == safe {
				t.Fatalf("safe=%v problems=%v", safe, ps)
			}
		})
	}
}

func TestSharedChildIsLintedInEachParentContext(t *testing.T) {
	child, a, end, _ := okDef("context-child")
	b := wf.NewAgent[rB, string](child)
	next := a.To(b, "next")
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return next }, next)
	done := b.To(end, "done")
	b.Spec(wf.PromptText("review {{summary}}", wf.Out(a)), wf.Retry[rB](wf.Const(1), done))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
	wf.Yield(end, func(wf.Ctx) rRes { return rRes{} })
	shared := wf.Typed[rItem, rRes](child)
	parent := func(name string, produces bool) wf.Child[rItem, rRes] {
		d := wf.New(name)
		var p *wf.Agent[rA, ScopeParentOutput]
		if produces {
			p = wf.NewAgent[rA, ScopeParentOutput](d)
		}
		m := wf.NewMap[rMap, rItem, string, rRes](d, wf.FromValues(rItem{ID: "one"}), func(i rItem) string { return i.ID }, shared)
		stop := wf.NewTerminal[rEnd](d, wf.Success)
		wf.Yield(stop, func(wf.Ctx) rRes { return rRes{} })
		m.Parallel(1).Join(wf.AllSettled(), m.To(stop, "met"), m.To(stop, "partial"))
		if p != nil {
			edge := p.To(m, "children")
			p.Spec(wf.PromptText("summary"), wf.Retry[rA](wf.Const(1), p.To(stop, "failed")))
			p.Route(func(ScopeParentOutput, wf.Ctx) wf.Edge[rA] { return edge }, edge)
		}
		return wf.Typed[rItem, rRes](d)
	}
	d := wf.New("contexts")
	choose := wf.NewAgent[rA, string](d)
	m1 := wf.NewMap[rMap, rItem, string, rRes](d, wf.FromValues(rItem{ID: "one"}), func(i rItem) string { return i.ID }, parent("safe-parent", true))
	m2 := wf.NewMap[rSub, rItem, string, rRes](d, wf.FromValues(rItem{ID: "two"}), func(i rItem) string { return i.ID }, parent("unsafe-parent", false))
	stop := wf.NewTerminal[rEnd](d, wf.Success)
	m1.Parallel(1).Join(wf.AllSettled(), m1.To(stop, "met"), m1.To(stop, "partial"))
	m2.Parallel(1).Join(wf.AllSettled(), m2.To(stop, "met"), m2.To(stop, "partial"))
	first, second := choose.To(m1, "safe"), choose.To(m2, "unsafe")
	choose.Spec(wf.PromptText("choose"), wf.Retry[rA](wf.Const(1), choose.To(stop, "failed")))
	choose.Route(func(string, wf.Ctx) wf.Edge[rA] { return second }, first, second)
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleInputUnresolved && strings.Contains(p.Path, "unsafe-parent") {
			return
		}
	}
	t.Fatalf("second parent context skipped: %v", d.Lint())
}
