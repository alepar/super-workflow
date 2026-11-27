// Package agentless is localize, repair, validate, select (lane D P6, VISION
// E10): an agent localizes the fault, a Map produces one repair candidate per
// suspect, a command validates the candidates, and a pure compute picks the
// winner. Selection is deterministic code, never a model's opinion.
package agentless

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// State marker types (parent, then child).
type (
	Localize struct{}
	Repair   struct{}
	Validate struct{}
	Rank     struct{}
	Select   struct{}
	Fixed    struct{}
	NoFix    struct{}

	Patch     struct{}
	PatchDone struct{}
	PatchFail struct{}
)

// Suspect is one localized fault site.
type Suspect struct{ File, Symbol string }

// Suspects is the localizer's output.
type Suspects struct{ Sites []Suspect }

// Candidate is one repair candidate's typed result.
type Candidate struct {
	Site  string
	Diff  string
	Lines int
}

// PatchOut is the repair agent's output.
type PatchOut struct{ Diff string }

// Ranking orders the candidates, smallest diff first.
type Ranking struct{ Sites []string }

// Choice is the selected candidate, or none.
type Choice struct {
	Site string
	Any  bool
}

// ValidateExit is the validator's exit-code Enum.
type ValidateExit int

const (
	validated ValidateExit = 0
	noneGreen ValidateExit = 1
)

func repairChild() wf.Child[Suspect, Candidate] {
	d := wf.New("agentless-repair")
	patch := wf.NewAgent[Patch, PatchOut](d)
	ok := wf.NewTerminal[PatchDone](d, wf.Success)
	bad := wf.NewTerminal[PatchFail](d, wf.Stopped)
	d.OnFailure(wf.Blocked, bad)
	patch.Spec(wf.PromptText("Propose a minimal patch for {{item}}.", wf.ChildInput()),
		wf.Retry[Patch](wf.Const(2), patch.To(bad, "no patch")))
	patchNext := patch.To(ok, "patch proposed")
	patch.Route(func(PatchOut, wf.Ctx) wf.Edge[Patch] { return patchNext }, patchNext)
	wf.Yield(ok, func(wf.Ctx) Candidate { return Candidate{} })
	wf.Yield(bad, func(wf.Ctx) Candidate { return Candidate{} })
	return wf.Typed[Suspect, Candidate](d)
}

// Definition builds the pipeline.
func Definition() *wf.Def {
	d := wf.New("agentless")
	exits := wf.NewEnum(d, "validate-exit", validated, noneGreen)

	localize := wf.NewAgent[Localize, Suspects](d)
	repair := wf.NewMap[Repair, Suspect, string, Candidate](d,
		wf.FromOutput(localize, func(s Suspects) []Suspect { return s.Sites }),
		func(s Suspect) string { return s.File + "#" + s.Symbol },
		repairChild())
	validate := wf.NewCommand[Validate](d, "go", "test", "./...", "-run", "{candidate}")
	rank := wf.NewCompute[Rank, Ranking](d, func(c wf.Ctx) (Ranking, error) {
		// Smallest diff first; reads wf.Joined(c, repair), skipping
		// budget-stopped candidates.
		return Ranking{}, nil
	})
	sel := wf.NewCompute[Select, Choice](d, func(c wf.Ctx) (Choice, error) {
		r := wf.ComputedOf(c, rank)
		if len(r.Sites) == 0 {
			return Choice{}, nil
		}
		return Choice{Site: r.Sites[0], Any: true}, nil
	})
	fixed := wf.NewTerminal[Fixed](d, wf.Success)
	noFix := wf.NewTerminal[NoFix](d, wf.Stopped)
	d.OnFailure(wf.Blocked, noFix)

	localize.Spec(wf.PromptText("Localize the failing behaviour to suspect files and symbols."),
		wf.Retry[Localize](wf.Const(2), localize.To(noFix, "no localization")))
	localizeNext := localize.To(repair, "suspects found")
	localize.Route(func(Suspects, wf.Ctx) wf.Edge[Localize] { return localizeNext }, localizeNext)

	repair.Join(wf.AllSettled(),
		repair.To(validate, "candidates ready"),
		repair.To(validate, "some candidates missing"))
	validate.RouteCases(wf.Cases[Validate, wf.RunResult](wf.ExitAs[ValidateExit], exits,
		wf.On(validated, validate.To(rank, "some candidate is green")),
		wf.On(noneGreen, validate.To(noFix, "no candidate is green").Ends("All generated repair candidates failed validation")),
	))
	rankToSel := rank.To(sel, "ranked")
	rank.Route(func(Ranking, wf.Ctx) wf.Edge[Rank] { return rankToSel }, rankToSel)
	pick, none := sel.To(fixed, "winner selected"), sel.To(noFix, "nothing to select").Ends("No viable repair candidate remains to select")
	sel.Route(func(c Choice, _ wf.Ctx) wf.Edge[Select] {
		if c.Any {
			return pick
		}
		return none
	}, pick, none)
	return d
}
