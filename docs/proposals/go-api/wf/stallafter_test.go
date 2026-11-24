package wf_test

import (
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// saDef is lsDef with a StallAfter(2m) hint on the agent.
func saDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[lsAsk, clAnswer](d)
	done := wf.NewTerminal[lsDone](d, wf.Success)
	gaveUp := wf.NewTerminal[lsGaveUp](d, wf.Stopped)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[lsAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	a.Dispatch(wf.StallAfter(2 * time.Minute))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[lsAsk] { return next }, next)
	return d
}

// TestStallAfterThreshold: a state's StallAfter replaces the 20m class
// default: stalled just past 2m of quiet, not just inside it.
func TestStallAfterThreshold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quiet time.Duration
		want  int
	}{
		{"inside", 119 * time.Second, 0},
		{"past", 121 * time.Second, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := saDef("sa-" + tc.name)
			m := newTranscripts()
			h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
			run, ins := handOneUnit(t, h, d)
			lvRunning(t, h, m, run, ins)

			h.now = h.now.Add(tc.quiet)
			h.handOut(run, 0)
			if n := countStatus(heartbeats(t, h, run), "stalled"); n != tc.want {
				t.Fatalf("stalled heartbeats after %s quiet = %d, want %d", tc.quiet, n, tc.want)
			}
		})
	}
}
