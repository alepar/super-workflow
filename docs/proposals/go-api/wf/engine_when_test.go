package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	wOpts struct{ Skip bool }
	wPick struct{}
	wFin  struct{}
	wAlt  struct{}
)

// guardSubDef is a Sub over chChild with a route-around edge to wAlt guarded by
// Skip=true; the child's chOK exit goes to wFin.
func guardSubDef() *wf.Def {
	d := wf.New("guard-sub")
	opts := wf.Options[wOpts](d)
	sub := wf.NewSub[chSubRun, chItem, chRes](d, chChild(), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[wFin](d, wf.Success)
	alt := wf.NewTerminal[wAlt](d, wf.Success)
	sub.To(alt, "skip: route around").When(wf.Field(opts, func(o *wOpts) *bool { return &o.Skip }).Is(true))
	sub.Exits(wf.OnExit[chOK](sub.To(fin, "ok")), wf.OnExit[chBad](sub.To(fin, "bad")))
	return d
}

func TestGuardRoutesAroundSub(t *testing.T) {
	d := guardSubDef()
	if ps := d.Lint(); len(ps) > 0 {
		t.Fatalf("lint: %v", ps)
	}
	h := newHarness(t, d, nil)
	run, err := h.e.Run(d, nil, wOpts{Skip: true})
	if err != nil {
		t.Fatal(err)
	}
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions %v: a met guard must start no child", ins)
	}
	if n := len(h.kind(run, wf.JournalChildSpawned)); n != 0 {
		t.Errorf("%d child_spawned records under a met skip guard, want 0", n)
	}
	statusOf(t, h.status(run), wf.RunEnded, "wAlt")
}

func TestGuardNotMetRunsTheSub(t *testing.T) {
	d := guardSubDef()
	h := newHarness(t, d, nil)
	run, err := h.e.Run(d, nil, wOpts{})
	if err != nil {
		t.Fatal(err)
	}
	st := h.drive(run, map[string][]string{"c-item/chWork": {`{"OK":true}`}})
	statusOf(t, st, wf.RunEnded, "wFin")
	if n := len(h.kind(run, wf.JournalChildSpawned)); n != 1 {
		t.Errorf("%d child_spawned records, want 1", n)
	}
}

// guardRouteDef is a compute whose Route returns the Skip-guarded edge
// regardless of the options.
func guardRouteDef() *wf.Def {
	d := wf.New("guard-route")
	opts := wf.Options[wOpts](d)
	skip := wf.Field(opts, func(o *wOpts) *bool { return &o.Skip }).Is(true)
	pick := wf.NewCompute[wPick, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	alt := wf.NewTerminal[wAlt](d, wf.Success)
	fin := wf.NewTerminal[wFin](d, wf.Success)
	toAlt := pick.To(alt, "skip").When(skip)
	toFin := pick.To(fin, "no skip").When(wf.Not(skip))
	pick.Route(func(bool, wf.Ctx) wf.Edge[wPick] { return toAlt }, toAlt, toFin)
	return d
}

func TestRouteGuardNotHeldIsNotTaken(t *testing.T) {
	d := guardRouteDef()
	h := newHarness(t, d, nil)
	run, err := h.e.Run(d, nil, wOpts{})
	if err != nil {
		t.Fatal(err)
	}
	h.next(run)
	st := h.status(run)
	if st.Terminal == "wAlt" {
		t.Fatal("the route took an edge whose guard does not hold")
	}
	var why string
	for _, r := range h.kind(run, wf.JournalRefused) {
		why += string(r.Data)
	}
	if !strings.Contains(why, "does not hold") {
		t.Errorf("refusals %q do not name the unheld guard", why)
	}
}

func TestRouteGuardHeldIsTaken(t *testing.T) {
	d := guardRouteDef()
	h := newHarness(t, d, nil)
	run, err := h.e.Run(d, nil, wOpts{Skip: true})
	if err != nil {
		t.Fatal(err)
	}
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "wAlt")
}
