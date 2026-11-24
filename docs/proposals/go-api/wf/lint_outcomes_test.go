package wf_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func hasOutcomeRule(d *wf.Def, rule wf.RuleID) bool {
	for _, p := range d.LintSound() {
		if p.Rule == rule {
			return true
		}
	}
	return false
}

func TestGateExpectedTerminalOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		actual   wf.Outcome
		contract bool
		want     wf.RuleID
	}{
		{"valid", wf.Stopped, true, ""},
		{"hold-to-shipped", wf.Success, true, wf.RuleEndsOutcomeMismatch},
		{"skip-menu-to-terminal", wf.Success, false, wf.RuleGateEndsMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := wf.New(tc.name)
			g := wf.NewGate[sdAsk, string](d, "Continue?", wf.NewEnum(d, "answer", "hold"), wf.Reserved)
			end := wf.NewTerminal[sdStop](d, tc.actual)
			e := g.To(end, "hold")
			if tc.contract {
				e = e.Ends(wf.Stopped, "the person retains the work")
			}
			g.Answers(wf.On("hold", e))
			if tc.want == "" {
				if hasOutcomeRule(d, wf.RuleEndsOutcomeMismatch) || hasOutcomeRule(d, wf.RuleGateEndsMissing) {
					t.Fatalf("valid contract rejected: %v", d.LintSound())
				}
			} else if !hasOutcomeRule(d, tc.want) {
				t.Fatalf("missing %s: %v", tc.want, d.LintSound())
			}
		})
	}
}

func TestEquivalentGateAnswersNeedReason(t *testing.T) {
	for _, declared := range []bool{false, true} {
		d := wf.New("aliases")
		g := wf.NewGate[sdAsk, string](d, "Continue?", wf.NewEnum(d, "answer", "yes", "ok"), wf.Reserved)
		end := wf.NewTerminal[sdDone](d, wf.Success)
		g.Answers(wf.On("yes", g.To(end, "yes").Ends(wf.Success, "approved")), wf.On("ok", g.To(end, "ok").Ends(wf.Success, "approved")))
		if declared {
			g.EquivalentAnswers("yes and ok are deliberate synonyms")
		}
		if hasOutcomeRule(d, wf.RuleGateAnswersEquivalent) == declared {
			t.Fatalf("declared=%v: %v", declared, d.LintSound())
		}
	}
}

func TestTerminalIntentAndEndsInViewAndHash(t *testing.T) {
	makeDef := func(intent string, expected wf.Outcome) *wf.Def {
		d := wf.New("intent")
		a := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		end := wf.NewTerminal[sdDone](d, wf.Success).Intent(intent)
		a.Then(a.To(end, "report").Ends(expected, "the report was produced"))
		return d
	}
	d := makeDef("negative review is a successfully produced result", wf.Success)
	v := wf.Inspect(d)
	if v.States[0].Edges[0].EndsOutcome != wf.Success || v.States[1].Intent == "" {
		t.Fatalf("missing contracts: %+v", v.States)
	}
	if d.Hash() == makeDef("different intent", wf.Success).Hash() || d.Hash() == makeDef("negative review is a successfully produced result", wf.Stopped).Hash() {
		t.Fatal("contract omitted from hash")
	}
	bad := makeDef(" ", wf.Success)
	if !strings.Contains(fmt.Sprint(bad.Lint()), "Intent") {
		t.Fatal("blank intent accepted")
	}
}

func TestEndsCannotChangeSealedContract(t *testing.T) {
	d := wf.New("sealed-ends")
	a := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	end := wf.NewTerminal[sdDone](d, wf.Success)
	e := a.To(end, "complete").Ends(wf.Success, "produced result")
	a.Then(e)
	before := d.Hash()
	e.Ends(wf.Stopped, "changed")
	if after := d.Hash(); after != before {
		t.Fatalf("sealed hash changed: %s -> %s", before, after)
	}
	if !strings.Contains(fmt.Sprint(d.Lint()), "sealed") {
		t.Fatal("sealed misuse not reported")
	}
}

func TestGateOutcomeContractBoundaries(t *testing.T) {
	for _, reservation := range []wf.Reservation{wf.Reserved, wf.PolicyAnswerable} {
		for _, dynamic := range []bool{false, true} {
			d := wf.New("candidate-contract")
			g := wf.NewGate[sdAsk, string](d, "Finish?", wf.NewEnum(d, "answer", "yes"), reservation)
			end := wf.NewTerminal[sdDone](d, wf.Success)
			e := g.To(end, "finish").Ends("legacy reason is not an outcome contract")
			if dynamic {
				g.Answers(wf.OnRoute("yes", func(wf.Ctx) wf.Edge[sdAsk] { return e }, e))
			} else {
				g.Answers(wf.On("yes", e))
			}
			if !hasOutcomeRule(d, wf.RuleGateEndsMissing) {
				t.Fatalf("reservation=%v dynamic=%v missing contract accepted", reservation, dynamic)
			}
		}
	}
	d := wf.New("nonterminal-contract")
	a := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	b := wf.NewCompute[sdRecord, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	end := wf.NewTerminal[sdDone](d, wf.Success)
	a.Then(a.To(b, "cleanup").Ends(wf.Success, "eventually succeeds"))
	b.Then(b.To(end, "finished"))
	if !hasOutcomeRule(d, wf.RuleEndsOutcomeMismatch) {
		t.Fatal("nonterminal accepted as terminal outcome contract")
	}
}

func TestEquivalentGateAnswersRespectEffects(t *testing.T) {
	d := wf.New("distinct-effects")
	g := wf.NewGate[sdAsk, string](d, "Continue?", wf.NewEnum(d, "answer", "yes", "ok"), wf.Reserved)
	end := wf.NewTerminal[sdDone](d, wf.Success)
	count := d.Counter("approvals")
	g.Answers(wf.On("yes", g.To(end, "yes").Ends(wf.Success, "approved").With(count.Inc())), wf.On("ok", g.To(end, "ok").Ends(wf.Success, "acknowledged")))
	if hasOutcomeRule(d, wf.RuleGateAnswersEquivalent) {
		t.Fatal("distinct effectful answers treated as equivalent")
	}
}

func TestOutcomeContractDeclarationValidation(t *testing.T) {
	for _, args := range []struct {
		first any
		rest  []string
	}{
		{42, nil}, {wf.Outcome("unknown"), []string{"why"}}, {wf.Success, nil},
		{wf.Success, []string{"one", "two"}}, {"legacy", []string{"extra"}}, {wf.Success, []string{" "}},
	} {
		d := wf.New("invalid-ends")
		a := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		end := wf.NewTerminal[sdDone](d, wf.Success)
		a.Then(a.To(end, "complete").Ends(args.first, args.rest...))
		if len(d.Lint()) == 0 {
			t.Fatalf("accepted Ends(%v,%v)", args.first, args.rest)
		}
	}
}

func TestOutcomeIntentMetadataSealing(t *testing.T) {
	makeDef := func(reason string) (*wf.Def, *wf.Gate[sdAsk, string], *wf.Terminal[sdDone]) {
		d := wf.New("intent-metadata")
		g := wf.NewGate[sdAsk, string](d, "Finish?", wf.NewEnum(d, "answer", "yes"), wf.Reserved).EquivalentAnswers(reason)
		end := wf.NewTerminal[sdDone](d, wf.Success).Intent("the requested report was produced")
		g.Answers(wf.On("yes", g.To(end, "done").Ends(wf.Success, "approved")))
		return d, g, end
	}
	d, g, end := makeDef("intentional synonyms")
	before := d.Hash()
	if wf.Inspect(d).States[0].EquivalentAnswers != "intentional synonyms" {
		t.Fatal("equivalence reason missing from view")
	}
	other, _, _ := makeDef("different reason")
	if before == other.Hash() {
		t.Fatal("equivalence reason omitted from hash")
	}
	g.EquivalentAnswers("changed")
	end.Intent("changed")
	if d.Hash() != before {
		t.Fatal("sealed intent metadata changed")
	}
	blank, _, _ := makeDef(" ")
	if !strings.Contains(fmt.Sprint(blank.Lint()), "EquivalentAnswers") {
		t.Fatal("blank equivalence accepted")
	}
}
