package wftest

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// Manual settlement of an unresolved effect is the engine's human boundary
// when the definition has no Uncertain route. A transient check failure or a
// waiting/held unit does not establish that boundary.
func (f *fuzzRun) manualUncertainBoundary(in wf.Instruction) bool {
	if in.Kind != wf.InstructionManualVerification || in.Effect <= 0 || in.Transient {
		return false
	}
	state, ok := f.schemas[f.stateKey(in)]
	if !ok {
		return false
	}
	for _, edge := range state.Edges {
		if edge.Class == "uncertain" {
			return false
		}
	}
	return true
}
