package wftest_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/agentless"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/approval"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/broaden"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/checkrepair"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/dispatch"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/escalation"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/fanout"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/gatedchain"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/noprogress"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/orchestrator"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/park"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/resumable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/router"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/stale"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tournament"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/typedexit"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/unavailable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"testing"
)

// Every reserved example gate has an executable delivery scenario beside its definition.
func TestEveryExampleReservedGateHasDeliveryScenario(t *testing.T) {
	expected := map[string]bool{"feature/Approve": false, "feature/Recovery": false, "feature/feature-task/Recovery": false, "approval/Approve": false, "critique/Human": false, "tour/Ship": false}
	defs := []*wf.Def{agentless.Definition(), approval.Definition(), broaden.Definition(), checkrepair.Definition(), critique.Definition(), dispatch.Definition(), escalation.Definition(), fanout.Definition(), feature.Definition(), gatedchain.Definition(), noprogress.Definition(), orchestrator.Definition(), park.Definition(), resumable.Definition(), router.Definition(), stale.Definition(), tour.Definition(), tournament.Definition(), typedexit.Definition(), unavailable.Definition()}
	var walk func(wf.DefView)
	walk = func(def wf.DefView) {
		for _, state := range def.States {
			if state.Reservation != "reserved" {
				continue
			}
			if _, ok := expected[state.Path]; !ok {
				t.Errorf("add an executable reserved-gate delivery scenario: %s", state.Path)
			} else {
				expected[state.Path] = true
			}
		}
		for _, child := range def.Children {
			walk(child)
		}
	}
	for _, def := range defs {
		walk(wf.Inspect(def))
	}
	for path, seen := range expected {
		if !seen {
			t.Errorf("delivery scenario names removed gate: %s", path)
		}
	}
}
