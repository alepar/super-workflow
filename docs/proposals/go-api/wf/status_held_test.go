package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// heldRows returns the waiting units of st that carry the given note.
func heldRows(st wf.RunStatus, note string) []wf.UnitStatus {
	var out []wf.UnitStatus
	for _, u := range st.Units {
		if u.State == "waiting" && u.Note == note {
			out = append(out, u)
		}
	}
	return out
}

func TestStatusListsHeldMapItems(t *testing.T) {
	d := fanDef("st-held", chItems(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 2 {
		t.Fatalf("instructions = %d, want 2 (Parallel is 2)", len(ins))
	}
	st := h.status(run)
	held := heldRows(st, "held")
	if len(held) != 8 {
		t.Fatalf("held units = %d, want 8 (units: %+v)", len(held), st.Units)
	}
	if len(st.Units) != 10 {
		t.Fatalf("units = %d, want 2 children and 8 held", len(st.Units))
	}
	if st.Progress.Total != 10 {
		t.Errorf("Progress.Total = %d, want 10", st.Progress.Total)
	}
	if got := heldRows(st, "deps"); len(got) != 0 {
		t.Errorf("deps rows = %d, want 0: nothing reports WaitingOnDeps", len(got))
	}
	if st.Progress.WaitingOnDeps != 0 {
		t.Errorf("Progress.WaitingOnDeps = %d, want 0: held is not a dependency wait", st.Progress.WaitingOnDeps)
	}
	if st.Binding == wf.BindingGraph {
		t.Errorf("Binding = %q, want not graph-bound for items held by Parallel", st.Binding)
	}
	if !strings.Contains(wf.RenderStatus(st), "waiting") {
		t.Errorf("RenderStatus omits the held units:\n%s", wf.RenderStatus(st))
	}
}

func TestStatusHeldItemsShrinkAsChildrenSpawn(t *testing.T) {
	d := fanDef("st-held2", chItems(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("instructions = %d, want 2", len(ins))
	}
	h.submit(run, ins[0], `{"OK":true}`)
	if n := len(heldRows(h.status(run), "held")); n != 7 {
		t.Fatalf("held after one child settled and the next spawned = %d, want 7", n)
	}
}
