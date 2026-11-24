package wf_test

import (
	"testing"

	harnesspkg "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	_ "github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// lateAdapter is registered from this test package's init function, which runs
// after the wf package's initialization, as a linking binary's would.
type lateAdapter struct{ harnesspkg.Generic }

func (lateAdapter) Name() string { return "wf-init-probe" }

var registerErr = harnesspkg.Register(lateAdapter{})

// The wf package must not read the harness registry while it initializes: a
// binary linking the engine registers its adapters from init, after wf's.
func TestRegisterAfterWfInitialization(t *testing.T) {
	if registerErr != nil {
		t.Fatalf("Register after wf initialized: %v", registerErr)
	}
}
