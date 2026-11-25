package wftest

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// CheckConfig supplies the no-model suite's corpus and simulation bounds.
// Seeds defaults to 64, numbered deterministically from zero. The factory
// must return a fresh Sim and World on every call, as for Fuzz and Calibrate.
type CheckConfig struct {
	// MaxRuns caps simulation runs, including calibration discovery and finding
	// reduction. Zero is unlimited. Runs follow calibration, interleaved mixed/
	// liveness seeds, gate answers, corpus, agent outcomes, repeated faults,
	// named scenarios, then fault recovery. Reduction runs immediately on a new
	// finding. Lint, recorded calibration probes and ChildProtocol are separate.
	MaxRuns int
	// ChildProtocol also probes the framework CLI against synthetic harness
	// records. This is a separate contract from definition simulation coverage.
	ChildProtocol bool
	CalibrationConfig
	// Unreachable contains reasoned exemptions for agent-state coverage.
	// Edge coverage exemptions belong on the definition's edges.
	Unreachable map[string]string
	// Scenarios adds named, bounded coverage runs for domain-specific results
	// and coordinated repair paths. Missing fixtures inherit the suite's progress
	// corpus and artifacts; ordinary random failures and schema draws are disabled.
	Scenarios map[string]FuzzConfig
}

// CheckReport retains every layer's diagnostics and reduced simulation
// findings. Problems determines failure; calibration's NotChecked entries
// are advisory observations for the definition author.
type CheckReport struct {
	// Advisories describe unknown calibration contexts and manual effect boundaries.
	// They are not included in Problems or RealProblems.
	Advisories []string
	// RealProblems are observed property, calibration, protocol, lint or fixture
	// failures, including invalid/traversed exemptions. CoverageProblems are
	// missing state/edge/end/fault coverage and budget-limited searches.
	// Problems retains the combined strict verdict for existing callers.
	RealProblems, CoverageProblems []string
	// Runs includes calibration discovery and finding-reduction replays.
	Runs int
	// NotExplored lists run-budget refusals and progressing step-bound cuts,
	// in deterministic order.
	NotExplored                          []string
	Protocol                             *ChildProtocolReport
	Problems                             []string
	Calibration                          *CalibrationReport
	Coverage                             *FuzzCoverage
	Findings                             []*FuzzResult
	UnreachableEdges                     int
	MixedSeeds, LivenessSeeds, GateSeeds int
	FaultSeeds                           int
	CorpusSeeds                          int
	AgentSeeds                           int
	RepeatedFaultSeeds                   int
	ScenarioSeeds                        int
	UnreachedFaults                      []string
}

func (r *CheckReport) String() string {
	var b strings.Builder
	if r.Protocol != nil {
		fmt.Fprint(&b, r.Protocol)
	}
	if r.Calibration != nil {
		fmt.Fprint(&b, r.Calibration)
	}
	if r.Coverage != nil {
		fmt.Fprint(&b, r.Coverage)
	}
	fmt.Fprintf(&b, "advisories: %d\n", len(r.Advisories))
	for _, advisory := range r.Advisories {
		fmt.Fprintln(&b, "advisory: "+advisory)
	}
	fmt.Fprintf(&b, "total simulation runs: %d\n", r.Runs)
	fmt.Fprintf(&b, "checker runs: %d mixed, %d liveness, %d gate-answer\n", r.MixedSeeds, r.LivenessSeeds, r.GateSeeds)
	fmt.Fprintf(&b, "fault-recovery runs: %d\n", r.FaultSeeds)
	fmt.Fprintf(&b, "corpus-path runs: %d\n", r.CorpusSeeds)
	fmt.Fprintf(&b, "agent-outcome runs: %d\n", r.AgentSeeds)
	fmt.Fprintf(&b, "repeated-fault runs: %d\n", r.RepeatedFaultSeeds)
	fmt.Fprintf(&b, "scenario runs: %d\n", r.ScenarioSeeds)
	for _, target := range r.UnreachedFaults {
		fmt.Fprintf(&b, "fault not reached: %s\n", target)
	}
	fmt.Fprintf(&b, "unreachable-by-design edges: %d\n", r.UnreachableEdges)
	for _, finding := range r.Findings {
		fmt.Fprint(&b, finding)
	}
	for _, problem := range r.Problems {
		// The protocol report above already printed these diagnostics.
		if r.Protocol != nil && (slices.Contains(r.Protocol.Problems, problem) || slices.Contains(r.Protocol.FixtureGaps, problem)) {
			continue
		}
		fmt.Fprintln(&b, problem)
	}
	return b.String()
}

// Check composes structural lint, check calibration, hostile simulation,
// liveness and state/edge reachability without invoking a model or host
// adapters. A missing corpus or unreachable edge is a failure, not a skip.
func Check(factory func() *Sim, cfg CheckConfig) *CheckReport {
	return check(factory, cfg, true)
}

func check(factory func() *Sim, cfg CheckConfig, lintDefinition bool) *CheckReport {
	calibration := cfg.CalibrationConfig
	cfg.CalibrationConfig = withRecordedCorpus(cfg.CalibrationConfig)
	r := &CheckReport{}
	originalFactory := factory
	relocationSeen := map[string]bool{}
	factory = func() *Sim {
		s := originalFactory()
		s.journalCheck = func(records []wf.JournalRecord, roots wf.PathRoots) {
			if err := wf.CheckJournalRelocation(records, roots); err != nil {
				problem := "relocation: " + err.Error()
				if !relocationSeen[problem] {
					relocationSeen[problem] = true
					r.addReal(problem)
				}
			}
		}
		return s
	}
	budget := &checkRunBudget{max: cfg.MaxRuns}
	cfg.budget, calibration.budget = budget, budget
	defer func() {
		r.Runs = budget.used
		r.NotExplored = append([]string(nil), budget.skipped...)
		for _, target := range r.NotExplored {
			r.addCoverage("not explored (budget): " + target)
		}
	}()
	if cfg.ChildProtocol {
		r.Protocol = CheckChildProtocol()
		r.addReal(r.Protocol.Problems...)
		r.addReal(r.Protocol.FixtureGaps...)
	}
	if cfg.Seeds < 0 || cfg.MaxSteps < 0 || cfg.MaxRuns < 0 {
		r.addReal("check: Seeds, MaxSteps and MaxRuns must not be negative")
		return r
	}
	if cfg.Seeds == 0 {
		cfg.Seeds = 64
	}
	d := factory().def
	beforeLint := len(r.Problems)
	if lintDefinition {
		lint := d.LintServed()
		lint = append(lint, d.LintSound()...)
		lint = append(lint, d.LintChecks()...)
		lint = append(lint, d.LintReachability()...)
		for _, p := range lint {
			r.addReal(p.Line())
		}
	}
	r.UnreachableEdges = d.UnreachableEdges()
	if len(r.Problems) != beforeLint {
		return r
	}
	calibration.Seeds, calibration.MaxSteps = cfg.Seeds, cfg.MaxSteps
	r.Calibration = calibrate(factory, calibration)
	r.addReal(r.Calibration.RealProblems...)
	r.addCoverage(r.Calibration.CoverageProblems...)
	r.Advisories = append(r.Advisories, r.Calibration.ContextUnknown...)
	r.Coverage = NewFuzzCoverage(factory, cfg.Unreachable)
	seen := map[string]bool{}
	record := func(result *FuzzResult) {
		for _, advisory := range result.Advisories {
			found := false
			for _, previous := range r.Advisories {
				if previous == advisory {
					found = true
					break
				}
			}
			if !found {
				r.Advisories = append(r.Advisories, advisory)
			}
		}
		r.Coverage.Add(result)
		if result.Finding != nil && !seen[result.Finding.Site()] {
			seen[result.Finding.Site()] = true
			r.addReal(result.Finding.String())
			r.Findings = append(r.Findings, result.minimize(factory, func() bool { return budget.take("minimize " + result.Finding.Site()) }))
		}
	}
	for seed := 0; seed < cfg.Seeds; seed++ {
		base := FuzzConfig{Seed: int64(seed), MaxSteps: cfg.MaxSteps, Valid: cfg.Valid, Progress: cfg.Progress, Artifacts: cfg.Artifacts, OutcomeSwitches: true}
		if result := cfg.run(factory, "mixed seeds", base); result != nil {
			record(result)
			r.MixedSeeds++
		}
		base.Liveness, base.LivenessAfter = true, -1
		if result := cfg.run(factory, "liveness seeds", base); result != nil {
			record(result)
			r.LivenessSeeds++
		}
	}
	// Gate exits need their own runs: liveness deliberately stops at a human
	// decision. Progress fixtures carry these runs through semantic routes.
	valid := map[string][]json.RawMessage{}
	for key, values := range cfg.Valid {
		valid[key] = values
	}
	for key, values := range cfg.Progress {
		valid[key] = values
	}
	targets := checkFaultTargets(wf.Inspect(d), d.Name())
	var gates func(wf.DefView)
	gates = func(view wf.DefView) {
		for _, state := range view.States {
			if state.Kind != "gate" || state.Cases == nil {
				continue
			}
			key := strings.TrimPrefix(state.Path, d.Name()+"/")
			for _, answer := range view.Enums[state.Cases.Enum] {
				answered := false
				for seed := 0; seed < cfg.Seeds; seed++ {
					result := cfg.run(factory, "gate "+key+" answer "+answer, FuzzConfig{Seed: int64(seed), MaxSteps: cfg.MaxSteps, Valid: valid, Progress: cfg.Progress, Artifacts: cfg.Artifacts, FailPercent: -1, SchemaPercent: -1, GateAnswers: map[string]string{key: answer}})
					if result == nil {
						break
					}
					record(result)
					r.GateSeeds++
					for _, action := range result.Script {
						var value string
						if action.Kind == FuzzAnswer && (action.State == key || action.State == state.Path) && json.Unmarshal(action.Output, &value) == nil && value == answer {
							answered = true
							break
						}
					}
					if answered {
						break
					}
				}
				if !answered {
					runs, _ := checkGatePreludePaths(factory, cfg, key, answer, targets, record)
					r.GateSeeds += runs
				}
			}
		}
		for _, child := range view.Children {
			gates(child)
		}
	}
	gates(wf.Inspect(d))
	r.CorpusSeeds = checkCorpusPaths(factory, cfg, record)
	r.AgentSeeds = checkAgentPaths(factory, cfg, record)
	r.RepeatedFaultSeeds = checkRepeatedFaultPaths(factory, cfg, record)
	r.ScenarioSeeds = checkScenarios(factory, cfg, record)
	// Recover after the actual fault, including command chains inside one engine
	// call. A random checkpoint before a failure cannot prove its repair route.
	for _, target := range targets {
		description := fmt.Sprintf("%s %s %s.%s", target.State, target.Class, target.Adapter, target.Op)
		label := "fault " + description
		reached := false
		modes := []bool{false}
		if target.Class == "timeout" {
			modes = append(modes, true)
		}
		for _, adverse := range modes {
			for seed := 0; seed < cfg.Seeds; seed++ {
				result := cfg.run(factory, label, FuzzConfig{Seed: int64(seed), MaxSteps: cfg.MaxSteps, Fault: &target, OutcomeSwitches: adverse, Valid: valid, Progress: cfg.Progress, Artifacts: cfg.Artifacts})
				if result == nil {
					break
				}
				record(result)
				r.FaultSeeds++
				if result.FaultInjected {
					reached = true
					break
				}
			}
			if reached {
				break
			}
		}
		// Broad faults can stop the parent before a child recovery gate is reached.
		// Isolate one declared effect fault at a time, preserving progress to it.
		if !reached && target.Class == "timeout" {
			for _, prelude := range targets {
				if prelude.Adapter == "" {
					continue
				}
				result := cfg.run(factory, label, FuzzConfig{MaxSteps: cfg.MaxSteps, Fault: &target, PreludeFault: &prelude, Valid: valid, Progress: cfg.Progress, Artifacts: cfg.Artifacts})
				if result == nil {
					break
				}
				record(result)
				r.FaultSeeds++
				if result.FaultInjected {
					reached = true
					break
				}
			}
		}
		if !reached && !budget.seen[label] {
			r.UnreachedFaults = append(r.UnreachedFaults, description)
			r.addCoverage("fixture gap: fault not reached: " + description)
		}
	}
	missing, violations := r.Coverage.stateProblems()
	r.addCoverage(missing...)
	r.addReal(violations...)
	missing, violations = r.Coverage.edgeProblems()
	r.addCoverage(missing...)
	r.addReal(violations...)
	return r
}

// CheckCommand adapts calibration and simulation to wf.Registration.Check.
// The CLI must run structural and check-example lint before invoking it; use
// Check for a complete standalone suite. Register this adapter only in a
// separate checker binary, keeping wftest out of shipped workflow binaries.
// It prints the report even on success, including unchecked mutation fields.
func CheckCommand(factory func() *Sim, cfg CheckConfig) func(io.Writer) error {
	return func(w io.Writer) error {
		report := check(factory, cfg, false)
		if _, err := fmt.Fprint(w, report); err != nil {
			return err
		}
		if len(report.Problems) != 0 {
			return fmt.Errorf("%d checker problem(s)", len(report.Problems))
		}
		return nil
	}
}

// checkFaultTargets preserves declaration order across the definition tree.
func checkFaultTargets(view wf.DefView, root string) []FuzzFault {
	var targets []FuzzFault
	for _, state := range view.States {
		key := strings.TrimPrefix(state.Path, root+"/")
		for _, kind := range state.EffectKinds() {
			for _, class := range []string{"effect-unavailable", "effect-timeout", "effect-adverse", "effect-uncertain"} {
				targets = append(targets, FuzzFault{State: key, Adapter: kind.Adapter, Op: kind.Op, Class: class})
			}
		}
		for _, kind := range state.ObservationKinds() {
			for _, class := range []string{"observation-unavailable", "observation-error"} {
				targets = append(targets, FuzzFault{State: key, Adapter: kind.Adapter, Op: kind.Op, Class: class})
			}
		}
		for _, edge := range state.Edges {
			if state.Kind == "gate" && edge.Timeout > 0 {
				targets = append(targets, FuzzFault{State: key, Class: "timeout"})
				break
			}
		}
	}
	for _, child := range view.Children {
		targets = append(targets, checkFaultTargets(child, root)...)
	}
	return targets
}

func (r *CheckReport) addReal(problems ...string) {
	r.RealProblems = append(r.RealProblems, problems...)
	r.Problems = append(r.Problems, problems...)
}
func (r *CheckReport) addCoverage(problems ...string) {
	r.CoverageProblems = append(r.CoverageProblems, problems...)
	r.Problems = append(r.Problems, problems...)
}
