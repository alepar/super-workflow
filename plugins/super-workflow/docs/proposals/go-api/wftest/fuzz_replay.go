package wftest

// Minimize removes actions while preserving the same property failure and
// site. At completion no single action can be deleted from the script. This
// is deletion-minimal, not a claim of a globally shortest path through an
// arbitrary program. The fixed replay budget keeps CI bounded; an incomplete
// reduction is reported explicitly and still retains a reproducing script.
func (r *FuzzResult) Minimize(factory func() *Sim) *FuzzResult {
	return r.minimize(factory, nil)
}

func (r *FuzzResult) minimize(factory func() *Sim, take func() bool) *FuzzResult {
	if r.Finding == nil {
		return r
	}
	best := r
	runs := 0
	for {
		changed := false
		for i := 0; i < len(best.Script); i++ {
			if runs == 64 || (take != nil && !take()) {
				best.ShrinkRuns = runs
				best.ShrinkComplete = false
				return best
			}
			candidate := append([]FuzzAction{}, best.Script[:i]...)
			candidate = append(candidate, best.Script[i+1:]...)
			replay := runFuzz(factory, r.config, candidate, true)
			runs++
			if sameFuzzFinding(r.Finding, replay.Finding) {
				best = replay
				changed = true
				break
			}
		}
		if !changed {
			best.ShrinkRuns = runs
			best.ShrinkComplete = true
			return best
		}
	}
}
func sameFuzzFinding(a, b *FuzzFinding) bool {
	return a != nil && b != nil && a.Rule == b.Rule && a.State == b.State && a.Edge == b.Edge && a.Target == b.Target
}
