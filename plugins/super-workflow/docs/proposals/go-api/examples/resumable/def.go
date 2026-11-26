// Package resumable is lane D pattern P11 and VISION E3: a stage-addressable
// pipeline that resumes after an interruption. Each stage carries a done-when
// postcondition read through an observation, so a stage whose output already
// exists is skipped; the run is addressed by its RunKey, pinned to a definition
// Version, and a crash inside the non-idempotent publish stage routes through
// the uncertain outcome class to a verification state instead of repeating it.
package resumable

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// StageExit is the exit code of a stage script.
type StageExit int

// Stage exit codes.
const (
	StageOK     StageExit = 0
	StageFailed StageExit = 1
)

// Verified is the verify agent's output.
type Verified struct{ Published bool }

// Marker types, one per state.
type (
	Fetch     struct{}
	Transform struct{}
	Publish   struct{}
	Verify    struct{}
	Done      struct{}
	Failed    struct{}
)

// Paths of the stage outputs, addressed by the run key.
const (
	fetched   = "work/{dataset}/raw.json"
	shaped    = "work/{dataset}/shaped.json"
	published = "work/{dataset}/published.json"
)

// Definition returns the resumable definition.
func Definition() *wf.Def {
	d := wf.New("resumable").Version(3)
	dataset := d.Param("dataset")
	d.RunKey(dataset)
	// Version 2 -> 3 renamed Load to Transform; the hashes are the pinned values
	// the executor recorded for each version.
	d.Migrations(wf.Migration{FromHash: "sha256:v2-pin", ToHash: "sha256:v3-pin", States: map[string]string{"Load": "Transform"}})

	exits := wf.NewEnum(d, "stage-exit", StageOK, StageFailed)
	fetch := shell.Command[Fetch](d, shell.Run("scripts/fetch", "{dataset}"))
	transform := shell.Command[Transform](d, shell.Run("scripts/transform", "{dataset}"))
	publish := shell.Command[Publish](d, shell.Run("scripts/publish", "{dataset}"))
	verify := wf.NewAgent[Verify, Verified](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	failed := wf.NewTerminal[Failed](d, wf.Stopped)
	d.OnFailure(wf.Blocked, failed)

	wf.DoneWhen(fetch, fsx.Exists(fetched), "raw.json exists")
	wf.DoneWhen(transform, fsx.Exists(shaped), "shaped.json exists")
	wf.DoneWhen(publish, fsx.Hash(published), "published.json digest recorded")

	fetch.RouteCases(wf.Cases(wf.ExitAs[StageExit], exits,
		wf.On(StageOK, fetch.To(transform, "fetched")),
		wf.On(StageFailed, fetch.To(failed, "fetch failed").Ends("Fetch failure requires intervention before downstream stages"))))
	transform.RouteCases(wf.Cases(wf.ExitAs[StageExit], exits,
		wf.On(StageOK, transform.To(publish, "transformed")),
		wf.On(StageFailed, transform.To(failed, "transform failed").Ends("Transformation failure prevents publishing"))))
	// The upload cannot be repeated: after a crash the executor does not re-run
	// it, it takes the uncertain edge and the verify agent checks the world.
	stamp := shell.Effect(wf.KeyOf("stamp", "{dataset}"), shell.Run("scripts/stamp", "{dataset}"))
	publish.RouteCases(wf.Cases(wf.ExitAs[StageExit], exits,
		wf.On(StageOK, publish.To(done, "published").With(stamp)),
		wf.On(StageFailed, publish.To(failed, "publish failed").Ends("Publishing failed and must not be repeated automatically"))))
	publish.Retry(wf.Attempts(1, publish.To(failed, "publish failed")).
		OnClass(wf.Uncertain, publish.To(verify, "crash window: verify before repeating").
			With(shell.Effect(wf.NonIdempotent("upload is not repeatable: verify, then stamp by hand"), shell.Run("scripts/note-uncertain", "{dataset}")))))

	verify.Spec(wf.PromptText("Check whether the upload landed for {{dataset}}.", dataset),
		wf.Attempts(1, verify.To(failed, "cannot verify")))
	landed, missing := verify.To(done, "landed: done"), verify.To(failed, "did not land").Ends("The upload is missing; recovery requires manual intervention")
	verify.Route(func(v Verified, _ wf.Ctx) wf.Edge[Verify] {
		if v.Published {
			return landed
		}
		return missing
	}, landed, missing)
	return d
}
