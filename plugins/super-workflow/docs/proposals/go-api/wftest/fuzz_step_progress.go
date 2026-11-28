package wftest

import (
	"encoding/json"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State and explicit workflow counters distinguish forward work from repeating
// a completed context. Attempt numbers and sibling activity are not progress.
func stepContext(status wf.RunStatus, instance string) string {
	for _, in := range status.Instances {
		if in.ID == instance {
			counters := map[string]int{}
			for name, value := range in.Counters {
				if value != 0 {
					counters[name] = value
				}
			}
			b, _ := json.Marshal(struct {
				State    string
				Counters map[string]int
			}{in.State, counters})
			return string(b)
		}
	}
	return ""
}

func (f *fuzzRun) progressingAtStepBound(status wf.RunStatus) bool {
	completed := f.completedContexts[f.last.Child]
	if len(completed) == 0 || completed[stepContext(status, f.last.Child)] {
		return false
	}
	for _, in := range status.Instances {
		if in.ID != f.last.Child || (in.Phase != "ready" && in.Phase != "issued") {
			continue
		}
		key := strings.TrimPrefix(f.journal.defs[in.ID]+"/"+in.State, f.sim.def.Name()+"/")
		if state, ok := f.schemas[key]; ok && state.Kind == "agent" {
			return true
		}
	}
	return false
}
