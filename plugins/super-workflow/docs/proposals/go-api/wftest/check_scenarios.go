package wftest

import "slices"

// checkScenarios runs supplied fixtures through the same real engine and
// property checks as generated seeds. Names impose a stable execution order.
func checkScenarios(factory func() *Sim, cfg CheckConfig, record func(*FuzzResult)) int {
	names := make([]string, 0, len(cfg.Scenarios))
	for name := range cfg.Scenarios {
		names = append(names, name)
	}
	slices.Sort(names)
	runs := 0
	for _, name := range names {
		scenario := cfg.Scenarios[name]
		scenario.corpusPrefix = true
		if scenario.MaxSteps == 0 {
			scenario.MaxSteps = cfg.MaxSteps
		}
		if scenario.Progress == nil {
			scenario.Progress = cfg.Progress
		}
		if scenario.Valid == nil {
			scenario.Valid = scenario.Progress
		}
		if scenario.Artifacts == nil {
			scenario.Artifacts = cfg.Artifacts
		}
		if scenario.FailPercent == 0 {
			scenario.FailPercent = -1
		}
		if scenario.SchemaPercent == 0 {
			scenario.SchemaPercent = -1
		}
		if result := cfg.run(factory, "scenario "+name, scenario); result != nil {
			record(result)
			runs++
		}
	}
	return runs
}
