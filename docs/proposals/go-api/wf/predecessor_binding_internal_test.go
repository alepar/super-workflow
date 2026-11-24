package wf

import "testing"

func TestPredecessorBinding(t *testing.T) {
	for _, pred := range []RunID{"", "r1"} {
		c := &cycle{run: "r1.2", st: &runState{started: &RunStartedData{Predecessor: pred}}}
		got, ok := c.scope(nil).lookup(predecessorName)
		if !ok || got != string(pred) {
			t.Errorf("{predecessor} with predecessor %q = %q, %v; want it bound to %q", pred, got, ok, pred)
		}
	}
}
