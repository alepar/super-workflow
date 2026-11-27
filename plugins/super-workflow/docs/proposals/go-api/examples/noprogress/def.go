// Package noprogress is VISION E2: retry with no progress, then fallback. The
// fix agent may try up to five times, but when its progress fingerprint (the
// failing check ids) is unchanged for two consecutive attempts the executor
// stops early and takes the stalled edge, which is a different route from
// plain exhaustion.
package noprogress

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// FixOut is the fix agent's output.
type FixOut struct {
	Summary    string
	FailingIDs []string
}

// Marker types, one per state.
type (
	Fix       struct{}
	Fixed     struct{}
	GaveUp    struct{}
	StalledAt struct{}
)

// Definition returns the noprogress definition.
func Definition() *wf.Def {
	d := wf.New("noprogress").Version(1)

	fix := wf.NewAgent[Fix, FixOut](d)
	fixed := wf.NewTerminal[Fixed](d, wf.Success)
	gaveUp := wf.NewTerminal[GaveUp](d, wf.Stopped)
	stalled := wf.NewTerminal[StalledAt](d, wf.Stalled)
	d.OnFailure(wf.Blocked, gaveUp)

	fix.Spec(wf.PromptText("Fix the failing checks; the previous attempt's failures are repeated in the refusal."),
		wf.Retry(wf.Const(5), fix.To(gaveUp, "five attempts used")).
			NoProgressAfter(2).
			OnStalled(fix.To(stalled, "same failures twice: stalled")).
			Fingerprint("failing-check-ids", func(c wf.Ctx) string { return "" }),
		wf.Checkf("checks-green", func(o FixOut, _ wf.Ctx) error { return nil }))
	ok := fix.To(fixed, "checks green")
	fix.Route(func(FixOut, wf.Ctx) wf.Edge[Fix] { return ok }, ok)
	return d
}
