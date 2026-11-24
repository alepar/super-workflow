package tour_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour/checker"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func TestTourChecker(t *testing.T) {
	factory, cfg := checker.Scenario()
	report := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: cfg})
	if len(report.Problems) != 0 {
		t.Fatalf("%s", report)
	}
}
