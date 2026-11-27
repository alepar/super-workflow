package wftest_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	fcReview struct{}
	fcTally  struct{}
	fcFull   struct{}
	fcShort  struct{}
	fcOut    struct{ OK bool }
)

// coverageDef is a review agent with a required and an optional reference
// input, large enough to be paged. Its route reads the attempt it decides; a
// compute after it reads the settled record into *seen.
func coverageDef(name string, seen *[]coverageRead) (*wf.Def, *wf.Agent[fcReview, fcOut]) {
	d := wf.New(name)
	review := wf.NewAgent[fcReview, fcOut](d).ReadsFetchCoverage()
	full := wf.NewTerminal[fcFull](d, wf.Success)
	short := wf.NewTerminal[fcShort](d, wf.Success)
	tally := wf.NewCompute[fcTally](d, func(c wf.Ctx) (bool, error) {
		f, ok := wf.FetchCoverageOf(c, review)
		*seen = append(*seen, coverageRead{f, ok})
		return ok && f.RequiredComplete(), nil
	})
	toTally := review.To(tally, "tally")
	review.Spec(wf.PromptText("Review {{spec}} with {{notes}}.",
		wf.Reference(wf.Bound("spec", strings.Repeat("spec constraint. ", 600)), true),
		wf.Reference(wf.Bound("notes", strings.Repeat("background note. ", 600)), false),
	), wf.Retry(wf.Const(3), toTally).OnClass(wf.Blocked, review.To(short, "blocked")))
	review.Route(func(_ fcOut, c wf.Ctx) wf.Edge[fcReview] {
		f, ok := wf.FetchCoverageOf(c, review)
		routed = append(routed, coverageRead{f, ok})
		return toTally
	}, toTally)
	toFull, toShort := tally.To(full, "full"), tally.To(short, "short")
	tally.Route(func(ok bool, _ wf.Ctx) wf.Edge[fcTally] {
		if ok {
			return toFull
		}
		return toShort
	}, toFull, toShort)
	return d, review
}

// routed is what review's own route read, the attempt it decided.
var routed []coverageRead

type coverageRead struct {
	f  wf.FetchCoverage
	ok bool
}

func lastRead(t *testing.T, seen []coverageRead) coverageRead {
	t.Helper()
	if len(seen) == 0 {
		t.Fatal("the compute never read coverage")
	}
	return seen[len(seen)-1]
}

func reference(t *testing.T, f wf.FetchCoverage, name string) wf.ReferenceCoverage {
	t.Helper()
	for _, r := range f.References {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no reference %q in %+v", name, f)
	return wf.ReferenceCoverage{}
}

// By default the paged subagent fetches every required page: the prompt and
// the required reference are complete, the optional one is untouched.
func TestFetchCoverageRequiredAndOptional(t *testing.T) {
	var seen []coverageRead
	routed = nil
	d, _ := coverageDef("coverage-default", &seen)
	tr := wftest.New(d, wftest.Cap(1), wftest.PagedPrompts(), wftest.WithTB(t)).Run(
		wftest.SubmitAt("fcReview", fcOut{OK: true}),
	)
	if tr.Terminal() != "fcFull" {
		t.Fatalf("terminal %q, want fcFull", tr.Terminal())
	}
	got := lastRead(t, seen)
	if !got.ok {
		t.Fatal("a paged run's coverage reads as unknown")
	}
	spec, notes := reference(t, got.f, "spec"), reference(t, got.f, "notes")
	if !got.f.Prompt.Complete() || got.f.Prompt.Pages == 0 {
		t.Errorf("prompt %+v, want every page fetched", got.f.Prompt)
	}
	if !spec.Required || spec.Pages < 2 || !spec.Complete() {
		t.Errorf("spec %+v, want a required paged reference fetched in full", spec)
	}
	if notes.Required || notes.Pages < 2 || notes.Fetched != 0 {
		t.Errorf("notes %+v, want an optional paged reference not fetched", notes)
	}
	if !strings.HasPrefix(got.f.Unit, "fcReview") {
		t.Errorf("unit %q, want the review unit", got.f.Unit)
	}
	if len(routed) == 0 || !routed[len(routed)-1].ok || routed[len(routed)-1].f.Unit != got.f.Unit || !routed[len(routed)-1].f.RequiredComplete() {
		t.Errorf("route read %+v, want the attempt it decided", routed)
	}
}

// A page fetched twice counts once, and a short fetch routes short.
func TestFetchCoverageRefetchCountsOnce(t *testing.T) {
	var seen []coverageRead
	d, _ := coverageDef("coverage-refetch", &seen)
	tr := wftest.New(d, wftest.Cap(1), wftest.PagedPrompts(), wftest.WithTB(t)).Run(
		wftest.SubmitAt("fcReview", fcOut{OK: true}).Fetching(wftest.Pages(""), wftest.Pages("spec", 1, 1), wftest.Pages("notes", 2)),
	)
	if tr.Terminal() != "fcShort" {
		t.Fatalf("terminal %q, want fcShort", tr.Terminal())
	}
	got := lastRead(t, seen)
	spec, notes := reference(t, got.f, "spec"), reference(t, got.f, "notes")
	if spec.Fetched != 1 || notes.Fetched != 1 || got.f.RequiredComplete() {
		t.Errorf("spec %+v notes %+v, want one page of each counted once and the required set short", spec, notes)
	}
}

// A retried attempt starts its own count: the record is the accepted
// attempt's, not the failed one's.
func TestFetchCoverageRetriedAttempt(t *testing.T) {
	var seen []coverageRead
	d, review := coverageDef("coverage-retry", &seen)
	tr := wftest.New(d, wftest.Cap(1), wftest.PagedPrompts(), wftest.WithTB(t)).Run(
		wftest.Fail(review, wf.FailTransient, "flaked"),
		wftest.SubmitAt("fcReview", fcOut{OK: true}).Fetching(),
	)
	if tr.Terminal() != "fcShort" {
		t.Fatalf("terminal %q, want fcShort", tr.Terminal())
	}
	got := lastRead(t, seen)
	spec := reference(t, got.f, "spec")
	if got.f.Prompt.Fetched != 1 || spec.Fetched != 0 {
		t.Errorf("prompt %+v spec %+v, want only the retried attempt's first prompt page", got.f.Prompt, spec)
	}
	if !strings.HasSuffix(got.f.Unit, "#2") {
		t.Errorf("unit %q, want the second attempt's", got.f.Unit)
	}
}

// A run without paged delivery reads as unknown, never as zero.
func TestFetchCoverageUnpagedIsUnknown(t *testing.T) {
	var seen []coverageRead
	d, _ := coverageDef("coverage-unpaged", &seen)
	tr := wftest.New(d, wftest.Cap(1), wftest.WithTB(t)).Run(
		wftest.SubmitAt("fcReview", fcOut{OK: true}),
	)
	if tr.Terminal() != "fcShort" {
		t.Fatalf("terminal %q, want fcShort", tr.Terminal())
	}
	if got := lastRead(t, seen); got.ok || got.f.Prompt.Pages != 0 || len(got.f.References) != 0 {
		t.Errorf("unpaged read = %+v, want unknown", got)
	}
}

// A read of a state that did not declare ReadsFetchCoverage blocks the step.
func TestFetchCoverageUndeclaredReadBlocks(t *testing.T) {
	d := wf.New("coverage-undeclared")
	review := wf.NewAgent[fcReview, fcOut](d)
	end := wf.NewTerminal[fcFull](d, wf.Success)
	tally := wf.NewCompute[fcTally](d, func(c wf.Ctx) (bool, error) {
		_, ok := wf.FetchCoverageOf(c, review)
		return ok, nil
	})
	toTally := review.To(tally, "tally")
	review.Spec(wf.PromptText("Review."), wf.Retry(wf.Const(1), toTally))
	review.Then(toTally)
	tally.Then(tally.To(end, "done"))
	rec := &recordingTB{TB: t}
	tr := wftest.New(d, wftest.Cap(1), wftest.PagedPrompts(), wftest.WithTB(rec)).Run(
		wftest.Submit(review, fcOut{OK: true}),
	)
	if tr.Terminal() != "" {
		t.Fatalf("terminal %q, want the run blocked", tr.Terminal())
	}
	found := false
	for _, r := range tr.Journal() {
		found = found || strings.Contains(string(r.Data), "does not declare ReadsFetchCoverage")
	}
	if !found {
		t.Errorf("no journaled cause names the missing declaration")
	}
}

// Lint notes a definition that reads fetch coverage; it is not an error.
func TestFetchCoverageLintNote(t *testing.T) {
	var seen []coverageRead
	d, _ := coverageDef("coverage-lint", &seen)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("Lint = %v, want none", ps)
	}
	ns := d.Notes()
	if len(ns) != 1 || ns[0].Rule != wf.RuleFetchCoverage || ns[0].Severity != wf.SeverityNote || !strings.Contains(ns[0].Msg, "fetched, never read") {
		t.Fatalf("Notes = %v, want one fetch-coverage note", ns)
	}
}

// Fetching without PagedPrompts is a script error, and so is PagedPrompts
// without Cap.
func TestFetchingNeedsPagedPrompts(t *testing.T) {
	var seen []coverageRead
	d, _ := coverageDef("coverage-misuse", &seen)
	rec := &recordingTB{TB: t}
	wftest.New(d, wftest.Cap(1), wftest.WithTB(rec)).Run(wftest.SubmitAt("fcReview", fcOut{}).Fetching())
	rec2 := &recordingTB{TB: t}
	wftest.New(d, wftest.PagedPrompts(), wftest.WithTB(rec2)).Run(wftest.SubmitAt("fcReview", fcOut{}))
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0], "Fetching needs PagedPrompts") {
		t.Errorf("Fetching without PagedPrompts: %q", rec.msgs)
	}
	if len(rec2.msgs) != 1 || !strings.Contains(rec2.msgs[0], "PagedPrompts needs Cap") {
		t.Errorf("PagedPrompts without Cap: %q", rec2.msgs)
	}
}
