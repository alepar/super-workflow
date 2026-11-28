// Package checkrepair is lane D pattern P13: a deterministic check state
// feeding a repair agent. A command runs the tests; on failure a compute state
// captures the failing run's stderr and the repair agent receives it as a typed
// prompt input, then the tests run again.
//
// A prompt input may name only an agent or compute state (wf.Out), never a
// command, so the stderr reaches the agent through CaptureFailure, which reads
// the same test run as an observation.
package checkrepair

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// TestExit is the test script's exit code.
type TestExit int

// Test exit codes.
const (
	TestsPass TestExit = 0
	TestsFail TestExit = 1
)

// Patch is the repair agent's output.
type Patch struct{ Summary string }

// Marker types, one per state.
type (
	RunTests       struct{}
	CaptureFailure struct{}
	Repair         struct{}
	Green          struct{}
	GaveUp         struct{}
)

var testRun = shell.Run("go", "test", "./...")

// Definition returns the checkrepair definition.
func Definition() *wf.Def {
	d := wf.New("checkrepair").Version(1)
	exits := wf.NewEnum(d, "test-exit", TestsPass, TestsFail)

	tests := shell.Command[RunTests](d, testRun)
	capture := wf.NewCompute[CaptureFailure, wf.RunResult](d, func(c wf.Ctx) (wf.RunResult, error) {
		return wf.Observed(c, shell.Observe(testRun))
	})
	repair := wf.NewAgent[Repair, Patch](d)
	green := wf.NewTerminal[Green](d, wf.Success)
	gaveUp := wf.NewTerminal[GaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)

	tests.RouteCases(wf.Cases[RunTests, wf.RunResult](wf.ExitAs[TestExit], exits,
		wf.On(TestsPass, tests.To(green, "tests pass")),
		wf.On(TestsFail, tests.To(capture, "tests fail: capture stderr"))))
	// The capture reads the test run: an unavailable read gives up.
	capture.Observes(shell.Observe(testRun))
	capture.Retry(wf.Retry[CaptureFailure](wf.Const(1), capture.To(gaveUp, "CaptureFailure failed x1")).
		OnClass(wf.Unavailable, capture.To(gaveUp, "test run unreadable: give up")))
	toRepair := capture.To(repair, "stderr to the repair agent")
	capture.Route(func(wf.RunResult, wf.Ctx) wf.Edge[CaptureFailure] { return toRepair }, toRepair)
	repair.Spec(wf.PromptText("Fix the failing tests. The failing run:\n{{failure}}", wf.As("failure", wf.Out(capture))),
		wf.Retry(wf.Const(3), repair.To(gaveUp, "repairs exhausted")),
		wf.Checkf("vet-clean", func(Patch, wf.Ctx) error { return nil }).
			Observes(shell.Observe(shell.Run("go", "vet", "./...")), fsx.SchemaCheck("repair.json", fsx.Schema{Name: "patch", Path: "schemas/patch.json"})))
	again := repair.To(tests, "patched: run tests again")
	repair.Route(func(Patch, wf.Ctx) wf.Edge[Repair] { return again }, again)
	// The third patch is tested before a fourth repair is refused.
	wf.MaxVisits(wf.Const(3), repair.To(gaveUp, "tests still fail after three repair rounds"))
	return d
}
