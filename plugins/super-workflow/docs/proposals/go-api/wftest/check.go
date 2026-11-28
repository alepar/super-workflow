package wftest

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// CheckConfig supplies the no-model suite's corpus and simulation bounds.
// Seeds defaults to 64, numbered deterministically from zero. The factory
// must return a fresh Sim and World on every call, as for Fuzz and Calibrate.
type CheckConfig struct {
	// MaxRuns caps simulation runs, including calibration discovery and finding
	// reduction, per variant: with Variants each one has its own MaxRuns, and
	// CheckReport.Runs is their sum. Zero is unlimited. Runs follow calibration, interleaved mixed/
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
	// Variants names run options values, each a value of the definition's
	// options struct. Check then calibrates and simulates once per variant,
	// overriding the factory's options, and judges coverage once on the
	// union: a state or edge any variant reaches is covered, and one marked
	// unreachable that any variant reaches is a violation. Before simulating,
	// every Edge.When guard must hold under at least one variant. Problems
	// observed under a variant name it. Empty keeps the factory's options.
	Variants map[string]any
	// Budget is the registration's default run budget (wf.Registration
	// Budget). When it gives a variant's options a budget, Check also
	// simulates that budget being spent: an all-good run, then the same run
	// stopped by the budget at its first, middle and last turns (and at the
	// default's own turn limit when the run reaches it), so the definition
	// meets a budget stop early, midway and late. Every limit stops the run
	// the same way, at its next agent instruction; where the stop lands is
	// what the definition sees.
	Budget func(options any) wf.RunBudget
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
	RunBudgetSeeds                       int // CheckConfig.Budget's runs
	UnreachedFaults                      []string
	// Variants lists CheckConfig.Variants by name, in the order simulated.
	Variants     []string
	findingSites map[string]bool
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
	if len(r.Variants) > 0 {
		fmt.Fprintf(&b, "variants: %s\n", strings.Join(r.Variants, ", "))
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
	if r.RunBudgetSeeds > 0 {
		fmt.Fprintf(&b, "run-budget runs: %d\n", r.RunBudgetSeeds)
	}
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
	// Each variant spends its own MaxRuns, so a bounded check simulates every
	// variant, not only the first.
	var budgets []*checkRunBudget
	defer func() {
		for _, b := range budgets {
			r.Runs += b.used
			r.NotExplored = append(r.NotExplored, b.skipped...)
		}
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
	variants := []checkVariant{{factory: factory}}
	if len(cfg.Variants) > 0 {
		variants = variants[:0]
		values := make([]any, 0, len(cfg.Variants))
		for _, name := range slices.Sorted(maps.Keys(cfg.Variants)) {
			value := cfg.Variants[name]
			values = append(values, value)
			variants = append(variants, checkVariant{name: name, factory: func() *Sim {
				s := factory()
				s.options = value
				return s
			}})
			r.Variants = append(r.Variants, name)
		}
		unheld, err := d.UnheldGuards(values...)
		if err != nil {
			r.addReal("variants: " + err.Error())
			return r
		}
		for _, guard := range unheld {
			r.addReal("variants: guard holds under no variant: " + guard)
		}
		if len(unheld) != 0 {
			return r
		}
	}
	r.Coverage = NewFuzzCoverage(factory, cfg.Unreachable)
	// One variant judges as it goes. Several defer calibration coverage and
	// fault reach to the union: a gap is one no variant closed.
	single := len(variants) == 1
	var calibrations []*CalibrationReport
	var unreached [][]string
	for _, v := range variants {
		r.Coverage.variant = v.name
		budget := &checkRunBudget{max: cfg.MaxRuns, tag: v.tag}
		budgets = append(budgets, budget)
		cfg.budget, calibration.budget = budget, budget
		report := calibrate(v.factory, calibration)
		calibrations = append(calibrations, report)
		if single {
			for _, problem := range report.RealProblems {
				r.addReal(v.tag(problem))
			}
			r.addCoverage(report.CoverageProblems...)
			r.Advisories = append(r.Advisories, report.ContextUnknown...)
		} else {
			for _, problem := range report.RealProblems {
				r.addRealOnce(v.tag(problem))
			}
			for _, advisory := range report.ContextUnknown {
				if !slices.Contains(r.Advisories, advisory) {
					r.Advisories = append(r.Advisories, advisory)
				}
			}
		}
		var missed []string
		r.simulate(v, cfg, budget, d, func(description string) {
			if single {
				r.UnreachedFaults = append(r.UnreachedFaults, description)
				r.addCoverage("fixture gap: fault not reached: " + description)
			} else {
				missed = append(missed, description)
			}
		})
		unreached = append(unreached, missed)
	}
	r.Calibration = mergeCalibration(calibrations)
	if !single {
		r.addCoverage(r.Calibration.CoverageProblems...)
		for _, description := range unreached[0] {
			if all(unreached[1:], func(other []string) bool { return slices.Contains(other, description) }) {
				r.UnreachedFaults = append(r.UnreachedFaults, description)
				r.addCoverage("fixture gap: fault not reached: " + description)
			}
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

// checkVariant is one named options value; the unnamed variant is a config
// without Variants, whose factory supplies the options.
type checkVariant struct {
	name    string
	factory func() *Sim
}

// tag names the variant on a problem; the unnamed variant adds nothing.
func (v checkVariant) tag(problem string) string {
	if v.name == "" {
		return problem
	}
	return "variant " + v.name + ": " + problem
}

// simulate runs every simulation phase under one variant, folding results into
// the shared coverage, and reports each fault this variant did not reach.
func (r *CheckReport) simulate(v checkVariant, cfg CheckConfig, budget *checkRunBudget, d *wf.Def, unreached func(string)) {
	factory := v.factory
	if r.findingSites == nil {
		r.findingSites = map[string]bool{}
	}
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
		result.Variant = v.name
		r.Coverage.Add(result)
		if result.Finding != nil && !r.findingSites[result.Finding.Site()] {
			r.findingSites[result.Finding.Site()] = true
			r.addReal(v.tag(result.Finding.String()))
			minimized := result.minimize(factory, func() bool { return budget.take("minimize " + result.Finding.Site()) })
			minimized.Variant = v.name
			r.Findings = append(r.Findings, minimized)
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
	r.RunBudgetSeeds += checkRunBudgetPaths(factory, cfg, d, valid, record)
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
			unreached(description)
		}
	}
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

// addRealOnce adds a problem not already reported, as when several variants
// observe the same one.
func (r *CheckReport) addRealOnce(problem string) {
	if !slices.Contains(r.RealProblems, problem) {
		r.addReal(problem)
	}
}

func all[T any](items []T, ok func(T) bool) bool {
	for _, item := range items {
		if !ok(item) {
			return false
		}
	}
	return true
}

// mergeCalibration folds the per-variant calibration reports. Observed
// problems, contexts and overrides are the union; coverage gaps and unchecked
// fields are those no variant closed. One report is returned as is.
func mergeCalibration(reports []*CalibrationReport) *CalibrationReport {
	if len(reports) == 1 {
		return reports[0]
	}
	union := func(pick func(*CalibrationReport) []string) []string {
		var out []string
		for _, report := range reports {
			for _, item := range pick(report) {
				if !slices.Contains(out, item) {
					out = append(out, item)
				}
			}
		}
		sort.Strings(out)
		return out
	}
	common := func(pick func(*CalibrationReport) []string) []string {
		var out []string
		for _, item := range pick(reports[0]) {
			if all(reports[1:], func(other *CalibrationReport) bool { return slices.Contains(pick(other), item) }) {
				out = append(out, item)
			}
		}
		return out
	}
	m := &CalibrationReport{
		RealProblems:     union(func(c *CalibrationReport) []string { return c.RealProblems }),
		CoverageProblems: common(func(c *CalibrationReport) []string { return c.CoverageProblems }),
		ContextUnknown:   union(func(c *CalibrationReport) []string { return c.ContextUnknown }),
		NotChecked:       common(func(c *CalibrationReport) []string { return c.NotChecked }),
		TypeRejected:     common(func(c *CalibrationReport) []string { return c.TypeRejected }),
		Overrides:        union(func(c *CalibrationReport) []string { return c.Overrides }),
	}
	m.Problems = append(append([]string(nil), m.RealProblems...), m.CoverageProblems...)
	sort.Strings(m.Problems)
	stale := map[string]bool{}
	for _, report := range reports {
		m.States, m.Samples = max(m.States, report.States), m.Samples+report.Samples
		for _, entry := range report.Stale {
			key := fmt.Sprintf("%s\x00%s\x00%d", entry.State, entry.Run, entry.Seq)
			if !stale[key] {
				stale[key] = true
				m.Stale = append(m.Stale, entry)
			}
		}
	}
	return m
}
