package wftest

import (
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A faulted run may reach the definition's intentional stopped terminal. A
// truly all-good run still owes success; unresolved holds and halts are not
// terminals. Require both the declaration and this run's actual root end.
func (f *fuzzRun) declaredStoppedAfterFault() bool {
	r := f.result
	if !r.adversarialInjected || r.Status.Phase != wf.RunEnded || r.Status.Outcome != wf.Stopped || r.Status.Terminal == "" || r.Trace == nil {
		return false
	}
	declared := false
	for _, s := range wf.Inspect(f.sim.def).States {
		if s.Name == r.Status.Terminal && s.Kind == "terminal" && s.Outcome == string(wf.Stopped) {
			declared = true
			break
		}
	}
	if !declared {
		return false
	}
	records := r.Trace.Journal()
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		if record.Kind != wf.JournalRunEnded || record.Run != r.Status.Run || record.Instance != "" {
			continue
		}
		var ended wf.RunEndedData
		return json.Unmarshal(record.Data, &ended) == nil && record.State == r.Status.Terminal && ended.Terminal == r.Status.Terminal && ended.Outcome == wf.Stopped
	}
	return false
}
