package wf_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	cnFirst  struct{}
	cnSecond struct{}
	cnFin    struct{}
	cnAsk    struct{}
	cnDone   struct{}
	cnInner  struct{}
)

// cnChild is a one-agent child named name whose prompt is prompt.
func cnChild(name, prompt string) wf.Child[chItem, chRes] {
	d := wf.New(name)
	ask := wf.NewAgent[cnAsk, chOut](d)
	done := wf.NewTerminal[cnDone](d, wf.Success)
	d.OnFailure(wf.Blocked, done)
	ask.Spec(wf.PromptText(prompt), wf.Retry[cnAsk](wf.Const(1), ask.To(done, "gave up")))
	toDone := ask.To(done, "asked")
	ask.Route(func(chOut, wf.Ctx) wf.Edge[cnAsk] { return toDone }, toDone)
	wf.Yield(done, func(wf.Ctx) chRes { return chRes{} })
	return wf.Typed[chItem, chRes](d)
}

// cnTwoSubs runs first, then second, each as a Sub of a definition named name.
func cnTwoSubs(name string, first, second wf.Child[chItem, chRes]) *wf.Def {
	d := wf.New(name)
	in := func(wf.Ctx) chItem { return chItem{} }
	a := wf.NewSub[cnFirst](d, first, in)
	b := wf.NewSub[cnSecond](d, second, in)
	fin := wf.NewTerminal[cnFin](d, wf.Success)
	a.Exits(wf.OnExit[cnDone](a.To(b, "first done")))
	b.Exits(wf.OnExit[cnDone](b.To(fin, "second done")))
	return d
}

func cnHasRule(ps []wf.Problem, r wf.RuleID, path string) bool {
	return slices.ContainsFunc(ps, func(p wf.Problem) bool { return p.Rule == r && p.Path == path })
}

// Two differing child definitions under one name are refused: a name
// identifies one definition in status paths and migration shapes.
func TestDuplicateChildNameRefused(t *testing.T) {
	d := cnTwoSubs("dup-children", cnChild("retrieval", "first"), cnChild("retrieval", "second"))
	ps := d.Lint()
	if !cnHasRule(ps, wf.RuleChildNameDuplicate, "dup-children") {
		t.Errorf("lint = %v, want %s at dup-children", ps, wf.RuleChildNameDuplicate)
	}
	h := newHarness(t, d, nil)
	if _, err := h.e.Run(d, nil, nil); err == nil || !strings.Contains(err.Error(), `"retrieval"`) {
		t.Fatalf("Run = %v, want a refusal naming the duplicate child", err)
	}
}

// The same child definition used by two states is one definition: allowed,
// and each Sub runs it.
func TestSharedChildDefinitionAllowed(t *testing.T) {
	kid := cnChild("retrieval", "same")
	d := cnTwoSubs("shared-child", kid, kid)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint = %v, want none", ps)
	}
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if st := h.drive(run, map[string][]string{"retrieval/cnAsk": {`{"OK":true}`, `{"OK":true}`}}); st.Terminal != "cnFin" {
		t.Fatalf("status = %+v, want ended at cnFin", st)
	}
}

// A helper built by calling its constructor twice gives two identical
// definitions: allowed, and each Sub runs its own.
func TestRebuiltIdenticalChildAllowed(t *testing.T) {
	d := cnTwoSubs("rebuilt-child", cnChild("retrieval", "same"), cnChild("retrieval", "same"))
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint = %v, want none", ps)
	}
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if st := h.drive(run, map[string][]string{"retrieval/cnAsk": {`{"OK":true}`, `{"OK":true}`}}); st.Terminal != "cnFin" {
		t.Fatalf("status = %+v, want ended at cnFin", st)
	}
}

// The check covers the whole tree: a nested child that reuses a name another
// branch, or the root, already gives a different definition is refused too.
func TestDuplicateChildNameNested(t *testing.T) {
	nest := func(name string, kid wf.Child[chItem, chRes]) wf.Child[chItem, chRes] {
		d := wf.New(name)
		s := wf.NewSub[cnInner](d, kid, func(wf.Ctx) chItem { return chItem{} })
		done := wf.NewTerminal[cnDone](d, wf.Success)
		s.Exits(wf.OnExit[cnDone](s.To(done, "inner done")))
		wf.Yield(done, func(wf.Ctx) chRes { return chRes{} })
		return wf.Typed[chItem, chRes](d)
	}
	cousins := cnTwoSubs("cousins", nest("left", cnChild("retrieval", "a")), nest("right", cnChild("retrieval", "b")))
	if ps := cousins.Lint(); !cnHasRule(ps, wf.RuleChildNameDuplicate, "cousins") {
		t.Errorf("cousins lint = %v, want %s", ps, wf.RuleChildNameDuplicate)
	}
	root := cnTwoSubs("retrieval", cnChild("retrieval", "a"), cnChild("other", "b"))
	if ps := root.Lint(); !cnHasRule(ps, wf.RuleChildNameDuplicate, "retrieval") {
		t.Errorf("root-name lint = %v, want %s", ps, wf.RuleChildNameDuplicate)
	}
	h := newHarness(t, cousins, nil)
	if _, err := h.e.Run(cousins, nil, nil); err == nil {
		t.Fatal("Run accepted a nested duplicate child name")
	}
}
