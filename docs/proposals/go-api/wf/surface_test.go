package wf_test

import (
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type Verdict string

const (
	Approved         Verdict = "approved"
	ChangesRequested Verdict = "changes_requested"
)

type Review struct {
	Verdict Verdict `json:"verdict"`
}

type Exit int

type Answer string

type Opts struct{ Autonomous bool }

type Item struct{ ID string }
type ItemResult struct{ Report wf.Ref[string] }
type SubIn struct{ Spec wf.Ref[string] }
type SubOut struct{ Notes wf.Ref[string] }

// marker types
type (
	Write    struct{}
	Check    struct{}
	Compute  struct{}
	Ask      struct{}
	WaitCI   struct{}
	Fan      struct{}
	Nested   struct{}
	Done     struct{}
	Stop     struct{}
	ChildRun struct{}
	ChildEnd struct{}
	SubWork  struct{}
	SubEnd   struct{}
)

func itemChild() wf.Child[Item, ItemResult] {
	d := wf.New("item")
	run := wf.NewAgent[ChildRun, string](d)
	end := wf.NewTerminal[ChildEnd](d, wf.Success)
	ok := run.To(end, "done")
	run.Spec(wf.PromptText("work on {{item}}", wf.ChildInput()), wf.Retry(wf.Const(2), ok))
	run.Route(func(string, wf.Ctx) wf.Edge[ChildRun] { return ok }, ok)
	wf.Yield(end, func(wf.Ctx) ItemResult { return ItemResult{} })
	return wf.Typed[Item, ItemResult](d)
}

func subChild() wf.Child[SubIn, SubOut] {
	d := wf.New("nested")
	work := wf.NewAgent[SubWork, string](d)
	end := wf.NewTerminal[SubEnd](d, wf.Success)
	notes := wf.NewArtifact[string](work, "notes", "notes.md")
	wf.Export(d, notes)
	ok := work.To(end, "done")
	work.Spec(wf.PromptFile("prompts/work.md", wf.ChildInput()), wf.Retry(wf.Const(1), ok))
	work.Route(func(string, wf.Ctx) wf.Edge[SubWork] { return ok }, ok)
	wf.Yield(end, func(wf.Ctx) SubOut { return SubOut{} })
	return wf.Typed[SubIn, SubOut](d)
}

func definition() *wf.Def {
	d := wf.New("surface").Version(1)
	epic := d.Param("epic")
	d.RunKey(epic)
	rounds := d.Counter("rounds")
	merge := d.Lock("merge", wf.LockRun)
	opts := wf.Options[Opts](d)
	verdicts := wf.NewEnum(d, "verdict", Approved, ChangesRequested)
	exits := wf.NewEnum(d, "check-exit", Exit(0), Exit(2), Exit(3))
	answers := wf.NewEnum(d, "answer", Answer("go"), Answer("stop"))

	write := wf.NewAgent[Write, Review](d)
	check := wf.NewCommand[Check](d, "scripts/check", "--json")
	comp := wf.NewCompute[Compute, int](d, func(c wf.Ctx) (int, error) { return c.Count(rounds), nil })
	ask := wf.NewGate[Ask](d, "Proceed?", answers, wf.PolicyAnswerable)
	ci := wf.NewWait[WaitCI](d, wf.Observe[bool]("gh", "checks", map[string]any{"pr": 1}), "checks green", func(b bool) bool { return b })
	items := wf.Observe[[]Item]("bd", "ready", nil)
	fan := wf.NewMap[Fan](d, wf.FromObservation(items), func(it Item) string { return it.ID }, itemChild())
	spec := wf.NewArtifact[string](write, "spec", "spec.md")
	nested := wf.NewSub[Nested](d, subChild(), func(c wf.Ctx) SubIn { return SubIn{Spec: wf.RefOf(c, spec)} })
	done := wf.NewTerminal[Done](d, wf.Success)
	stop := wf.NewTerminal[Stop](d, wf.Stopped)

	toCheck := write.To(check, "written")
	giveUp := write.To(stop, "no progress")
	write.Spec(wf.PromptFile("prompts/write.md", epic, spec),
		wf.Retry(wf.Const(3).Plus(wf.Count(rounds)), giveUp).NoProgressAfter(2).OnClass(wf.Unavailable, giveUp),
		wf.Checkf("has verdict", func(r Review, _ wf.Ctx) error { return nil }))
	write.RouteCases(wf.Cases(func(r Review) Verdict { return r.Verdict }, verdicts,
		wf.On(Approved, toCheck), wf.On(ChangesRequested, toCheck.With(rounds.Inc(), wf.ExternalEffect{Adapter: "bd", Op: "create", Key: wf.KeyOf("x")}))))

	pass := check.To(comp, "pass")
	fail := check.To(stop, "fail")
	check.RouteCases(wf.Cases(wf.ExitAs[Exit], exits, wf.On(Exit(0), pass), wf.On(Exit(2), fail), wf.On(Exit(3), fail)))
	wf.Hold(check, merge)

	toAsk := comp.To(ask, "computed")
	comp.Route(func(int, wf.Ctx) wf.Edge[Compute] { return toAsk }, toAsk)

	goOn := ask.To(ci, "go")
	halt := ask.To(stop, "stop")
	ask.Answers(wf.On(Answer("go"), goOn), wf.On(Answer("stop"), halt)).
		Presents(spec).Timeout(24*time.Hour, halt)

	ci.Ready(ci.To(fan, "green")).Poll(time.Minute).Timeout(time.Hour, ci.To(stop, "timeout"))

	met := fan.To(nested, "all ok")
	partial := fan.To(stop, "partial")
	fan.Parallel(4).Writes("docs/**").ItemWrites(func(Item) []string { return nil }).
		Lease(10*time.Minute).OnSiblingFailure(wf.ContinueSiblings).Requery().
		Join(wf.AllOK(), met, partial)

	nested.Exits(wf.OnExit[SubEnd](nested.To(done, "nested done")))
	wf.Consumes(nested, wf.FromChild(nested, wf.NewArtifact[string](nested, "unused", "x")))
	wf.MaxVisits(wf.Const(5), giveUp)
	wf.DoneWhen(check, wf.Observe[bool]("fs", "exists", map[string]any{"path": "out.json"}), "out.json exists")

	d.SetPolicy(wf.Policy(
		wf.PolicyAnswer(ask, Answer("go"), wf.Field(opts, func(o *Opts) *bool { return &o.Autonomous }).Is(true)).Park("auto-approved"),
	))
	return d
}

func TestSurfaceBuildsWithoutMisuse(t *testing.T) {
	d := definition()
	if d.Name() != "surface" {
		t.Fatalf("name = %q", d.Name())
	}
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleBuild {
			t.Errorf("definition-time misuse: %s", p)
		}
	}
}

func TestOutcomeClassNames(t *testing.T) {
	if got := wf.Unavailable.String(); got != "unavailable" {
		t.Fatalf("Unavailable.String() = %q", got)
	}
}

// Compile-only references to declared-but-unimplemented executor types.
var (
	_ wf.Executor
	_ wf.Journal
	_ = wf.Instruction{}
	_ = wf.Submission{}
	_ = wf.SubmitResult{}
	_ = wf.JournalRecord{Kind: wf.JournalAutoAnswered}
	_ = wf.Migration{}
	_ = wf.DispatchHints{}
	_ = []wf.JoinPolicy{wf.AllSettled(), wf.Quorum(2), wf.JoinWhen("any", func(wf.MapResult[string, ItemResult]) bool { return true })}
	_ = wf.MapResult[string, ItemResult]{}
	_ = wf.Instance[ItemResult]{BudgetStopped: true}
	_ = wf.FromOutput[Write, Review, Item]
	_ = wf.Joined[Fan, Item, string, ItemResult]
	_ = wf.ChildResult[Nested, SubIn, SubOut]
	_ = wf.OutputOf[Write, Review]
	_ = wf.ComputedOf[Compute, int]
	_ = wf.InputOf[SubIn]
	_ = wf.OptionsOf[Opts]
	_ = wf.Observed[bool]
	_ = wf.Read[string]
	_ = wf.CasesOpen[Write, Review, string]
	_ = wf.OnRoute[Write, Verdict]
	_ = wf.Not
	_ = wf.NonIdempotent
	_ = wf.Reserved
	_ = wf.LockRunKey
	_ = wf.CancelSiblings
	_ = wf.Main
	_ = wf.DoneWhen[bool]
)
