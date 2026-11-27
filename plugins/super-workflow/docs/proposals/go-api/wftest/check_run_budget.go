package wftest

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// checkRunBudgetPaths simulates CheckConfig.Budget being spent (see there).
// Simulated attempts spend little, so a default sized for real runs is rarely
// reached by itself: each run cuts the turn limit at a turn of an all-good
// reference run instead. It returns the runs it made.
func checkRunBudgetPaths(factory func() *Sim, cfg CheckConfig, d *wf.Def, valid map[string][]json.RawMessage, record func(*FuzzResult)) int {
	if cfg.Budget == nil || wf.DefaultBudget(d, cfg.Budget, factory().options) == (wf.RunBudget{}) {
		return 0
	}
	good := FuzzConfig{MaxSteps: cfg.MaxSteps, Valid: valid, Progress: cfg.Progress, Artifacts: cfg.Artifacts, FailPercent: -1, SchemaPercent: -1}
	ref := cfg.run(factory, "run budget", good)
	if ref == nil {
		return 0
	}
	record(ref)
	runs, turns := 1, 0
	if ref.Trace != nil {
		for _, r := range ref.Trace.Journal() {
			if r.Kind == wf.JournalInstructionIssued {
				turns++
			}
		}
	}
	// Turns t lets t instructions out and stops the next one, so the last
	// cut that stops anything is one short of the reference run's turns.
	cuts := []int{1, turns / 2, turns - 1}
	if b := wf.DefaultBudget(d, cfg.Budget, factory().options); b.Turns > 0 && b.Turns < turns {
		cuts = append(cuts, b.Turns)
	}
	slices.Sort(cuts)
	for _, t := range slices.Compact(cuts) {
		if t < 1 || t >= turns {
			continue
		}
		c := good
		c.RunBudget = wf.RunBudget{Turns: t}
		result := cfg.run(factory, fmt.Sprintf("run budget at turn %d", t), c)
		if result == nil {
			break
		}
		record(result)
		runs++
	}
	return runs
}
