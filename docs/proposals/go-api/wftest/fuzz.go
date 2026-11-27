package wftest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// FuzzConfig bounds one deterministic hostile-agent run. Prefix holds concrete
// regression actions before random actions begin. Valid holds known-good
// outputs per state (keyed like FuzzAction.State); a random submit picks one
// of them half the time and draws from the output schema otherwise, so a
// seed can pass semantic checks a schema cannot express and reach depth.
// A factory must return a fresh Sim and World, including fresh state
// captured by its adapter hooks.
// FuzzFault selects one adverse outcome before the all-good recovery phase.
// State is qualified like FuzzAction.State. Adapter/Op identify an effect kind;
// a gate timeout uses Class "timeout" and leaves them empty.
type FuzzFault struct {
	State, Class, Adapter, Op string
}

type FuzzConfig struct {
	// EffectResults supplies finite state-scoped structured effect outcomes.
	EffectResults []FuzzEffectSequence
	corpusPrefix  bool // liveness prefix submits use curated corpus, never schema fallback
	// Fault drives progress outputs until this fault occurs, then checks liveness.
	Fault *FuzzFault
	// PreludeFault injects one isolated effect fault to reach the primary Fault.
	// It does not start the all-good recovery phase.
	PreludeFault *FuzzFault
	// RepeatPrelude repeats the selected effect prelude for coverage of exhaustion paths.
	// It does not enable liveness or an all-good recovery phase.
	RepeatPrelude bool
	// Progress designates outputs that advance the intended successful path;
	// accepted negative reviews belong in Valid, not Progress.
	Progress map[string][]json.RawMessage
	// GateAnswers drives declared gate answers in coverage runs.
	GateAnswers map[string]string
	// AgentActions repeatedly selects a hostile outcome at a qualified state.
	// Other states follow the ordinary configuration; useful for exhaustion paths.
	AgentActions map[string]FuzzActionKind
	// Liveness enables a bounded all-good phase. LivenessAfter is the number
	// of mixed steps before it (-1 chooses a seeded random step).
	Liveness      bool
	LivenessAfter int
	// LivenessSteps bounds all-good work per instance; zero uses three times
	// MaxSteps. The global phase bound scales with spawned instances.
	LivenessSteps   int
	OutcomeSwitches bool
	probe           func(*wf.Engine, wf.RunID, wf.Instruction)
	Seed            int64
	// MaxSteps bounds work per instance; the global driver bound scales with
	// spawned instances. Per-instance actions and journal records still cap cycles.
	MaxSteps int
	// RunBudget is the engine's run budget for this run (see
	// wf.RunBudget); zero is none. A run the budget stops ends the drive
	// with End "run budget".
	RunBudget wf.RunBudget
	Prefix    []FuzzAction
	Valid     map[string][]json.RawMessage
	// FailPercent is the share of agent turns that take a hostile action
	// (refuse, transient, blocked, ended without done), 20 when zero.
	// SchemaPercent is the share of submits drawn from the output schema
	// rather than Valid, 50 when zero; it applies only where Valid has
	// outputs. A long definition runs some seeds gentler to reach depth.
	FailPercent, SchemaPercent int
	// Artifacts holds, per state (keyed like Valid), the contents a submit
	// writes to its allocated artifacts, as a scenario's WithArtifacts does;
	// an artifact without one gets a placeholder.
	Artifacts map[string]map[string]string
}

type FuzzActionKind string

const (
	FuzzSubmit FuzzActionKind = "submit"
	// FuzzWait records a mixed-phase turn with every outcome disabled.
	FuzzWait             FuzzActionKind = "wait"
	FuzzRefuse           FuzzActionKind = "refuse"
	FuzzTransient        FuzzActionKind = "transient"
	FuzzBlocked          FuzzActionKind = "blocked"
	FuzzEndedWithoutDone FuzzActionKind = "ended-without-done"
	// FuzzAnswer answers an open human gate; Output is the answer as a JSON
	// string.
	FuzzAnswer FuzzActionKind = "answer"
	// FuzzTimeout advances to an open gate's declared deadline.
	FuzzTimeout FuzzActionKind = "timeout"
	// FuzzRecover answers an open recovery offer (B2, F1) the way a
	// coordinator does; Output is the choice (wf.RecoveryRestart,
	// wf.RecoveryGiveUp or wf.RecoveryResume) as a JSON string.
	FuzzRecover FuzzActionKind = "recover"
)

// FuzzAction is JSON-portable; State addresses a qualified state, not a nonce
// or a temporary directory. Refuse uses the CLI's JSON-string normalization for malformed output, so the journal remains valid.
type FuzzAction struct {
	State  string
	Kind   FuzzActionKind
	Output json.RawMessage `json:",omitempty"`
}

type FuzzFinding struct {
	Rule, State, Edge, Target, Why string
	Seed                           int64
}

// Site identifies a finding without its seed: "rule state", plus
// " edge "label" -> target" when an edge is named. A test that tracks a
// known finding matches on it.
func (f FuzzFinding) Site() string {
	if f.Edge == "" {
		return f.Rule + " " + f.State
	}
	return fmt.Sprintf("%s %s edge %q -> %s", f.Rule, f.State, f.Edge, f.Target)
}

func (f FuzzFinding) String() string {
	edge := ""
	if f.Edge != "" {
		edge = fmt.Sprintf(" edge %q -> %s", f.Edge, f.Target)
	}
	return fmt.Sprintf("fuzz: %s %s%s · %s · seed %d", f.Rule, f.State, edge, f.Why, f.Seed)
}

type FuzzRefusal struct {
	State   string
	Reasons []string
	Fix     string
}

// FuzzResult retains the actual journal and concrete actions, not just a seed
// whose choices might change when a schema changes.
type FuzzResult struct {
	// Variant names the CheckConfig.Variants entry the run simulated; empty
	// outside Check or without variants.
	Variant string
	// BudgetReason marks incomplete mixed exploration, never a successful end.
	BudgetReason         string
	Advisories           []string
	adversarialInjected  bool // an actual fault, hostile action, or consumed crash; never a configured switch
	FaultInjected        bool
	PreludeFaultInjected bool
	liveSteps            []int
	PrefixOutcome        bool
	PrefixReason         string
	Suspended            bool
	Switches             map[string]bool
	LivenessStart        int
	Seed                 int64
	Script               []FuzzAction
	Trace                *Trace
	Status               wf.RunStatus
	Finding              *FuzzFinding
	Refusals             []FuzzRefusal
	Agents               int
	// Reached lists the agent states issued, by their definition path
	// (the root definition's name trimmed), one entry per issue.
	Reached []string
	// Mix counts the actions taken by source: prefix, replay, valid and
	// schema submits, and each failure kind.
	Mix map[string]int
	// End classifies how the seed ended: success, human gate, stopped
	// (a named non-success end) or finding.
	End            string
	ReplayEqual    bool
	ShrinkRuns     int
	ShrinkComplete bool
	config         FuzzConfig
}

func (r *FuzzResult) String() string {
	var b strings.Builder
	if r.Variant != "" {
		fmt.Fprintln(&b, "variant: "+r.Variant)
	}
	if r.BudgetReason != "" {
		fmt.Fprintln(&b, "not explored (budget): "+r.BudgetReason)
	}
	for _, advisory := range r.Advisories {
		fmt.Fprintln(&b, "advisory: "+advisory)
	}
	if r.config.PreludeFault != nil {
		p := r.config.PreludeFault
		fmt.Fprintf(&b, "prelude fault: %s %s %s.%s injected=%t\n", p.State, p.Class, p.Adapter, p.Op, r.PreludeFaultInjected)
	}
	if r.config.Fault != nil {
		t := r.config.Fault
		fmt.Fprintf(&b, "fault: %s %s %s.%s injected=%t\n", t.State, t.Class, t.Adapter, t.Op, r.FaultInjected)
	}
	if len(r.Switches) > 0 {
		keys := make([]string, 0, len(r.Switches))
		for key := range r.Switches {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		fmt.Fprintln(&b, "switches:")
		for _, key := range keys {
			fmt.Fprintf(&b, "  %s=%t\n", key, r.Switches[key])
		}
	}
	if r.PrefixOutcome {
		reason := r.PrefixReason
		if reason == "" {
			reason = "run ended before any eligible live checkpoint"
		}
		fmt.Fprintln(&b, "prefix outcome: "+reason)
	}
	if r.config.Liveness {
		fmt.Fprintf(&b, "liveness starts at step %d\n", r.LivenessStart)
	}
	if r.Finding != nil {
		fmt.Fprintln(&b, r.Finding)
	} else {
		fmt.Fprintf(&b, "seed %d: %s %s\n", r.Seed, r.Status.Phase, r.Status.Terminal)
	}
	script, err := json.MarshalIndent(r.Script, "", "  ")
	if err != nil {
		fmt.Fprintf(&b, "script encoding: %v\n", err)
	} else {
		fmt.Fprintf(&b, "script:\n%s\n", script)
	}
	for _, refusal := range r.Refusals {
		fmt.Fprintf(&b, "refused %s: %s\nfix: %s\n", refusal.State, strings.Join(refusal.Reasons, "; "), refusal.Fix)
	}
	if r.ShrinkRuns > 0 && !r.ShrinkComplete {
		fmt.Fprintf(&b, "reduction: replay budget exhausted after %d attempts; script still reproduces\n", r.ShrinkRuns)
	}
	if r.Trace != nil {
		fmt.Fprintf(&b, "trace:\n%s", r.Trace.String())
	}
	return b.String()
}

// Fuzz drives the real engine without executing external adapters. Only the
// factory's World supplies observations/effects; unknown reads are not faked
// into success. The bound also catches loops inside a single engine call.
func Fuzz(factory func() *Sim, config FuzzConfig) *FuzzResult {
	return fuzz(factory, config, nil)
}

func fuzz(factory func() *Sim, config FuzzConfig, take func() bool) *FuzzResult {
	if config.Fault != nil {
		config.Liveness = true
		config.LivenessAfter = 0
	}
	if config.Liveness {
		config.corpusPrefix = true
	}
	if config.Fault == nil && config.Liveness && config.LivenessAfter == -1 {
		mixed := config
		mixed.Liveness = false
		mixed.LivenessAfter = 0
		if take != nil && !take() {
			return nil
		}
		prefix := runFuzz(factory, mixed, nil, false)
		if len(prefix.liveSteps) == 0 {
			prefix.PrefixOutcome = true
			return prefix
		}
		random := rand.New(rand.NewSource(config.Seed ^ 0xc4ec))
		config.LivenessAfter = prefix.liveSteps[random.Intn(len(prefix.liveSteps))]
	}
	if take != nil && !take() {
		return nil
	}
	return runFuzz(factory, config, nil, false)
}

// Replay executes exactly the recorded actions from a fresh factory.
func (r *FuzzResult) Replay(factory func() *Sim) *FuzzResult {
	return runFuzz(factory, r.config, r.Script, true)
}

var errFuzzBound = errors.New("fuzz step bound reached")

type fuzzJournal struct {
	defs map[string]string
	wf.Journal
	limit, records  int
	instanceRecords map[string]int
	onRecord        func(wf.JournalRecord)
}

func (j *fuzzJournal) Append(r wf.JournalRecord) error {
	if j.records >= j.limit*j.instances() || j.instanceRecords[r.Instance] >= j.limit {
		return errFuzzBound
	}
	if err := j.Journal.Append(r); err != nil {
		return err
	}
	if j.instanceRecords == nil {
		j.instanceRecords = map[string]int{}
	}
	j.instanceRecords[r.Instance]++
	j.records++
	if j.onRecord != nil {
		j.onRecord(r)
	}
	if j.defs == nil {
		j.defs = map[string]string{}
	}
	switch r.Kind {
	case wf.JournalRunStarted:
		var d wf.RunStartedData
		if json.Unmarshal(r.Data, &d) == nil {
			j.defs[""] = d.Def
		}
	case wf.JournalChildSpawned:
		var d wf.ChildSpawnedData
		if json.Unmarshal(r.Data, &d) == nil {
			j.defs[d.ID] = j.defs[r.Instance] + "/" + d.Def
		}
	}
	return nil
}

type fuzzRun struct {
	step    int
	good    bool
	journal *fuzzJournal
	// fixes counts, per unit, the outputs validate refused that the child
	// replaced before done (at most fuzzFixes): a child that validates.
	fixes map[string]int
	// reads, when set, collects the world's hooked reads (see runFuzz).
	reads         *[]string
	result        *FuzzResult
	sim           *Sim
	runner        *runner
	engine        *wf.Engine
	engineOptions wf.EngineOptions
	now           time.Time
	schemas       map[string]wf.StateView
	views         map[string]wf.DefView
	writers       map[string]string
	random        *rand.Rand
	// choices draws the hostile agent's actions. It is separate from random,
	// which feeds engine nonces, so a replayed script sees the same nonces.
	choices *rand.Rand
	// skew counts the recovery answers the fuzzer inserted where a prefix
	// named none (a prefix written before the recovery offer): the prefix
	// is indexed by the script's length less them.
	skew int
	// lastSchema marks the current random submit as schema-drawn.
	lastSchema        bool
	last              wf.Instruction
	open              *wf.Instruction
	timeoutGate       *wf.Instruction // last real gate timeout selected, for intentional terminal proof
	actionContext     string
	completedContexts map[string]map[string]bool
}

func runFuzz(factory func() *Sim, config FuzzConfig, script []FuzzAction, replay bool) (result *FuzzResult) {
	if config.MaxSteps == 0 {
		config.MaxSteps = 256
	}
	result = &FuzzResult{Seed: config.Seed, config: config, Mix: map[string]int{}}
	if config.LivenessSteps < 0 || config.LivenessAfter < -1 {
		result.Finding = &FuzzFinding{Rule: "invalid-bound", Why: "invalid liveness bounds", Seed: config.Seed}
		return
	}
	if config.MaxSteps < 1 {
		result.Finding = &FuzzFinding{Rule: "invalid-bound", Why: "MaxSteps must be positive", Seed: config.Seed}
		return
	}
	s := factory()
	dir, err := os.MkdirTemp("", "wftest-fuzz-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	cj := NewCrashJournal(wf.MemJournal())
	j := &fuzzJournal{Journal: cj, limit: config.MaxSteps * 32}
	r := &runner{s: s, j: j, cj: cj, id: "fuzz", dir: dir, reg: wf.NewAdapters()}
	f := &fuzzRun{journal: j, fixes: map[string]int{}, result: result, sim: s, runner: r, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), schemas: map[string]wf.StateView{}, views: map[string]wf.DefView{}, writers: map[string]string{}, random: rand.New(rand.NewSource(config.Seed)), choices: rand.New(rand.NewSource(config.Seed ^ 0x5eed))}
	if s.clock != nil {
		f.now = s.clock()
	}
	var index func(wf.DefView)
	index = func(v wf.DefView) {
		f.views[v.Name] = v
		for _, state := range v.States {
			f.schemas[strings.TrimPrefix(state.Path, s.def.Name()+"/")] = state
		}
		for _, child := range v.Children {
			index(child)
		}
	}
	index(wf.Inspect(s.def))
	f.setupPhases(config)
	finishSequences := f.setupEffectSequences(config)
	if result.Finding != nil {
		return
	}
	if config.Liveness {
		bound := config.LivenessSteps
		if bound == 0 {
			bound = 3 * config.MaxSteps
		}
		j.limit = (result.LivenessStart + bound) * 32
		if config.Fault != nil {
			j.limit = (config.MaxSteps + bound) * 32
		}
	}
	if config.Fault == nil && config.Liveness && result.LivenessStart == 0 {
		f.good = true
		s.world.setFuzzGood(true)
	}
	defer func() {
		if p := recover(); p != nil {
			f.find("definition-panic", fmt.Sprintf("the definition must handle the generated input: %v", p))
		}
		result.Trace = r.trace()
		if s.journalCheck != nil {
			workspace := s.workspace
			if workspace == "" {
				workspace, _ = os.Getwd()
			}
			s.journalCheck(result.Trace.Journal(), wf.PathRoots{Workspace: workspace, State: dir})
		}
		if f.engine != nil {
			if status, e := f.engine.Status(r.id); e == nil {
				result.Status = status
			}
		}
		if result.Finding == nil && result.BudgetReason == "" {
			finishSequences()
		}
		// A read the world could not answer means the seed left the fixture:
		// whatever the run did next says nothing about the definition.
		if result.Finding == nil && len(s.world.missing) > 0 {
			f.find("fixture-gap", "the test world has no value for "+s.world.missing[0])
		}
		// So is a prompt file the test's skills root lacks.
		if result.Finding == nil {
			f.missingPrompts()
		}
		// The engine halts on a definition bug (an unresolved placeholder, an
		// unbound prompt input, an undeclared read) rather than routing it:
		// no agent or human answer repairs it, so it is never a named stop.
		if result.Finding == nil {
			f.definitionErrors()
		}
		if result.Finding == nil && config.Liveness && f.good && result.Status.Phase == wf.RunEnded && !result.Status.Outcome.Delivered() && !f.declaredStoppedAfterFault() && !f.endsDeclaredStop() {
			f.find("liveness", "all-good results cannot recover this run to success or a human gate")
		}
		if result.Finding == nil && !result.Suspended && !result.PrefixOutcome {
			f.finishProperties()
		}
		switch {
		case result.PrefixOutcome:
			result.End = "prefix outcome"
		case result.Suspended:
			result.End = "outcomes disabled"
		case result.Finding != nil:
			result.End = "finding"
		case result.BudgetReason != "":
			result.End = "budget"
		case result.Status.BudgetStop != "":
			result.End = "run budget"
		case result.Status.Phase == wf.RunEnded && result.Status.Outcome.Delivered():
			result.End = string(result.Status.Outcome) // success or partial
		case f.last.Kind == wf.InstructionHumanGate:
			result.End = "human gate"
		case f.manualUncertainBoundary(f.last):
			result.End = "human gate"
		case f.last.Kind == wf.InstructionManualVerification:
			result.End = "manual verification"
		case f.last.Kind == wf.InstructionWaiting:
			result.End = "waiting"
		case f.last.Kind == wf.InstructionRecovery:
			result.End = "recovery offer"
		default:
			result.End = "stopped"
		}
	}()
	// The world's hooked reads, recorded while reads is set: validate and done
	// are compared only when they read the same world.
	WrapRead(s.world, func(next func(wf.ObservationDesc) (json.RawMessage, bool)) func(wf.ObservationDesc) (json.RawMessage, bool) {
		return func(d wf.ObservationDesc) (json.RawMessage, bool) {
			v, ok := next(d)
			if f.reads != nil {
				*f.reads = append(*f.reads, fmt.Sprint(d)+"="+string(v))
			}
			return v, ok
		}
	})
	if err = r.reg.Register(s.world); err != nil {
		f.find("adapter-error", err.Error())
		return
	}
	opts := wf.EngineOptions{Assets: s.assets, Defs: []*wf.Def{s.def}, RunID: r.id, Harness: "claude", Clock: func() time.Time { return f.now }, RunDir: func(wf.RunID) string { return dir }, EffectRunner: s.runner, Nonce: func() string { return fmt.Sprintf("%016x", f.random.Uint64()) }, RunBudget: config.RunBudget}
	if s.workspace != "" {
		opts.Workspace = s.workspace
	}
	f.engineOptions = opts
	j.onRecord = f.reconcileUncertainRecord
	f.engine = wf.NewEngine(j, r.reg, opts).(*wf.Engine)
	r.e = f.engine
	f.drive(func() { _, err = f.engine.Run(s.def, s.params, s.options) })
	if err != nil {
		f.engineError(err)
		return
	}
	limit := config.MaxSteps
	if config.Liveness {
		bound := config.LivenessSteps
		if bound == 0 {
			bound = 3 * config.MaxSteps
		}
		limit = result.LivenessStart + bound
		if config.Fault != nil {
			limit = config.MaxSteps + bound
		}
	}
	var steps fuzzStepBudget
	goodLimit := config.LivenessSteps
	if goodLimit == 0 {
		goodLimit = 3 * config.MaxSteps
	}
driver:
	for step := 0; step < limit*j.instances(); step++ {
		f.step = step
		if config.Fault != nil && !result.FaultInjected && step >= config.MaxSteps*j.instances() {
			break
		}
		if config.Fault != nil && result.FaultInjected {
			bound := config.LivenessSteps
			if bound == 0 {
				bound = 3 * config.MaxSteps
			}
			if step >= result.LivenessStart+bound*j.instances() {
				break
			}
		}
		current, er := f.engine.Status(r.id)
		if er != nil {
			f.engineError(er)
			return
		}
		if current.Phase == wf.RunEnded {
			return
		}
		if current.BudgetStop != "" {
			result.Status = current // the run budget stopped the run: nothing more starts
			return
		}
		eligible := true
		if config.corpusPrefix && !f.good {
			eligible = f.livenessCheckpointEligible(current)
		}
		if eligible {
			result.liveSteps = append(result.liveSteps, step)
		}
		if config.Fault == nil && config.Liveness && step >= result.LivenessStart {
			if !f.good && !eligible {
				f.find("checkpoint-ineligible", "hostile prefix spent the active recovery reserve before the requested all-good checkpoint")
				return
			}
			f.good = true
			s.world.setFuzzGood(true)
		}
		var ins []wf.Instruction
		var e error
		if f.open != nil && f.stillHanded(r.id, f.open.Name) {
			ins = []wf.Instruction{*f.open}
			f.open = nil
		} else {
			// A held hand-out the run retired since (a recovery's give-up
			// cancelled its sibling) is not acted on: ask again.
			f.open = nil
			if f.drive(func() {
				ins, e = f.engine.HandOutAs(r.id, 1, wf.Coordinator{Harness: "claude", Session: "fuzz-session"})
			}) {
				continue
			}
		}
		if e != nil {
			f.engineError(e)
			return
		}
		status, e := f.engine.Status(r.id)
		if e != nil {
			f.engineError(e)
			return
		}
		result.Status = status
		if status.Phase == wf.RunEnded {
			return
		}
		answered := false
		for _, in := range ins {
			in, e = simulationHumanGate(in)
			if e != nil {
				f.find("reserved-gate-delivery", e.Error())
				return
			}
			if in.Kind == wf.InstructionRecovery {
				if !steps.take(in.Child, limit, goodLimit, f.good) {
					f.last = in
					break driver
				}
				f.last = in
				action, ok := f.recoverAction(in, config, script, replay)
				if !ok {
					return
				}
				result.Script = append(result.Script, action)
				var choice string
				if e = json.Unmarshal(action.Output, &choice); e == nil {
					f.drive(func() { e = f.engine.Recover(r.id, in.Name, choice) })
				}
				if e != nil {
					f.engineError(e)
					return
				}
				// A unit handed out in the same call is still the
				// coordinator's to start: keep it for the next step.
				for _, other := range ins {
					if other.Kind == wf.InstructionAgent {
						held := other
						f.open = &held
						break
					}
				}
				answered = true
				break
			}
			if in.Kind == wf.InstructionManualVerification || in.Kind == wf.InstructionWaiting {
				f.externalStop(in)
				return
			}
			if in.Kind != wf.InstructionHumanGate {
				continue
			}
			if !steps.take(in.Child, limit, goodLimit, f.good) {
				f.last = in
				break driver
			}
			f.last = in
			action, ok := f.gateAction(in, config, script, replay)
			if !ok {
				return
			}
			result.Script = append(result.Script, action)
			if action.Kind == FuzzTimeout {
				gate := in
				f.timeoutGate = &gate
				f.now = in.Deadline
				if f.recoveredFault(wf.EffectContext{State: f.stateKey(in)}, "timeout") {
					// Advancing the clock schedules the timeout; the next engine
					// drive actually takes its edge and starts the recovery budget.
					result.LivenessStart = step + 1
					f.sim.world.setFuzzGood(true)
				}
				answered = true
				break
			}
			var value string
			if e = json.Unmarshal(action.Output, &value); e == nil {
				f.drive(func() {
					e = f.engine.AnswerVisit(r.id, wf.GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: value})
				})
			}
			if e != nil {
				f.engineError(e)
				return
			}
			answered = true
			break
		}
		if answered {
			continue
		}
		var in wf.Instruction
		for _, candidate := range ins {
			if candidate.Kind == wf.InstructionAgent {
				in = candidate
				break
			}
		}
		if in.Kind == "" {
			if len(status.Due) > 0 {
				// A consumed backoff can remain in the status beside a newly
				// launched effect. Only future deadlines advance the simulation.
				var next time.Time
				for _, due := range status.Due {
					if due.Due.After(f.now) && (next.IsZero() || due.Due.Before(next)) {
						next = due.Due
					}
				}
				if next.After(f.now) {
					f.now = next.Add(time.Nanosecond)
					continue
				}
			}
			if status.Phase == wf.RunBlocked && (len(status.Fix) > 0 || f.namedHalt(status)) {
				if config.Liveness && f.good {
					f.find("liveness", "run halted without a human gate or repair route")
				}
				return
			}
			f.find("stall-unnamed", "the run must offer work, a human decision, or a named failure")
			return
		}
		if !steps.take(in.Child, limit, goodLimit, f.good) {
			f.last = in
			break driver
		}
		f.last = in
		var action FuzzAction
		switch {
		case replay:
			if len(result.Script) >= len(script) {
				f.find("script-incomplete", "the script must reach the original observation")
				return
			}
			action = script[len(result.Script)]
		case !f.good && config.AgentActions[f.stateKey(in)] != "":
			action = FuzzAction{State: in.State, Kind: config.AgentActions[f.stateKey(in)]}
		case f.good || (config.Fault != nil && !config.OutcomeSwitches):
			action = FuzzAction{State: in.State, Kind: FuzzSubmit}
			valid := f.corpus(in, config.Progress)
			if len(valid) == 0 {
				f.find("no-progress-example", "all-good phase requires a designated progress example")
				return
			}
			action.Output = valid[f.choices.Intn(len(valid))]
		case len(result.Script)-f.skew < len(config.Prefix):
			action = config.Prefix[len(result.Script)-f.skew]
		default:
			// One action in five is hostile, spread over the failure classes;
			// a run of submits is what carries a seed past the first states.
			action = FuzzAction{State: in.State, Kind: FuzzSubmit}
			fail, schema := config.FailPercent, config.SchemaPercent
			if fail == 0 {
				fail = 20
			}
			if schema == 0 {
				schema = 50
			}
			if !config.OutcomeSwitches && f.choices.Intn(100) < fail {
				action.Kind = []FuzzActionKind{FuzzRefuse, FuzzTransient, FuzzBlocked, FuzzEndedWithoutDone}[f.choices.Intn(4)]
			}
			if config.OutcomeSwitches {
				kinds := []FuzzActionKind{FuzzSubmit, FuzzRefuse, FuzzTransient, FuzzBlocked, FuzzEndedWithoutDone}
				var enabled []FuzzActionKind
				for _, kind := range kinds {
					if result.Switches[f.stateKey(in)+"/"+string(kind)] {
						enabled = append(enabled, kind)
					}
				}
				if len(enabled) == 0 {
					enabled = []FuzzActionKind{FuzzWait}
				}
				action.Kind = enabled[f.choices.Intn(len(enabled))]
			}
			if action.Kind == FuzzSubmit && config.corpusPrefix {
				valid := f.valid(in)
				if len(valid) == 0 {
					valid = f.corpus(in, config.Progress)
				}
				if len(valid) == 0 {
					f.find("fixture-gap", "liveness prefix requires curated Valid or Progress outputs; schema results may change the progress context")
					return
				}
				action.Output = valid[f.choices.Intn(len(valid))]
			} else if valid := f.valid(in); action.Kind == FuzzSubmit && len(valid) > 0 && f.choices.Intn(100) >= schema {
				action.Output = valid[f.choices.Intn(len(valid))]
			} else if action.Kind == FuzzSubmit {
				state, ok := f.schemas[f.stateKey(in)]
				if !ok || state.OutputSchema() == nil {
					f.find("schema-missing", "the issued agent must expose its declared output schema")
					return
				}
				action.Output, e = drawSchema(state.OutputSchema(), f.choices.Intn(16))
				f.lastSchema = true
				if e != nil {
					f.find("schema-unsupported", e.Error())
					return
				}
			}
		}
		if action.State != "" && action.State != in.State && !strings.HasSuffix(in.State, "/"+action.State) {
			f.find("script-mismatch", "the next action must name the issued state")
			return
		}
		switch {
		case replay:
			result.Mix["replay"]++
		case len(result.Script)-f.skew < len(config.Prefix):
			result.Mix["prefix"]++
		case action.Kind == FuzzSubmit && action.Output != nil && f.lastSchema:
			result.Mix["schema"]++
		case action.Kind == FuzzSubmit:
			result.Mix["valid"]++
		default:
			result.Mix[string(action.Kind)]++
		}
		f.lastSchema = false
		action.State = in.State
		result.Reached = append(result.Reached, f.stateKey(in))
		result.Script = append(result.Script, action)
		result.Agents++
		f.actionContext = stepContext(status, in.Child)
		f.drive(func() { e = f.act(in, action) })
		if e != nil {
			f.engineError(e)
			return
		}
		if result.Suspended || result.Finding != nil {
			return
		}
	}
	// The final action may have reached a terminal or gate exactly at the
	// bound. Inspect the fold without driving another step before failing.
	status, err := f.engine.Status(r.id)
	if err != nil {
		f.engineError(err)
		return
	}
	result.Status = status
	if status.Phase == wf.RunEnded {
		return
	}
	for _, instance := range status.Instances {
		key := strings.TrimPrefix(f.journal.defs[instance.ID]+"/"+instance.State, s.def.Name()+"/")
		if state, ok := f.schemas[key]; ok && state.Kind == "gate" && (instance.Phase == "ready" || instance.Phase == "issued") {
			f.last = wf.Instruction{Kind: wf.InstructionHumanGate, State: instance.State}
			return
		}
	}
	if config.Liveness && !f.good {
		result.PrefixOutcome = true
		result.PrefixReason = "prefix bound reached before the all-good phase began"
		return
	}
	if config.Liveness {
		f.find("liveness", "all-good phase did not reach success or a human gate within its bound")
	} else if f.progressingAtStepBound(status) {
		result.BudgetReason = "step bound reached with pending work in a new state/counter context"
	} else {
		f.find("loops-unbounded", "the seed must reach an outcome within its step bound")
	}
	return
}

func (f *fuzzRun) definitionErrors() {
	for _, record := range f.runner.trace().Journal() {
		if record.Kind != wf.JournalRefused {
			continue
		}
		var d wf.RefusedData
		if json.Unmarshal(record.Data, &d) != nil {
			continue
		}
		for _, reason := range d.Reasons {
			for _, bug := range []error{wf.ErrUnresolved, wf.ErrPromptUnbound, wf.ErrUndeclaredObservation} {
				if strings.Contains(reason, bug.Error()) {
					f.findAt(f.defOf(record.Instance)+"/"+record.State, "definition-error", "the output the agent gave reached a definition bug: "+strings.TrimPrefix(reason, "definition error: "))
					return
				}
			}
		}
	}
}

func (f *fuzzRun) missingPrompts() {
	for _, record := range f.runner.trace().Journal() {
		var d wf.RefusedData
		if record.Kind != wf.JournalRefused || json.Unmarshal(record.Data, &d) != nil {
			continue
		}
		for _, reason := range d.Reasons {
			if strings.Contains(reason, wf.ErrPromptNotFound.Error()) {
				f.findAt(f.defOf(record.Instance)+"/"+record.State, "fixture-gap", "the test's prompt files lack one the state reads: "+reason)
				return
			}
		}
	}
}

// defOf names the definition an instance runs, from its child_spawned record.
func (f *fuzzRun) defOf(instance string) string {
	for _, record := range f.runner.trace().Journal() {
		var d wf.ChildSpawnedData
		if record.Kind == wf.JournalChildSpawned && json.Unmarshal(record.Data, &d) == nil && d.ID == instance {
			return d.Def
		}
	}
	return f.sim.def.Name()
}

// gateAction picks the answer for an open human gate: the script's next
// action when replaying or in the prefix (a script that does not answer here
// stops at the gate, as the recorded seed did). A targeted or enabled timeout
// selects its declared deadline; explicit GateAnswers override random timeouts.
// Otherwise it chooses an answer three times in four and stops the fourth.
func (f *fuzzRun) gateAction(in wf.Instruction, config FuzzConfig, script []FuzzAction, replay bool) (FuzzAction, bool) {
	if target := config.Fault; !replay && target != nil && !f.result.FaultInjected && target.Class == "timeout" && target.State == f.stateKey(in) && target.Adapter == "" && target.Op == "" && f.timedGate(in) {
		f.result.Mix["timeout"]++
		return FuzzAction{State: in.State, Kind: FuzzTimeout}, true
	}
	if f.good {
		return FuzzAction{}, false
	}
	for _, key := range []string{f.stateKey(in), in.Class, in.State, in.State[strings.LastIndex(in.State, "/")+1:]} {
		if answer, ok := config.GateAnswers[key]; ok && !replay {
			raw, _ := json.Marshal(answer)
			f.result.Mix["answer"]++
			return FuzzAction{State: in.State, Kind: FuzzAnswer, Output: raw}, true
		}
	}
	n := len(f.result.Script)
	var action FuzzAction
	switch {
	case replay && n < len(script):
		action = script[n]
	case replay:
		return action, false
	case n-f.skew < len(config.Prefix):
		action = config.Prefix[n-f.skew]
	default:
		if config.OutcomeSwitches && f.result.Switches[f.stateKey(in)+"/timeout"] && f.timedGate(in) {
			action = FuzzAction{Kind: FuzzTimeout}
			break
		}
		if len(in.Answers) == 0 || f.choices.Intn(4) == 0 {
			return action, false
		}
		raw, _ := json.Marshal(in.Answers[f.choices.Intn(len(in.Answers))])
		action = FuzzAction{Kind: FuzzAnswer, Output: raw}
	}
	if action.Kind != FuzzAnswer && action.Kind != FuzzTimeout {
		return action, false
	}
	if action.Kind == FuzzTimeout && (!f.timedGate(in) || action.State != "" && action.State != in.State) {
		f.find("script-mismatch", "timeout must name an open gate with a declared deadline")
		return action, false
	}
	action.State = in.State
	if replay {
		f.result.Mix["replay"]++
	} else if n-f.skew < len(config.Prefix) {
		f.result.Mix["prefix"]++
	} else {
		f.result.Mix[string(action.Kind)]++
	}
	return action, true
}

// reservedTimeoutEnd recognizes a reserved decision boundary only after its
// declared timeout really transitions directly into the declared stopped terminal.
// A child settlement completes this gate's targeted liveness obligation;
// its parent's later outcome is outside that obligation. Other properties still run.
func (f *fuzzRun) reservedTimeoutEnd() bool {
	if f.timeoutGate == nil {
		return false
	}
	in := *f.timeoutGate
	state := f.schemas[f.stateKey(in)]
	if state.Reservation != "reserved" {
		return false
	}
	prefix := strings.TrimSuffix(f.stateKey(in), state.Name)
	for index, edge := range state.Edges {
		terminal := f.schemas[prefix+edge.To]
		if edge.Timeout <= 0 || terminal.Kind != "terminal" || terminal.Outcome != string(wf.Stopped) {
			continue
		}
		selected, traversed := false, false
		trace := f.result.Trace
		if trace == nil {
			// Liveness findings are raised only after an engine call returns,
			// including journal-bound errors, so the journal lock is released.
			trace = f.runner.trace()
		}
		records := trace.Journal()
		for _, record := range records {
			if traversed && record.Kind == wf.JournalChildSettled {
				var data wf.ChildSettledData
				// Stopped terminals retain their last failure class, defaulting
				// to Refused; Accepted is reserved for successful children.
				if json.Unmarshal(record.Data, &data) == nil && data.ID == in.Child && data.Terminal == edge.To && data.Outcome == wf.Stopped && !data.Cancelled && !data.Running {
					return true
				}
			}
			if record.Instance != in.Child {
				continue
			}
			switch record.Kind {
			case wf.JournalAutoAnswered:
				var data wf.AutoAnsweredData
				if record.State == state.Name && json.Unmarshal(record.Data, &data) == nil && data.Answer == "timeout" && data.Plan != nil && len(data.Plan.Steps) == 1 {
					step := data.Plan.Steps[0]
					selected = step.From == state.Name && step.Index == index && step.To == edge.To
				}
			case wf.JournalTransition:
				var data wf.TransitionData
				traversed = selected && record.State == state.Name && json.Unmarshal(record.Data, &data) == nil && data.To == edge.To
				selected = false
			case wf.JournalRunEnded:
				if traversed && record.State == edge.To {
					return true
				}
			}
		}
	}
	return false
}

// timedGate verifies both the declaration and the engine-issued deadline.
func (f *fuzzRun) timedGate(in wf.Instruction) bool {
	if in.Deadline.IsZero() {
		return false
	}
	state := f.schemas[f.stateKey(in)]
	if state.Kind != "gate" {
		return false
	}
	for _, edge := range state.Edges {
		if edge.Timeout > 0 {
			return true
		}
	}
	return false
}

// stillHanded reports whether the unit named name is still live in run
// (handed, started, running or stalled), so a held instruction for it may be
// acted on; a withdrawn or settled unit is not.
func (f *fuzzRun) stillHanded(run wf.RunID, name string) bool {
	st, err := f.engine.Status(run)
	if err != nil {
		return false
	}
	for _, u := range st.Units {
		if u.Name == name {
			switch u.State {
			case wf.UnitHanded.String(), wf.UnitStarted.String(), wf.UnitRunning.String(), wf.UnitStalled.String():
				return true
			}
			return false
		}
	}
	return false
}

// recoverAction is the coordinator's answer to a recovery offer: the
// script's on replay or from the prefix, else drawn from the choices the
// offer prints (restart three times in four, so seeds reach depth; never
// resume).
func (f *fuzzRun) recoverAction(in wf.Instruction, config FuzzConfig, script []FuzzAction, replay bool) (FuzzAction, bool) {
	n := len(f.result.Script)
	var action FuzzAction
	switch {
	case replay && n < len(script):
		action = script[n]
	case replay:
		return action, false
	case n-f.skew < len(config.Prefix) && config.Prefix[n-f.skew].Kind == FuzzRecover:
		action = config.Prefix[n-f.skew]
	case n-f.skew < len(config.Prefix):
		// A prefix that names no answer predates the offer: answer as the
		// engine acted before it (an exhaustion took its edge: give up; an
		// ended child was offered again: restart), recorded for replay.
		choice := wf.RecoveryGiveUp
		if in.Recovery != nil && in.Recovery.Transcript != "" {
			choice = wf.RecoveryRestart
		}
		raw, _ := json.Marshal(choice)
		action = FuzzAction{Kind: FuzzRecover, Output: raw}
		f.skew++
	default:
		// Never resume: the fuzzer's children are simulated, and none is
		// left to continue.
		choice := wf.RecoveryRestart
		if !f.good && f.choices.Intn(4) == 0 {
			choice = wf.RecoveryGiveUp
		}
		raw, _ := json.Marshal(choice)
		action = FuzzAction{Kind: FuzzRecover, Output: raw}
	}
	if action.Kind != FuzzRecover {
		return action, false
	}
	action.State = in.State
	f.result.Mix["recover"]++
	return action, true
}

// stateKey names the issued agent state by its definition path, the key of
// schemas, Valid and Reached. An unknown state keeps its instruction name.
func (f *fuzzRun) stateKey(in wf.Instruction) string {
	for _, key := range []string{in.Class, in.State} {
		if _, ok := f.schemas[key]; ok {
			return key
		}
	}
	return in.State
}

// valid returns the configured known-good outputs for the issued state,
// matched by its class, its qualified state or its last path segment.
func (f *fuzzRun) valid(in wf.Instruction) []json.RawMessage {
	return f.corpus(in, f.result.config.Valid)
}

func (f *fuzzRun) corpus(in wf.Instruction, corpus map[string][]json.RawMessage) []json.RawMessage {
	for _, key := range []string{f.stateKey(in), in.Class, in.State, in.State[strings.LastIndex(in.State, "/")+1:]} {
		if v, ok := corpus[key]; ok && key != "" {
			return v
		}
	}
	return nil
}

func (f *fuzzRun) find(rule, why string) { f.findAt(f.last.State, rule, why) }

// findAt records the first finding at state, a path relative to the root
// definition or already qualified by it.
func (f *fuzzRun) findAt(state, rule, why string) {
	if f.result.Finding != nil {
		return
	}
	// A proven reserved timeout boundary completes only this liveness
	// obligation. Keep every independent property finding observable.
	if rule == "liveness" && f.reservedTimeoutEnd() {
		return
	}
	if state == "" {
		state = f.sim.def.Name()
	}
	if !strings.Contains(state, "/") {
		state = f.sim.def.Name() + "/" + state
	}
	f.result.Finding = &FuzzFinding{Rule: rule, State: state, Why: why, Seed: f.result.Seed}
}
func (f *fuzzRun) engineError(err error) {
	if errors.Is(err, errFuzzBound) {
		if f.result.config.Liveness {
			f.find("liveness", "engine did not return within the journal bound")
		} else {
			f.find("loops-unbounded", "the engine must return within its journal step bound")
		}
	} else {
		f.find("engine-error", err.Error())
	}
}

// validate runs Validate, reporting a panic in the definition instead of
// raising it.
func (f *fuzzRun) validate(name string, output json.RawMessage) (res wf.SubmitResult, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	res, err = f.engine.Validate(f.runner.id, name, wf.Submission{Output: output})
	return res, err, false
}

// fuzzFixes is how many refused outputs a validating child replaces on one
// attempt before it runs done anyway.
const fuzzFixes = 2

func (f *fuzzRun) act(in wf.Instruction, a FuzzAction) error {
	if a.Kind == FuzzWait {
		if !f.result.config.Liveness {
			f.result.Suspended = true
		}
		f.open = &in
		return nil
	}
	transcript := filepath.Join(f.runner.dir, "transcript-"+strings.ReplaceAll(in.Name, "/", "_")+".jsonl")
	if f.fixes[in.Name] == 0 {
		if err := f.claim(&in, transcript); err != nil {
			return err
		}
	}
	if f.result.config.probe != nil {
		if err := f.writeArtifacts(in); err != nil {
			return err
		}
		f.result.config.probe(f.engine, f.runner.id, in)
	}
	return f.finish(in, a, transcript)
}

// claim starts the unit, fetches its prompt into in and reads its body, as a
// child does once per attempt.
func (f *fuzzRun) claim(in *wf.Instruction, transcript string) error {
	if err := os.WriteFile(transcript, []byte("{\"type\":\"assistant\"}\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chtimes(transcript, f.now, f.now); err != nil {
		return err
	}
	if err := f.engine.Started(f.runner.id, in.Name, wf.Claim{Harness: "claude", Session: "fuzz-session", Agent: in.Name, Transcript: transcript, Context: wf.ContextFresh}); err != nil {
		return err
	}
	fetched, err := f.engine.PromptFetch(f.runner.id, in.Name, "")
	if err != nil {
		return err
	}
	in.Prompt = fetched.Prompt
	in.PromptHash, in.PromptShapeHash = fetched.PromptHash, fetched.PromptShapeHash
	in.Inputs = fetched.Inputs
	// A real child reads the prompt body it was pointed at before done (F2).
	return f.readPromptBody(in.Name, transcript)
}

// finish takes the child's action a on the claimed unit in.
func (f *fuzzRun) finish(in wf.Instruction, a FuzzAction, transcript string) error {
	var err error
	if a.Kind == FuzzEndedWithoutDone {
		f.now = f.now.Add(10 * time.Second)
		if err = os.WriteFile(transcript, []byte("{\"type\":\"user\",\"toolEndsTurn\":true}\n"), 0o600); err != nil {
			return err
		}
		err = os.Chtimes(transcript, f.now, f.now)
		if err == nil {
			f.result.adversarialInjected = true
		}
		return err
	}
	var res wf.SubmitResult
	switch a.Kind {
	case FuzzTransient, FuzzBlocked:
		class := wf.FailTransient
		if a.Kind == FuzzBlocked {
			class = wf.FailBlocked
		}
		res, err = f.engine.FailedAs(f.runner.id, in.Name, "fuzz injected "+string(a.Kind), class)
		if err == nil {
			f.result.adversarialInjected = true
		}
	case FuzzSubmit, FuzzRefuse:
		if err = f.writeArtifacts(in); err != nil {
			return err
		}
		output := a.Output
		if a.Kind == FuzzRefuse {
			output = json.RawMessage(`"["`)
		}
		// Every submit is validated first (C5): validate must record nothing
		// and give the verdict done then gives.
		before := f.journal.records
		// A definition that panics on this output panics in validate first;
		// done then hits it with the submission journaled, the reproducer.
		var validated, submitted []string
		f.sim.world.observationValidation(true, true)
		defer f.sim.world.observationValidation(false, true)
		f.reads = &validated
		check, verr, panicked := f.validate(in.Name, output)
		f.sim.world.observationValidation(false, false)
		observationFault := f.sim.world.observationFaultPending()
		f.reads = nil
		if verr != nil {
			return verr
		}
		if f.journal.records != before {
			f.find("validate-wrote", "validate must not append to the journal")
		}
		if !observationFault && !panicked && a.Kind == FuzzSubmit && check.Class == wf.Refused && (f.good || (f.result.config.Fault != nil && !f.result.config.OutcomeSwitches)) {
			f.find("progress-context", fmt.Sprintf("declared progress output %s was refused in the reached context: %s; verify the corpus/context and acceptance check before claiming liveness", output, strings.Join(check.Refusal, "; ")))
			return nil
		}
		if !observationFault && !panicked && a.Kind == FuzzSubmit && check.Class == wf.Refused && f.fixes[in.Name] < fuzzFixes {
			// A validating child replaces a refused output before done: its
			// next action is for the same unit.
			f.fixes[in.Name]++
			f.result.Mix["fixed"]++
			held := in
			f.open = &held
			return nil
		}
		f.reads = &submitted
		res, err = f.engine.Done(f.runner.id, in.Name, wf.Submission{Output: output})
		if err == nil && a.Kind == FuzzRefuse {
			f.result.adversarialInjected = true
		}
		f.reads = nil
		// Done reads what validate read, then drives on: the same world is
		// the same prefix. A world that changed between them (a concurrent
		// writer) may rightly give another verdict.
		same := len(submitted) >= len(validated) && slices.Equal(submitted[:len(validated)], validated)
		if err == nil && !panicked && same && (check.Class != res.Class || !slices.Equal(check.Refusal, res.Refusal)) {
			f.find("validate-differs", fmt.Sprintf("validate must give done's verdict: validate %s %v, done %s %v", check.Class, check.Refusal, res.Class, res.Refusal))
		}
	default:
		return fmt.Errorf("unknown fuzz action %q", a.Kind)
	}
	if err != nil {
		return err
	}
	if a.Kind == FuzzSubmit && res.Class == wf.Accepted && f.actionContext != "" {
		if f.completedContexts == nil {
			f.completedContexts = map[string]map[string]bool{}
		}
		if f.completedContexts[in.Child] == nil {
			f.completedContexts[in.Child] = map[string]bool{}
		}
		f.completedContexts[in.Child][f.actionContext] = true
	}
	if res.Class == wf.Refused {
		// The fix line is what the engine tells the child next: the route a
		// failure takes, or the next state.
		fix := ""
		switch {
		case res.Failure != nil:
			fix = fmt.Sprintf("%+v", *res.Failure)
		case res.Next != "":
			fix = "the unit moves on to " + res.Next
		}
		f.result.Refusals = append(f.result.Refusals, FuzzRefusal{State: in.State, Reasons: res.Refusal, Fix: fix})
		if len(res.Refusal) == 0 {
			f.find("refusal-unnamed", "every refusal must name what to repair")
		} else if fix == "" {
			f.find("refusal-no-fix", "a refusal must say what happens next: an open re-submit, a failure route or a next state")
		}
	}
	return nil
}

// readPromptBody appends a Claude Read of the unit's latest fetched prompt
// body to its transcript, as the child's first tool call.
func (f *fuzzRun) readPromptBody(name, transcript string) error {
	records := f.runner.trace().Journal()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind != wf.JournalPromptFetched {
			continue
		}
		var d wf.PromptFetchedData
		if json.Unmarshal(records[i].Data, &d) != nil || d.Name != name || d.PromptDir == "" {
			continue
		}
		line, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_fuzz", "name": "Read", "input": map[string]string{"file_path": filepath.Join(d.PromptDir, "prompt.md")}}}}})
		if err != nil {
			return err
		}
		file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = file.Write(append(line, '\n'))
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		return os.Chtimes(transcript, f.now, f.now)
	}
	return nil
}

func (f *fuzzRun) writeArtifacts(in wf.Instruction) error {
	marker := "\n## Artifacts\n"
	i := strings.LastIndex(in.Prompt, marker)
	if i < 0 {
		return nil
	}
	block, _, _ := strings.Cut(in.Prompt[i+len(marker):], "\n## ")
	files := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		name, path, ok := strings.Cut(strings.TrimPrefix(line, "- "), ": ")
		if !ok {
			continue
		}
		path = strings.TrimSuffix(path, " (optional: leave it unwritten when there is nothing to record)")
		visits := 0
		for _, record := range f.runner.trace().Journal() {
			if record.Instance != in.Child || record.Kind != wf.JournalTransition {
				continue
			}
			var transition wf.TransitionData
			if json.Unmarshal(record.Data, &transition) == nil && transition.To == in.State[strings.LastIndex(in.State, "/")+1:] {
				visits++
			}
		}
		owner := fmt.Sprintf("%s/%s/visit-%d", in.Child, in.Class, visits)
		if old, ok := f.writers[path]; ok && old != owner {
			f.find("artifact-two-writers", "each allocated artifact path must have one producer instance")
			return nil
		}
		f.writers[path] = owner
		files[name] = fmt.Sprintf("{\"fuzzAttempt\":%d}\n", in.Attempt)
		for _, key := range []string{f.stateKey(in), in.Class, in.State[strings.LastIndex(in.State, "/")+1:]} {
			if content, ok := f.result.config.Artifacts[key][name]; ok {
				files[name] = content
				break
			}
		}
	}
	return f.runner.writeArtifacts(Step{files: files}, in)
}

func (f *fuzzRun) finishProperties() {
	r := f.result
	if r.BudgetReason == "" && r.Status.Running != 0 {
		f.find("unit-left-running", "every claimed unit must finish or be recovered before the seed stops")
		return
	}
	if !f.explainedStop() {
		return
	}
	if r.BudgetReason == "" && r.Status.BudgetStop == "" && r.Status.Phase != wf.RunEnded && f.last.Kind != wf.InstructionHumanGate && f.last.Kind != wf.InstructionManualVerification && f.last.Kind != wf.InstructionWaiting && f.last.Kind != wf.InstructionRecovery && len(r.Status.Fix) == 0 && !f.namedHalt(r.Status) {
		f.find("stall-unnamed", "the seed must end at a decision or a named failure")
		return
	}

	// Fold a copy of the journal in a new engine, without any mutable adapter
	// state or a prior in-memory fold. Status does not drive effects.
	journal := wf.MemJournal()
	unlock, err := journal.Lock(f.runner.id)
	if err != nil {
		f.engineError(err)
		return
	}
	for _, record := range r.Trace.Journal() {
		if err = journal.Append(record); err != nil {
			unlock()
			f.engineError(err)
			return
		}
	}
	unlock()
	replay := wf.NewEngine(journal, f.runner.reg, wf.EngineOptions{Assets: f.sim.assets, Defs: []*wf.Def{f.sim.def}, Clock: func() time.Time { return f.now }})
	status, err := replay.Status(f.runner.id)
	if err != nil {
		f.engineError(err)
		return
	}
	r.ReplayEqual = reflect.DeepEqual(status, r.Status)
	if !r.ReplayEqual {
		f.find("replay-diverged", "folding the recorded journal must produce the same state")
	}
}

// namedHalt requires the current blocked visit's actual halt record. A prior
// refusal cannot justify an unrelated instance that has stopped offering work.
func (f *fuzzRun) namedHalt(status wf.RunStatus) bool {
	if status.Phase != wf.RunBlocked {
		return false
	}
	records := f.runner.trace().Journal()
	for _, instance := range status.Instances {
		if instance.Phase != "blocked" || strings.TrimSpace(instance.Reason) == "" {
			continue
		}
		for i := len(records) - 1; i >= 0; i-- {
			record := records[i]
			if record.Instance != instance.ID || record.State == "" {
				continue
			}
			if record.State != instance.State || record.Kind == wf.JournalTransition || record.Kind == wf.JournalInstructionIssued {
				break
			}
			if record.Kind != wf.JournalRefused {
				continue
			}
			var halt wf.RefusedData
			if json.Unmarshal(record.Data, &halt) == nil && halt.Halt && strings.TrimSpace(strings.Join(halt.Reasons, "")) != "" {
				return true
			}
			break
		}
	}
	return false
}

// externalStop preserves holds and recognizes uncertain-effect settlement as
// a human boundary when the definition supplies no Uncertain route.
func (f *fuzzRun) externalStop(in wf.Instruction) {
	f.last = in
	if in.Kind == wf.InstructionManualVerification {
		w := f.sim.world
		w.mu.Lock()
		if p := w.fuzzUncertain; p != nil && p.at.State == f.stateKey(in) && p.at.Instance == in.Child {
			w.finishUncertain()
		}
		w.mu.Unlock()
	}
	f.last = in // finishing the injected fault may reset last to its state
	if f.manualUncertainBoundary(in) {
		f.result.Advisories = append(f.result.Advisories, fmt.Sprintf("no Uncertain route: uncertain effect at %s ends in manual verification", in.State))
		return
	}
	if f.result.config.Liveness && f.good {
		f.find("liveness", fmt.Sprintf("run stopped at %s, not success or a declared human gate", in.Kind))
	}
}

// recoveredFault runs inside World's lock for effects; World sets its own good
// phase after this callback returns, avoiding recursive mutex acquisition.
func (f *fuzzRun) recoveredFault(at wf.EffectContext, class string) bool {
	target := f.result.config.Fault
	if target == nil || f.result.FaultInjected || target.State != at.State || target.Class != class {
		return false
	}
	f.result.FaultInjected = true
	f.last = wf.Instruction{State: at.State}
	f.result.LivenessStart = f.step
	f.good = true
	return true
}

// drive catches only the scenario runner's intentional crash. The interrupted
// mutation is already durable; resume by driving a fresh fold, never repeating
// the original submission or gate answer.
func (f *fuzzRun) drive(call func()) bool {
	if !f.runner.guard(call) {
		return false
	}
	f.result.adversarialInjected = true
	f.reads = nil
	f.engine = wf.NewEngine(f.journal, f.runner.reg, f.engineOptions).(*wf.Engine)
	f.runner.e, f.runner.dead = f.engine, false
	return true
}

// Non-idempotent effects and idempotent commands reconcile without Check.
// Their real refusal/result records provide the corresponding recovery point.
func (f *fuzzRun) reconcileUncertainRecord(record wf.JournalRecord) {
	w := f.sim.world
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.fuzzUncertain
	if p == nil || p.at.Instance != record.Instance || !(p.at.State == record.State || strings.HasSuffix(p.at.State, "/"+record.State)) {
		return
	}
	if record.Kind == wf.JournalEffectResult || (record.Kind == wf.JournalRefused && record.Class == wf.Uncertain) {
		w.finishUncertain()
	}
}
