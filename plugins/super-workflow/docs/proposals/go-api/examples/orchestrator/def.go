// Package orchestrator is orchestrator-workers with a dynamic task list (lane
// D P4, VISION E8): a lead agent writes the task list, a Map fans out one
// worker per task (no per-task states in the graph), and the list is
// re-queried as workers settle, since workers may add tasks.
package orchestrator

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// State marker types (parent, then child).
type (
	Lead       struct{}
	Fan        struct{}
	Synthesize struct{}
	Done       struct{}
	Failed     struct{}

	Worker     struct{}
	WorkerDone struct{}
	WorkerFail struct{}
)

// Task is one unit of work the lead delegates.
type Task struct{ ID, Brief string }

// Plan is the lead agent's output.
type Plan struct{ Tasks []Task }

// TaskResult is the worker child's typed result.
type TaskResult struct {
	ID      string
	Summary string
}

// WorkerOut is the worker agent's output.
type WorkerOut struct{ Summary string }

func workerChild() wf.Child[Task, TaskResult] {
	d := wf.New("orchestrator-worker")
	w := wf.NewAgent[Worker, WorkerOut](d)
	ok := wf.NewTerminal[WorkerDone](d, wf.Success)
	bad := wf.NewTerminal[WorkerFail](d, wf.Stopped)
	d.OnFailure(wf.Blocked, bad)
	w.Spec(wf.PromptText("Do this task and report: {{item}}", wf.ChildInput()),
		wf.Retry[Worker](wf.Const(2), w.To(bad, "worker gave up")))
	wNext := w.To(ok, "task done")
	w.Route(func(WorkerOut, wf.Ctx) wf.Edge[Worker] { return wNext }, wNext)
	wf.Yield(ok, func(wf.Ctx) TaskResult { return TaskResult{} })
	wf.Yield(bad, func(wf.Ctx) TaskResult { return TaskResult{} })
	return wf.Typed[Task, TaskResult](d)
}

// Definition builds the orchestrator.
func Definition() *wf.Def {
	d := wf.New("orchestrator")
	lead := wf.NewAgent[Lead, Plan](d)
	fan := wf.NewMap[Fan, Task, string, TaskResult](d,
		wf.FromOutput(lead, func(p Plan) []Task { return p.Tasks }),
		func(t Task) string { return t.ID },
		workerChild())
	synth := wf.NewCompute[Synthesize, int](d, func(c wf.Ctx) (int, error) {
		n := 0
		for _, in := range wf.Joined(c, fan).Items {
			if !in.BudgetStopped {
				n++
			}
		}
		return n, nil
	})
	done := wf.NewTerminal[Done](d, wf.Success)
	failed := wf.NewTerminal[Failed](d, wf.Stopped)
	d.OnFailure(wf.Blocked, failed)

	lead.Spec(wf.PromptText("Break the goal into independent tasks."),
		wf.Retry[Lead](wf.Const(2), lead.To(failed, "no usable task list")))
	leadNext := lead.To(fan, "tasks listed")
	lead.Route(func(Plan, wf.Ctx) wf.Edge[Lead] { return leadNext }, leadNext)

	fan.Requery().Join(wf.AllOK(),
		fan.To(synth, "all workers succeeded"),
		fan.To(failed, "a worker failed").Ends("Synthesis requires every worker to succeed"))
	synthNext := synth.To(done, "synthesized")
	synth.Route(func(int, wf.Ctx) wf.Edge[Synthesize] { return synthNext }, synthNext)
	return d
}
