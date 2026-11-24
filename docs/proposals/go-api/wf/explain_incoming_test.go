package wf_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"strings"
	"testing"
)

func TestExplainIncomingRoute(t *testing.T) {
	d := wf.New("explain-incoming")
	a := wf.NewAgent[rA, string](d)
	b := wf.NewAgent[rB, string](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	edge := a.To(b, "evidence passed").With(d.Counter("decisions").Inc())
	a.Spec(wf.PromptText("a"), wf.Retry[rA](wf.Const(1), a.To(end, "failed")))
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return edge }, edge)
	done := b.To(end, "done")
	b.Spec(wf.PromptText("b"), wf.Retry[rB](wf.Const(1), done))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `"ok"`)
	in := h.next(run)[0]
	text, err := h.e.(*wf.Engine).Explain(run, in.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "evidence passed") || !strings.Contains(text, "decisions") {
		t.Fatalf("incoming route/effect absent: %s", text)
	}
}

func TestExplainIncomingDoneWhen(t *testing.T) {
	d := wf.New("explain-donewhen")
	a := wf.NewAgent[rA, string](d)
	b := wf.NewAgent[rB, string](d)
	end := wf.NewTerminal[rEnd](d, wf.Success)
	edge := a.To(b, "already satisfied").With(d.Counter("skips").Inc())
	a.Spec(wf.PromptText("a"), wf.Retry[rA](wf.Const(1), a.To(end, "failed")))
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return edge }, edge)
	wf.DoneWhen(a, wf.Observe[bool]("x", "done", nil), "existing evidence")
	done := b.To(end, "done")
	b.Spec(wf.PromptText("b"), wf.Retry[rB](wf.Const(1), done))
	b.Route(func(string, wf.Ctx) wf.Edge[rB] { return done }, done)
	rt := newFakeRuntime()
	rt.set("x", "done", nil, true)
	h := newHarness(t, d, rt)
	run := h.run(d, nil)
	in := h.next(run)[0]
	text, err := h.e.(*wf.Engine).Explain(run, in.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "already satisfied") || !strings.Contains(text, "skips") {
		t.Fatalf("incoming DoneWhen route/effect absent: %s", text)
	}
}
