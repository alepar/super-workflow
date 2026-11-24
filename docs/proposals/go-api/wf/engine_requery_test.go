package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// requeryDef is a Requery Map over the kv.items observation, up to five
// children at a time.
func requeryDef(name string) *wf.Def {
	src := wf.FromObservation(wf.Observe[[]chItem]("kv", "items", nil))
	return fanDef(name, src, 5, wf.AllOK(), func(m *wf.Map[chFan, chItem, string, chRes]) { m.Requery() })
}

func setItems(w *world, ns ...int) {
	var xs []chItem
	for _, n := range ns {
		xs = append(xs, chItem{N: n})
	}
	w.set("kv", "items", nil, xs)
}

// withdrawnOf decodes the withdrawn records of the run with the instance each
// was journaled on.
func withdrawnOf(t *testing.T, h *harness, run wf.RunID) map[string]wf.WithdrawnData {
	t.Helper()
	out := map[string]wf.WithdrawnData{}
	for _, r := range h.kind(run, wf.JournalWithdrawn) {
		var d wf.WithdrawnData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out[r.Instance] = d
	}
	return out
}

// settledAs maps child id to its child_settled payload.
func settledAs(t *testing.T, h *harness, run wf.RunID) map[string]wf.ChildSettledData {
	t.Helper()
	out := map[string]wf.ChildSettledData{}
	for _, r := range h.kind(run, wf.JournalChildSettled) {
		var d wf.ChildSettledData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out[d.ID] = d
	}
	return out
}

func TestRequeryAtHandOutAddsAndWithdraws(t *testing.T) {
	d := requeryDef("rq-add")
	w := newWorld()
	setItems(w, 1, 2)
	h := newHarness(t, d, w)
	run := h.run(d, nil)

	first := h.handOut(run, 1)
	if got := childIDs(first); len(got) != 1 || got[0] != "chFan[1]" {
		t.Fatalf("HandOut(1) = %v, want chFan[1]", got)
	}

	setItems(w, 1, 3)
	second := h.handOut(run, 5)
	if got := childIDs(second); len(got) != 1 || got[0] != "chFan[3]" {
		t.Fatalf("HandOut(5) after the source changed = %v, want only chFan[3] (chFan[2] left, chFan[1] is out)", got)
	}
	wd := withdrawnOf(t, h, run)
	if len(wd) != 1 || wd["chFan[2]"].Name != "chFan/2/chWork#1" || wd["chFan[2]"].Reason != "item left the source" {
		t.Fatalf("withdrawn = %+v, want exactly chFan[2] under chFan/2/chWork#1", wd)
	}
	st := settledAs(t, h, run)
	if s, ok := st["chFan[2]"]; !ok || !s.Cancelled {
		t.Fatalf("child_settled = %+v, want chFan[2] Cancelled", st)
	}
	if _, ok := st["chFan[1]"]; ok {
		t.Fatalf("chFan[1], already handed out and still in the source, was settled")
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != 2 {
		t.Fatalf("handed_out records = %d, want 2 (chFan[1], chFan[3])", n)
	}
}

func TestHandedUnitWithdrawn(t *testing.T) {
	d := requeryDef("rq-handed")
	w := newWorld()
	setItems(w, 1, 2)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.handOut(run, 5)
	if len(ins) != 2 {
		t.Fatalf("HandOut(5) = %v, want both", childIDs(ins))
	}
	var gone wf.Instruction
	for _, in := range ins {
		if in.Child == "chFan[2]" {
			gone = in
		}
	}

	setItems(w, 1)
	if more := h.handOut(run, 5); len(more) != 0 {
		t.Fatalf("second HandOut = %v, want nothing", childIDs(more))
	}
	wd := withdrawnOf(t, h, run)
	if len(wd) != 1 || wd["chFan[2]"].Name != gone.Name {
		t.Fatalf("withdrawn = %+v, want the handed name %s", wd, gone.Name)
	}
	err := h.e.Started(run, gone.Name, claim("a"))
	if !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Started on a withdrawn name = %v, want ErrRejected", err)
	}
}

func TestStartedUnitNotWithdrawn(t *testing.T) {
	d := requeryDef("rq-started")
	w := newWorld()
	setItems(w, 1, 2)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	var started wf.Instruction
	for _, in := range h.handOut(run, 5) {
		if in.Child == "chFan[2]" {
			started = in
		}
	}
	if err := h.e.Started(run, started.Name, claim("a")); err != nil {
		t.Fatal(err)
	}

	setItems(w) // both items left the source
	h.handOut(run, 5)
	wd := withdrawnOf(t, h, run)
	if _, ok := wd["chFan[2]"]; ok || len(wd) != 1 || wd["chFan[1]"].Name == "" {
		t.Fatalf("withdrawn = %+v, want only the unstarted chFan[1]", wd)
	}

	res, err := h.e.Done(run, started.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
	if err != nil {
		t.Fatalf("Done on the started unit whose item left: %v", err)
	}
	if res.Class != wf.Accepted {
		t.Fatalf("Done result = %+v, want Accepted", res)
	}
	h.handOut(run, 5)
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
}

func TestRequeryAfterDone(t *testing.T) {
	d := requeryDef("rq-done")
	w := newWorld()
	setItems(w, 1)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.handOut(run, 5)
	if len(ins) != 1 {
		t.Fatalf("HandOut = %v", childIDs(ins))
	}
	if err := h.e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	setItems(w, 1, 2)
	if _, err := h.e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.kind(run, wf.JournalChildSpawned)); n != 2 {
		t.Fatalf("child_spawned after the done = %d, want 2: the done re-read the source", n)
	}
}

func TestRequeryRefoldsSame(t *testing.T) {
	d := requeryDef("rq-refold")
	w := newWorld()
	setItems(w, 1, 2)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.handOut(run, 1)
	setItems(w, 1, 3)
	h.handOut(run, 5)
	before := h.status(run)

	// A second engine over the same journal folds it from scratch, with the
	// source now different: the journaled read, not a new one, decides.
	setItems(w, 9)
	again := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
	after, err := again.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("refolded status differs:\nbefore %+v\nafter  %+v", before, after)
	}
}

// chTwoStage is a two-agent child, as super-code's task: implement (chWork),
// then review (chStep), then done.
func chTwoStage() wf.Child[chItem, chRes] {
	d := wf.New("c-two")
	impl := wf.NewAgent[chWork, chOut](d)
	review := wf.NewAgent[chStep, chOut](d)
	ok := wf.NewTerminal[chOK](d, wf.Success)
	bad := wf.NewTerminal[chBad](d, wf.Stopped)
	impl.Spec(wf.PromptText("implement {{item}}", wf.ChildInput()), wf.Retry[chWork](wf.Const(1), impl.To(bad, "gave up")))
	review.Spec(wf.PromptText("review {{item}}", wf.ChildInput()), wf.Retry[chStep](wf.Const(1), review.To(bad, "gave up")))
	toReview, toOK := impl.To(review, "implemented"), review.To(ok, "clean")
	impl.Route(func(chOut, wf.Ctx) wf.Edge[chWork] { return toReview }, toReview)
	review.Route(func(chOut, wf.Ctx) wf.Edge[chStep] { return toOK }, toOK)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	return wf.Typed[chItem, chRes](d)
}

// The e2e shape of item 64: a task's implement unit ran and finished, its
// review awaits hand-out, and its item leaves the requeried source (its bead
// went in_progress when the implementer claimed it). A child that has run a
// unit is never withdrawn: its review is still handed out and the task
// finishes. An item whose child never ran a unit is still withdrawn.
func TestRequeryKeepsAChildThatRanAUnit(t *testing.T) {
	d := wf.New("rq-two")
	src := wf.FromObservation(wf.Observe[[]chItem]("kv", "items", nil))
	m := wf.NewMap[chFan, chItem, string, chRes](d, src, func(i chItem) string { return fmt.Sprint(i.N) }, chTwoStage())
	m.Parallel(5).Requery().Join(wf.AllSettled(), m.To(wf.NewTerminal[chMerged](d, wf.Success), "met"), m.To(wf.NewTerminal[chPartial](d, wf.Stopped), "partial"))
	w := newWorld()
	setItems(w, 1, 3)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	var impl wf.Instruction
	for _, in := range h.handOut(run, 1) {
		impl = in
	}
	if impl.Child != "chFan[1]" {
		t.Fatalf("first hand-out %+v, want chFan[1]", impl)
	}
	if err := h.e.Started(run, impl.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, impl.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	setItems(w) // item 1's bead was claimed and item 3's closed elsewhere: both left
	var review wf.Instruction
	for _, in := range h.handOut(run, 5) {
		if in.Child == "chFan[1]" {
			review = in
		}
	}
	wd := withdrawnOf(t, h, run)
	if _, ok := wd["chFan[1]"]; ok {
		t.Fatalf("chFan[1], whose implement unit ran, was withdrawn: %+v", wd)
	}
	if _, ok := wd["chFan[3]"]; !ok {
		t.Errorf("chFan[3], which never ran a unit, was not withdrawn: %+v", wd)
	}
	if review.Name != "chFan/1/chStep#1" {
		t.Fatalf("the review of chFan[1] was not handed out: %+v", review)
	}
	if err := h.e.Started(run, review.Name, claim("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, review.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if s := settledAs(t, h, run)["chFan[1]"]; s.Cancelled || s.Terminal != "chOK" {
		t.Errorf("chFan[1] settled %+v, want chOK, not cancelled", s)
	}
}

// unitChildren is the children of the unit instructions in ins.
func unitChildren(ins []wf.Instruction) []string {
	var ids []string
	for _, in := range ins {
		if in.Child != "" {
			ids = append(ids, in.Child)
		}
	}
	return ids
}

// doneAs starts and finishes the unit of ins with OK ok.
func doneAs(t *testing.T, h *harness, run wf.RunID, ins wf.Instruction, ok bool) {
	t.Helper()
	if err := h.e.Started(run, ins.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, ins.Name, wf.Submission{Output: json.RawMessage(fmt.Sprintf(`{"OK":%t}`, ok))}); err != nil {
		t.Fatal(err)
	}
}

// Item 96: a task held on a blocker settles in the round and its bead leaves
// bd ready; the blocker closes while a sibling still runs, and the bead is
// ready again. The next hand-out re-admits the key as a new child, in the
// same round, and the join counts the re-admitted child, not the held one.
func TestRequeryReadmitsAKeyThatLeftAndReturned(t *testing.T) {
	d := requeryDef("rq-readmit")
	w := newWorld()
	setItems(w, 1, 2)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	by := map[string]wf.Instruction{}
	for _, in := range h.handOut(run, 5) {
		by[in.Child] = in
	}
	if err := h.e.Started(run, by["chFan[2]"].Name, claim("b")); err != nil {
		t.Fatal(err)
	}
	setItems(w, 2) // item 1 is held: its bead is blocked by the blocker bead
	doneAs(t, h, run, by["chFan[1]"], false)
	if got := unitChildren(h.handOut(run, 5)); len(got) != 0 {
		t.Fatalf("hand-out while item 1 is held = %v, want none", got)
	}

	setItems(w, 1, 2) // the blocker bead closed: item 1 is ready again
	got := h.handOut(run, 5)
	if ids := unitChildren(got); len(ids) != 1 || ids[0] != "chFan[1]~2" {
		t.Fatalf("hand-out after the blocker closed = %v, want [chFan[1]~2] while chFan[2] runs", ids)
	}
	doneAs(t, h, run, got[0], true)
	if _, err := h.e.Done(run, by["chFan[2]"].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if st := h.status(run); st.Phase != wf.RunEnded || st.Terminal != "chMerged" {
		t.Fatalf("run = %s at %q, want ended at chMerged: the re-admitted child replaces the held one", st.Phase, st.Terminal)
	}
}

// A failed key that never left the source is not re-admitted: the failure
// stands and the join decides on it.
func TestRequeryDoesNotReadmitAKeyThatStayed(t *testing.T) {
	d := requeryDef("rq-stayed")
	w := newWorld()
	setItems(w, 1)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.handOut(run, 5)
	doneAs(t, h, run, ins[0], false)
	if got := unitChildren(h.handOut(run, 5)); len(got) != 0 {
		t.Fatalf("hand-out = %v, want none", got)
	}
	if st := h.status(run); st.Phase != wf.RunEnded || st.Terminal != "chPartial" {
		t.Fatalf("run = %s at %q, want ended at chPartial", st.Phase, st.Terminal)
	}
}

// Item 96 through the coordinator's bare next: Engine.Next re-reads a Requery
// Map's source as HandOut does, so a key back in the source is offered by the
// next call that asks for work, not only by a counted hand-out.
func TestNextRequeriesLikeHandOut(t *testing.T) {
	d := requeryDef("rq-next")
	w := newWorld()
	setItems(w, 1)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins, err := h.e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := unitChildren(ins); len(got) != 1 || got[0] != "chFan[1]" {
		t.Fatalf("Next = %v, want chFan[1]", got)
	}
	setItems(w, 1, 2)
	ins, err = h.e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := unitChildren(ins); !slices.Contains(got, "chFan[2]") {
		t.Fatalf("Next after the source grew = %v, want chFan[2] offered", got)
	}
}
