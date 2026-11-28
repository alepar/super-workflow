package wftest

import (
	"encoding/json"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A faulted run may reach the definition's intentional stopped terminal. A
// truly all-good run still owes success; unresolved holds and halts are not
// terminals. Require both the declaration and this run's actual root end.
func (f *fuzzRun) declaredStoppedAfterFault() bool {
	return f.result.adversarialInjected && f.rootStoppedEnd() != nil
}

// An all-good run may also end at a stopped terminal its definition reached
// through an edge marked Ends(reason): the author declared that end
// intentional, for example an options-guarded hand-off to a person. The
// root's last transition into the terminal must come from a state whose
// every edge to that terminal is a plain edge so marked: the journal records
// the source and target, not the edge. A stop reached any other way (a
// timeout, an exhausted budget or visit cap, a failure-class route, a
// forwarded child stop, an unmarked edge) still fails liveness.
func (f *fuzzRun) endsDeclaredStop() bool {
	terminal := f.rootStoppedEnd()
	if terminal == nil {
		return false
	}
	records := f.result.Trace.Journal()
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		if record.Kind != wf.JournalTransition || record.Run != f.result.Status.Run || record.Instance != "" {
			continue
		}
		var moved wf.TransitionData
		if json.Unmarshal(record.Data, &moved) != nil || moved.To != terminal.Name {
			return false
		}
		marked := false
		for _, s := range wf.Inspect(f.sim.def).States {
			if s.Name != record.State {
				continue
			}
			for _, e := range s.Edges {
				if e.To != terminal.Name {
					continue
				}
				if e.Ends == "" || e.Timeout > 0 || e.Fallback != "" || e.Class != "" || e.Over {
					return false
				}
				marked = true
			}
		}
		return marked
	}
	return false
}

// rootStoppedEnd is the definition's stopped terminal this run's root ended
// at, proven by the root's run_ended record; nil for any other end.
func (f *fuzzRun) rootStoppedEnd() *wf.StateView {
	r := f.result
	if r.Status.Phase != wf.RunEnded || r.Status.Outcome != wf.Stopped || r.Status.Terminal == "" || r.Trace == nil {
		return nil
	}
	var terminal *wf.StateView
	for _, s := range wf.Inspect(f.sim.def).States {
		if s.Name == r.Status.Terminal && s.Kind == "terminal" && s.Outcome == string(wf.Stopped) {
			terminal = &s
			break
		}
	}
	if terminal == nil {
		return nil
	}
	records := r.Trace.Journal()
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		if record.Kind != wf.JournalRunEnded || record.Run != r.Status.Run || record.Instance != "" {
			continue
		}
		var ended wf.RunEndedData
		if json.Unmarshal(record.Data, &ended) == nil && record.State == r.Status.Terminal && ended.Terminal == r.Status.Terminal && ended.Outcome == wf.Stopped {
			return terminal
		}
		return nil
	}
	return nil
}
