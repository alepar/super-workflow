package wf

import (
	"testing"
	"time"
)

// endSettleDeadlines are the end settle deadlines c lists.
func endSettleDeadlines(c *cycle) []deadline {
	var out []deadline
	for _, d := range c.deadlines(unitT0) {
		if d.Kind == deadlineEndSettle {
			out = append(out, d)
		}
	}
	return out
}

// The end settle deadline and loseEndedChildren share one guard (endWatched):
// a unit the reconcile skips lists none, or a blocking wait would wake for it
// again and again.
func TestEndSettleDeadlineFollowsTheSharedGuard(t *testing.T) {
	r := newIssuedRun(t)
	name := r.handOut()
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Harness: "pi", Session: "s", Agent: "wf-ag", Transcript: "/t", Expires: unitT0.Add(time.Hour)})
	due := unitT0.Add(7 * time.Second)

	c := dlCycle(t, r.recs, r.def)
	if got := endSettleDeadlines(c); len(got) != 0 {
		t.Fatalf("no pending settle, yet deadlines %+v", got)
	}
	c.e.endPending = map[string]endPend{"/t": {mt: unitT0, due: due}}
	got := endSettleDeadlines(c)
	if len(got) != 1 || !got[0].Due.Equal(due) || got[0].Unit != name {
		t.Fatalf("end settle deadlines = %+v, want one for %s at %v", got, name, due)
	}

	in := c.st.insts[""]
	in.Unit.Recovery = &RecoveryOfferedData{Unit: name}
	if got := endSettleDeadlines(c); len(got) != 0 {
		t.Errorf("a unit with an open recovery lists %+v", got)
	}
	in.Unit.Recovery = nil
	in.Settled = true
	if got := endSettleDeadlines(c); len(got) != 0 {
		t.Errorf("a settled unit lists %+v", got)
	}
	in.Settled = false
	c.e.endPending = map[string]endPend{"/other": {due: due}}
	if got := endSettleDeadlines(c); len(got) != 0 {
		t.Errorf("a pending entry for another transcript lists %+v", got)
	}
}
