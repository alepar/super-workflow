package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	mlsOut struct {
		Summary string `json:"summary" maxlen:"5"`
	}
	mlsAgent    struct{}
	mlsCompute  struct{}
	mlsSub      struct{}
	mlsDone     struct{}
	mlsStop     struct{}
	mlsChildRun struct{}
	mlsChildEnd struct{}
	mlsMap      struct{}
)

// mlsEcho is a child whose compute returns its input and whose terminal
// yields long.
func mlsEcho(name string, long mlsOut) wf.Child[mlsOut, mlsOut] {
	child := wf.New(name)
	work := wf.NewCompute[mlsChildRun](child, func(c wf.Ctx) (mlsOut, error) { return wf.InputOf[mlsOut](c), nil })
	end := wf.NewTerminal[mlsChildEnd](child, wf.Success)
	toEnd := work.To(end, "echoed")
	work.Route(func(mlsOut, wf.Ctx) wf.Edge[mlsChildRun] { return toEnd }, toEnd)
	wf.Yield(end, func(wf.Ctx) mlsOut { return long })
	return wf.Typed[mlsOut, mlsOut](child)
}

// TestMaxlenBoundsOnlyAgentSubmits: maxlen bounds what an agent submits; it is
// not a storage limit. An agent's over-cap result is refused and retried, while
// the same type over the same cap passes as a compute output, a Map item, a Sub
// input, a child's yield and the run's own yield.
func TestMaxlenBoundsOnlyAgentSubmits(t *testing.T) {
	long := mlsOut{Summary: strings.Repeat("x", 50)}
	d := wf.New("maxlen-submit")
	a := wf.NewAgent[mlsAgent, mlsOut](d)
	c := wf.NewCompute[mlsCompute](d, func(wf.Ctx) (mlsOut, error) { return long, nil })
	m := wf.NewMap[mlsMap, mlsOut, string, mlsOut](d, wf.FromCompute(c, func(o mlsOut) []mlsOut { return []mlsOut{o} }),
		func(o mlsOut) string { return "one" }, mlsEcho("maxlen-item", long))
	s := wf.NewSub[mlsSub, mlsOut, mlsOut](d, mlsEcho("maxlen-child", long), func(wf.Ctx) mlsOut { return long })
	done := wf.NewTerminal[mlsDone](d, wf.Success)
	stop := wf.NewTerminal[mlsStop](d, wf.Stopped)
	a.Spec(wf.PromptText("summarize"), wf.Retry[mlsAgent](wf.Const(2), a.To(stop, "failed")))
	toC := a.To(c, "summarized")
	a.Route(func(mlsOut, wf.Ctx) wf.Edge[mlsAgent] { return toC }, toC)
	toM := c.To(m, "computed")
	c.Route(func(mlsOut, wf.Ctx) wf.Edge[mlsCompute] { return toM }, toM)
	m.Parallel(1).Join(wf.AllSettled(), m.To(s, "met"), m.To(stop, "partial"))
	c.Retry(wf.Retry[mlsCompute](wf.Const(1), c.To(stop, "refused")))
	s.Exits(wf.OnExit[mlsChildEnd](s.To(done, "echoed")))
	wf.Yield(done, func(wf.Ctx) mlsOut { return long })

	tr := wftest.New(d, wftest.WithTB(t)).Run(
		wftest.SubmitAt("mlsAgent", long),
		wftest.SubmitAt("mlsAgent", mlsOut{Summary: "short"}))
	if tr.Terminal() != "mlsDone" || tr.RetriesSpent() != 1 {
		t.Fatalf("terminal %q, %d refusals: want the agent's long summary refused once, then the computed, mapped, child and yielded values accepted:\n%s",
			tr.Terminal(), tr.RetriesSpent(), tr)
	}
}
