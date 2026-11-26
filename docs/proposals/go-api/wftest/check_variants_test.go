package wftest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	variantOpts  struct{ Skip bool }
	variantDraft struct{}
	variantAudit struct{}
	variantDone  struct{}
	variantPick  struct{}
)

// variantFactory is draft -> audit -> done, where the Skip option routes
// around audit. markSkip declares the skip edge unreachable, the workaround a
// single options value forces.
func variantFactory(markSkip bool) func() *wftest.Sim {
	return func() *wftest.Sim {
		d := wf.New("review")
		opts := wf.Options[variantOpts](d)
		skip := wf.Field(opts, func(o *variantOpts) *bool { return &o.Skip }).Is(true)
		draft := wf.NewAgent[variantDraft, bool](d)
		audit := wf.NewAgent[variantAudit, bool](d)
		done := wf.NewTerminal[variantDone](d, wf.Success)
		stop := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
		stop.Answers(wf.On(fuzzVerdict("yes"), stop.To(done, "approved").Ends(wf.Success, "the person approves completion")))
		d.OnFailure(wf.Blocked, stop)
		draft.Spec(wf.PromptText("draft"), wf.Retry[variantDraft](wf.Const(1), draft.To(stop, "exhausted")))
		audit.Spec(wf.PromptText("audit"), wf.Retry[variantAudit](wf.Const(1), audit.To(stop, "exhausted")))
		draft.Then(draft.To(audit, "drafted"))
		skipEdge := audit.To(done, "skip audit").When(skip)
		if markSkip {
			skipEdge.Unreachable("only one options value is simulated")
		}
		audit.Then(audit.To(done, "audited"))
		return wftest.New(d)
	}
}

func variantConfig(variants map[string]any) wftest.CheckConfig {
	corpus := map[string][]json.RawMessage{"variantDraft": {json.RawMessage(`true`)}, "variantAudit": {json.RawMessage(`true`)}}
	return wftest.CheckConfig{Variants: variants, CalibrationConfig: wftest.CalibrationConfig{Seeds: 4, Valid: corpus, Progress: corpus}}
}

func TestCheckOneOptionsValueLeavesTheGuardedPathUncovered(t *testing.T) {
	got := wftest.Check(variantFactory(false), variantConfig(nil))
	if !strings.Contains(strings.Join(got.CoverageProblems, "\n"), `"skip audit"`) {
		t.Fatalf("skip edge reported covered under Skip=false: %v", got.Problems)
	}
	if len(got.Variants) != 0 || (strings.Contains(got.String(), "variants:") || strings.Contains(got.String(), "variant ")) {
		t.Fatalf("a config without Variants mentions variants: %s", got)
	}
}

// TestCheckVariantsCoverTheUnion is the pattern a workflow adopts: one named
// variant per side of each option guard, coverage judged on their union.
func TestCheckVariantsCoverTheUnion(t *testing.T) {
	got := wftest.Check(variantFactory(false), variantConfig(map[string]any{
		"audited": variantOpts{},
		"skipped": variantOpts{Skip: true},
	}))
	if len(got.Problems) != 0 {
		t.Fatalf("union of variants not covered: %v", got.Problems)
	}
	if strings.Join(got.Variants, ",") != "audited,skipped" || !strings.Contains(got.String(), "variants: audited, skipped") {
		t.Fatalf("variants not reported: %v\n%s", got.Variants, got)
	}
	// Each variant alone leaves a gap, so the union is doing the work.
	for name, value := range map[string]any{"audited": variantOpts{}, "skipped": variantOpts{Skip: true}} {
		alone := wftest.Check(variantFactory(false), variantConfig(map[string]any{name: value}))
		if len(alone.Problems) == 0 {
			t.Errorf("variant %s alone covered everything", name)
		}
	}
}

func TestCheckVariantReachingAnUnreachableEdgeIsAViolation(t *testing.T) {
	got := wftest.Check(variantFactory(true), variantConfig(map[string]any{
		"audited": variantOpts{},
		"skipped": variantOpts{Skip: true},
	}))
	real := strings.Join(got.RealProblems, "\n")
	if !strings.Contains(real, "marked unreachable (only one options value is simulated)") || !strings.Contains(real, "(variant skipped)") {
		t.Fatalf("traversed exemption not a violation naming its variant: %v", got.RealProblems)
	}
}

func TestCheckVariantsRequireEachGuardToHold(t *testing.T) {
	got := wftest.Check(variantFactory(false), variantConfig(map[string]any{
		"audited":       variantOpts{},
		"audited-again": &variantOpts{},
	}))
	want := "variants: guard holds under no variant: review/variantAudit -> variantDone when Skip=true"
	if strings.Join(got.RealProblems, "\n") != want || got.Coverage != nil || got.Runs != 0 {
		t.Fatalf("unheld guard: problems %v, coverage %v, runs %d", got.RealProblems, got.Coverage, got.Runs)
	}
}

func TestCheckVariantFindingsNameTheirVariant(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("variant-panic")
		opts := wf.Options[variantOpts](d)
		pick := wf.NewCompute[variantPick, bool](d, func(c wf.Ctx) (bool, error) {
			if wf.OptionsOf(c, opts).Skip {
				panic("skip is not supported")
			}
			return true, nil
		})
		done := wf.NewTerminal[variantDone](d, wf.Success)
		pick.Then(pick.To(done, "picked"))
		return wftest.New(d)
	}
	got := wftest.Check(factory, wftest.CheckConfig{Variants: map[string]any{"plain": variantOpts{}, "skip": variantOpts{Skip: true}}, CalibrationConfig: wftest.CalibrationConfig{Seeds: 1}})
	if len(got.Findings) == 0 || got.Findings[0].Variant != "skip" || !strings.Contains(got.Findings[0].String(), "variant: skip") {
		t.Fatalf("finding without its variant: %v\n%s", got.Findings, got)
	}
	tagged := false
	for _, problem := range got.RealProblems {
		tagged = tagged || strings.HasPrefix(problem, "variant skip: ")
	}
	if !tagged {
		t.Fatalf("problem not tagged with its variant: %v", got.RealProblems)
	}
}

// TestCheckVariantsEachSpendTheirOwnRuns bounds MaxRuns at about one
// variant's need. A budget shared in name order lets "audited" spend it all
// and leaves the later "skipped" variant's guarded edge untraversed; each
// variant spending its own MaxRuns covers both sides.
func TestCheckVariantsEachSpendTheirOwnRuns(t *testing.T) {
	cfg := variantConfig(map[string]any{"audited": variantOpts{}, "skipped": variantOpts{Skip: true}})
	cfg.MaxRuns = 30
	got := wftest.Check(variantFactory(false), cfg)
	if len(got.Problems) != 0 {
		t.Fatalf("bounded check starved a variant: %v", got.Problems)
	}
	if got.Coverage.EdgesReached["review/variantAudit/edge[1]"] == 0 {
		t.Fatalf("the later variant's guarded edge was not traversed: %v", got.Coverage.EdgesReached)
	}
	if got.Runs <= cfg.MaxRuns {
		t.Fatalf("runs %d not summed across variants (MaxRuns %d each)", got.Runs, cfg.MaxRuns)
	}
	// A refusal names the variant whose budget refused it.
	cfg.MaxRuns = 2
	tight := wftest.Check(variantFactory(false), cfg)
	tagged := map[string]bool{}
	for _, target := range tight.NotExplored {
		name, _, _ := strings.Cut(strings.TrimPrefix(target, "variant "), ":")
		tagged[name] = strings.HasPrefix(target, "variant ")
	}
	if !tagged["audited"] || !tagged["skipped"] {
		t.Fatalf("budget refusals not tagged per variant: %v", tight.NotExplored)
	}
}
