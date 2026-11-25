package wftest

// Spawned instances earn independent work budgets. Their parent still pays for
// every spawn and settlement, so a spawn cycle cannot replenish its own budget.
func (j *fuzzJournal) instances() int {
	if len(j.defs) == 0 {
		return 1
	}
	return len(j.defs)
}

type fuzzStepBudget struct {
	total, good map[string]int
}

func (b *fuzzStepBudget) take(instance string, limit, goodLimit int, good bool) bool {
	if b.total == nil {
		b.total = map[string]int{}
		b.good = map[string]int{}
	}
	if b.total[instance] >= limit || (good && b.good[instance] >= goodLimit) {
		return false
	}
	b.total[instance]++
	if good {
		b.good[instance]++
	}
	return true
}
