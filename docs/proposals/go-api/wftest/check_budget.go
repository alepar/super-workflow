package wftest

// checkRunBudget is shared by discovery, search and reduction. It records only
// work actually refused, so spending the last run is not itself exhaustion.
type checkRunBudget struct {
	max, used int
	skipped   []string
	seen      map[string]bool
	// tag names the variant on a refused target; nil leaves it as is.
	tag func(string) string
}

func (b *checkRunBudget) take(target string) bool {
	if b == nil {
		return true
	}
	if b.max > 0 && b.used >= b.max {
		b.skip(target)
		return false
	}
	b.used++
	return true
}

func (b *checkRunBudget) skip(target string) {
	if b == nil || b.seen[target] {
		return
	}
	if b.seen == nil {
		b.seen = map[string]bool{}
	}
	b.seen[target] = true
	if b.tag != nil {
		target = b.tag(target)
	}
	b.skipped = append(b.skipped, target)
}

func (cfg CalibrationConfig) run(factory func() *Sim, target string, config FuzzConfig) *FuzzResult {
	result := fuzz(factory, config, func() bool { return cfg.budget.take(target) })
	if result != nil && result.Finding == nil && result.BudgetReason != "" {
		cfg.budget.skip(target + ": " + result.BudgetReason)
	}
	return result
}
