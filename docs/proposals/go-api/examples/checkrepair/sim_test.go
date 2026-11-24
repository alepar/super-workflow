package checkrepair_test

import (
	"fmt"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/checkrepair"
	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func TestFailingTestsStopAfterThreeRepairs(t *testing.T) {
	world := wftest.NewWorld()
	wftest.Exit(world, 1, "go", "test")
	wftest.Set(world, shell.Observe(shell.Run("go", "test", "./...")), wf.RunResult{Exit: 1, Stderr: "test failed"})
	wftest.Set(world, shell.Observe(shell.Run("go", "vet", "./...")), wf.RunResult{})
	wftest.Set(world, fsx.SchemaCheck("repair.json", fsx.Schema{Name: "patch", Path: "schemas/patch.json"}), fsx.SchemaResult{OK: true})
	var script []wftest.Step
	for i := 1; i <= 3; i++ {
		script = append(script, wftest.SubmitAt("Repair", checkrepair.Patch{Summary: fmt.Sprintf("repair %d", i)}))
	}
	tr := wftest.New(checkrepair.Definition(), wftest.WithTB(t), wftest.WithWorld(world)).Run(script...)
	if tr.Terminal() != "GaveUp" {
		t.Fatalf("terminal=%s\n%s", tr.Terminal(), tr)
	}
	wftest.AssertRoute(t, tr, "RunTests", "CaptureFailure", "Repair", "RunTests", "CaptureFailure", "Repair", "RunTests", "CaptureFailure", "Repair", "RunTests", "CaptureFailure", "GaveUp")
}
