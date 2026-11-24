package wf_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/agentless"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/approval"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/broaden"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/checkrepair"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/dispatch"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/escalation"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/fanout"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/gatedchain"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/noprogress"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/orchestrator"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/park"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/resumable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/router"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/stale"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tournament"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/typedexit"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/unavailable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// TestDepthsOfExamples pins every example's critical depths. The values are
// the ones the earlier longest-simple-path walk computed: the acyclic examples
// must not move, and the four looping ones (checkrepair, critique, stale,
// typedexit: one loop each, whose every state reaches the whole loop) agree
// with it too.
func TestDepthsOfExamples(t *testing.T) {
	for _, tc := range []struct {
		d    *wf.Def
		want string
	}{
		{agentless.Definition(), "Fixed:0 Localize:2 NoFix:0 Rank:0 Repair:1 Select:0 Validate:0"},
		{approval.Definition(), "Approve:1 Done:0 Draft:2 Execute:1 Expired:0 Failed:0 Rejected:0"},
		{broaden.Definition(), "Accepted:0 Answer:2 AnswerWider:1 Escalated:0"},
		{checkrepair.Definition(), "CaptureFailure:1 GaveUp:0 Green:0 Repair:1 RunTests:1"},
		{critique.Definition(), "Accepted:0 Critique:2 Draft:2 Dropped:0 Human:0"},
		{dispatch.Definition(), "Fan:1 Merged:0 Partial:0"},
		{escalation.Definition(), "Cheap:2 Done:0 GaveUp:0 Strong:1"},
		{fanout.Definition(), "Fan:1 Merged:0 Partial:0"},
		{gatedchain.Definition(), "Done:0 Draft:2 Expand:1 SchemaRejected:0 Vet:1 VetRejected:0"},
		{noprogress.Definition(), "Fix:1 Fixed:0 GaveUp:0 StalledAt:0"},
		{orchestrator.Definition(), "Done:0 Failed:0 Fan:1 Lead:2 Synthesize:0"},
		{park.Definition(), "Held:0 Release:0 Shipped:0"},
		{resumable.Definition(), "Done:0 Failed:0 Fetch:1 Publish:1 Transform:1 Verify:1"},
		{router.Definition(), "Bug:0 Classify:1 Feature:0 Question:0 Unclear:0"},
		{stale.Definition(), "AwaitHead:1 GaveUp:0 Review:1 Reviewed:0 TimedOut:0"},
		{tournament.Definition(), "Decided:0 Judges:1 NoQuorum:0"},
		{typedexit.Definition(), "BlockedStop:0 Done:0 FormatStop:0 LimitsStop:0 Next:1 Work:1"},
		{unavailable.Definition(), "NotConverged:0 ToolBlocked:0 Triage:1 TriageDone:0"},
	} {
		e := wf.NewEngine(wf.MemJournal(), nil, wf.EngineOptions{Defs: []*wf.Def{tc.d}})
		got := wf.Depths(e, tc.d)
		var parts []string
		for k, n := range got {
			parts = append(parts, fmt.Sprintf("%s:%d", k, n))
		}
		sort.Strings(parts)
		if s := strings.Join(parts, " "); s != tc.want {
			t.Errorf("depths of %s = %s, want %s", tc.d.Name(), s, tc.want)
		}
	}
}
