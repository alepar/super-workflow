package wf_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State markers of the nested Map fixture: an outer Map whose child holds an
// inner Map named Panel.
type (
	Outer   struct{}
	Panel   struct{}
	pnDone  struct{}
	pnShort struct{}
)

// nestedPanelDef is an outer Map over items 1 and 2 keyed by outerKey; each
// outer child runs an inner Map, Panel, with the one key "s1".
func nestedPanelDef(outerKey func(chItem) string) *wf.Def {
	cd := wf.New("panel-child")
	pm := wf.NewMap[Panel, chItem, string, chRes](cd, chItems(1), func(chItem) string { return "s1" }, chChild())
	done := wf.NewTerminal[pnDone](cd, wf.Success)
	short := wf.NewTerminal[pnShort](cd, wf.Stopped)
	pm.Parallel(1).Join(wf.AllOK(), pm.To(done, "met"), pm.To(short, "partial"))
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(done, res)
	wf.Yield(short, res)

	d := wf.New("nested-panel")
	om := wf.NewMap[Outer, chItem, string, chRes](d, chItems(1, 2), outerKey, wf.Typed[chItem, chRes](cd))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	om.Parallel(2).Join(wf.AllOK(), om.To(merged, "met"), om.To(partial, "partial"))
	return d
}

// An outer key that spells an inner instance id does not take that id: the
// outer keys "c1" and "c1]/Panel[s1" and the inner key "s1" make three
// distinct children, and the run settles (sw-ivuk: the second outer child and
// the first child's inner Panel child shared an id and Next never returned).
func TestMapKeyWithIDDelimitersKeepsIDsDistinct(t *testing.T) {
	keys := map[int]string{1: "c1", 2: "c1]/Panel[s1"}
	d := nestedPanelDef(func(i chItem) string { return keys[i.N] })
	h := newHarness(t, d, newWorld())
	run := h.run(d, nil)

	done := make(chan int, 1)
	go func() { done <- answerAll(h, run, okOut) }()
	select {
	case n := <-done:
		if n != 2 {
			t.Errorf("answered %d instructions, want 2 (one per inner Panel child)", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not settle: Next did not return")
	}

	ids := map[string]bool{}
	for _, r := range h.kind(run, wf.JournalChildSpawned) {
		var sp wf.ChildSpawnedData
		if err := json.Unmarshal(r.Data, &sp); err != nil {
			t.Fatal(err)
		}
		if ids[sp.ID] {
			t.Errorf("child id %q spawned twice", sp.ID)
		}
		ids[sp.ID] = true
	}
	if len(ids) != 4 {
		t.Errorf("spawned ids = %v, want 4 distinct (2 outer, 2 inner)", ids)
	}
	for id := range ids {
		if strings.Count(id, "/") > 1 {
			t.Errorf("id %q has more than two segments: a key's delimiter was not escaped", id)
		}
	}
	if acceptedOf(h, run, "Outer") == nil {
		t.Fatal("outer Map did not join")
	}
}

// An ordinary key keeps the id it always had, so journals and artifact paths
// written before the escape still resume, replay and fork.
func TestMapKeyOrdinaryIDUnchanged(t *testing.T) {
	d := nestedPanelDef(func(i chItem) string { return map[int]string{1: "c1", 2: "c-2.x_y z"}[i.N] })
	h := newHarness(t, d, newWorld())
	run := h.run(d, nil)
	answerAll(h, run, okOut)
	ids := map[string]bool{}
	for _, r := range h.kind(run, wf.JournalChildSpawned) {
		var sp wf.ChildSpawnedData
		if err := json.Unmarshal(r.Data, &sp); err != nil {
			t.Fatal(err)
		}
		ids[sp.ID] = true
	}
	for _, want := range []string{"Outer[c1]", "Outer[c-2.x_y z]", "Outer[c1]/Panel[s1]", "Outer[c-2.x_y z]/Panel[s1]"} {
		if !ids[want] {
			t.Errorf("no child %q; spawned %v", want, ids)
		}
	}
}
