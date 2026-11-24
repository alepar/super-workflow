package wf

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// ManufactureScript is one manufactured state's script (MANUFACTURE.md):
// the path from the run's start to a target offer, one queue of steps per
// state. A predecessor script has Until "ended" and no target.
type ManufactureScript struct {
	Edge         string             `json:"edge,omitempty"`
	Registration string             `json:"registration"`
	TargetState  string             `json:"target_state,omitempty"`
	Hash         string             `json:"hash"`
	Until        string             `json:"until,omitempty"`
	Params       map[string]string  `json:"params,omitempty"`
	RunID        string             `json:"run_id,omitempty"`
	Target       *ManufactureTarget `json:"target,omitempty"`
	Steps        []ManufactureStep  `json:"steps"`
	Trigger      *ScriptStep        `json:"trigger,omitempty"`
	// Options is the run's options, as run --options gives them.
	Options json.RawMessage `json:"options,omitempty"`
	// Carried is every other field of the script, copied into the report.
	Carried map[string]json.RawMessage `json:"-"`
}

// ManufactureTarget narrows the target offer to an instance and a visit.
type ManufactureTarget struct {
	Instance string `json:"instance,omitempty"`
	Visit    int    `json:"visit,omitempty"`
}

// ManufactureStep is one step of a script's path: the state it answers, the
// edge that state is expected to take (nil for an engine-driven entry), and
// the step.
type ManufactureStep struct {
	State    string      `json:"state"`
	Edge     *string     `json:"edge"`
	Instance string      `json:"instance,omitempty"`
	Step     *ScriptStep `json:"step"`
}

// ScriptStep is a step in wftest's vocabulary (MANUFACTURE.md, Steps).
type ScriptStep struct {
	Op      string          `json:"op"`
	State   string          `json:"state,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Class   string          `json:"class,omitempty"`
	Times   int             `json:"times,omitempty"`
	Refusal string          `json:"refusal,omitempty"`
	Do      []ScriptCommand `json:"do,omitempty"`
	// Then, on the trigger only, answers the recovery offer the trigger's
	// failures lead to at the target visit (a recover step).
	Then *ScriptStep `json:"then,omitempty"`
}

// ScriptCommand is one world action a step runs before its submission.
type ScriptCommand struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir,omitempty"`
}

var scriptFields = map[string]bool{"edge": true, "registration": true, "target_state": true, "hash": true, "until": true, "params": true, "run_id": true, "target": true, "steps": true, "trigger": true, "options": true}

// ParseManufactureScript decodes a script, keeping its other fields as
// Carried, and checks its shape.
func ParseManufactureScript(raw []byte) (*ManufactureScript, error) {
	var s ManufactureScript
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("manufacture script: %w", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("manufacture script: %w", err)
	}
	for k, v := range all {
		if !scriptFields[k] {
			if s.Carried == nil {
				s.Carried = map[string]json.RawMessage{}
			}
			s.Carried[k] = v
		}
	}
	switch {
	case s.Registration == "" || s.Hash == "":
		return nil, fmt.Errorf("manufacture script: registration and hash are required")
	case s.Until != "" && s.Until != "ended":
		return nil, fmt.Errorf("manufacture script: until %q: only \"ended\" is supported", s.Until)
	case s.Until == "" && (s.Edge == "" || s.TargetState == ""):
		return nil, fmt.Errorf("manufacture script: edge and target_state are required (or until \"ended\" for a predecessor)")
	}
	for i, st := range s.Steps {
		if st.State == "" {
			return nil, fmt.Errorf("manufacture script: step %d has no state", i+1)
		}
		if st.Step != nil && st.Step.Op == "fail" && st.Step.Times < 0 {
			return nil, fmt.Errorf("manufacture script: step %d: times must be a non-negative int", i+1)
		}
		if st.Step != nil && st.Step.Then != nil {
			return nil, fmt.Errorf("manufacture script: step %d: then is applied only on the trigger", i+1)
		}
	}
	if t := s.Trigger; t != nil && t.Then != nil && (t.Op != "fail" || t.Then.Op != "recover" || t.Then.Then != nil) {
		return nil, fmt.Errorf("manufacture script: then is a fail trigger's, and must be one recover step")
	}
	if err := s.staticCommands(); err != nil {
		return nil, err
	}
	return &s, nil
}

// staticCommands is staticCommand for every world action of s.
func (s *ManufactureScript) staticCommands() error {
	for i, st := range s.Steps {
		if st.Step == nil {
			continue
		}
		for j, c := range st.Step.Do {
			if err := staticCommand(c.Argv); err != nil {
				return fmt.Errorf("manufacture script: step %d: do %d: %w", i+1, j, err)
			}
		}
	}
	for t := s.Trigger; t != nil; t = t.Then {
		for j, c := range t.Do {
			if err := staticCommand(c.Argv); err != nil {
				return fmt.Errorf("manufacture script: trigger: do %d: %w", j, err)
			}
		}
	}
	return nil
}

// staticCommand refuses a template where an expanded value would be
// parsed as code, or could change what is: the executable, and any shell
// (sh, bash, zsh, dash) call but one exact shape. A shell call with a
// template must be [shell, one option cluster such as -c, -ec or -eu (only
// the letters c, e, u, x and v, so no option takes a value), the literal
// program text, positional data...]: a value goes in as positional data
// (sh -c '... "$1"' x {{item.id}}), which the shell never parses.
func staticCommand(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	if templateVar.MatchString(argv[0]) {
		return fmt.Errorf("a template in the executable; pass values as positional arguments")
	}
	shell := filepath.Base(argv[0])
	switch shell {
	case "sh", "bash", "zsh", "dash":
	default:
		return nil
	}
	templated := false
	for _, a := range argv[1:] {
		templated = templated || templateVar.MatchString(a)
	}
	if !templated {
		return nil
	}
	if len(argv) >= 3 && shellCluster.MatchString(argv[1]) && !templateVar.MatchString(argv[2]) {
		return nil
	}
	return fmt.Errorf("a template in a %s call of another shape; use [%s, -c (or -ec, -eu), literal program, positional values...]: pass values as positional arguments, read as \"$1\"", shell, shell)
}

// shellCluster is the one option cluster a templated shell call may carry:
// it holds c and no letter that takes a value.
var shellCluster = regexp.MustCompile(`^-[euxv]*c[euxv]*$`)

// ManufactureReport is what a manufacture drive did (MANUFACTURE.md, Report).
type ManufactureReport struct {
	Edge             string                 `json:"edge,omitempty"`
	Registration     string                 `json:"registration"`
	Run              RunID                  `json:"run"`
	Until            string                 `json:"until,omitempty"`
	Reached          bool                   `json:"reached"`
	Target           *ManufactureReached    `json:"target,omitempty"`
	Ended            *ManufactureEnded      `json:"ended,omitempty"`
	TriggerApplied   bool                   `json:"trigger_applied,omitempty"`
	TriggerEdgeTaken string                 `json:"trigger_edge_taken,omitempty"`
	Divergence       *ManufactureDivergence `json:"divergence,omitempty"`
	RefusedStep      *ManufactureRefused    `json:"refused_step,omitempty"`
	Stuck            *ManufactureStuck      `json:"stuck,omitempty"`
	UnusedSteps      []string               `json:"unused_steps,omitempty"`
	NeedsStub        []string               `json:"needs_stub,omitempty"`
	Stubbed          bool                   `json:"stubbed"`
	CoverageTier     string                 `json:"coverage_tier,omitempty"`
	// Unverified are transitions whose edge the driver could not read (the
	// decision carried no edge plan): their step's edge is not claimed.
	Unverified []ManufactureUnverified `json:"unverified,omitempty"`
	// DoOnHost: world actions ran on the host, not under a sandbox (the
	// caller's --do-on-host).
	DoOnHost bool `json:"do_on_host,omitempty"`
	// GitConfigDropped are the fixture's local git settings left out of the
	// copy: each could run a program, a hook or a filter, or read another
	// file.
	GitConfigDropped []string                   `json:"git_config_dropped,omitempty"`
	Carried          map[string]json.RawMessage `json:"carried,omitempty"`
	Predecessors     []ManufactureReport        `json:"predecessors,omitempty"`
}

// ManufactureReached is the target offer: its instruction_issued seq, the
// checkpoint before it, and the unit.
type ManufactureReached struct {
	Seq         int64  `json:"seq"`
	SnapshotSeq int64  `json:"snapshot_seq,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Instance    string `json:"instance"`
}

// ManufactureEnded is how a predecessor (or a diverted run) ended.
type ManufactureEnded struct {
	Terminal string `json:"terminal"`
	Outcome  string `json:"outcome"`
	Cause    string `json:"cause,omitempty"`
}

// ManufactureDivergence is the first state that took another edge than the
// script's.
type ManufactureDivergence struct {
	State    string `json:"state"`
	Instance string `json:"instance"`
	Expected string `json:"expected"`
	Taken    string `json:"taken"`
	// NeedsStub: the world may decide the expected edge (a settle step, or a
	// compute whose state declares reads: an observation or a consumed
	// artifact), so a fixture fact or a stub may be needed.
	// NoDeclaredReads: a compute whose state declares no reads. What the run
	// already holds (earlier outputs, options, params, effect outcomes)
	// routes it, so look first at the script's earlier steps; the detector
	// knows only what the state declares.
	NeedsStub       bool `json:"needs_stub,omitempty"`
	NoDeclaredReads bool `json:"no_declared_reads,omitempty"`
}

// ManufactureUnverified is a transition a step expected an edge of, whose
// edge the journal does not say.
type ManufactureUnverified struct {
	State    string `json:"state"`
	Instance string `json:"instance"`
	Expected string `json:"expected"`
}

// ManufactureRefused is a submit step whose output the engine refused.
type ManufactureRefused struct {
	State   string   `json:"state"`
	Unit    string   `json:"unit"`
	Reasons []string `json:"reasons"`
}

// ManufactureStuck is an offer or instruction the script cannot answer.
type ManufactureStuck struct {
	State string `json:"state,omitempty"`
	Kind  string `json:"kind"`
	Why   string `json:"why"`
}

// ManufactureOptions configure Manufacture.
type ManufactureOptions struct {
	// ApplyTrigger applies the script's trigger at the exact target instance
	// and visit and requires that it take the script's edge.
	ApplyTrigger bool
	// Advance moves the engine's clock forward (the driver passes waits).
	Advance func(time.Duration)
	// Do runs one world action of a step: argv in dir, returning its
	// stdout. The caller supplies the boundary it runs in (a sandbox); with
	// none a script with world actions is refused.
	Do func(argv []string, dir string) (string, error)
	// MaxRounds bounds the drive; zero is 10000.
	MaxRounds int
}

// ScriptedHarness is the harness kind of the manufacture driver and its
// synthetic children. It is engine-internal: no environment resolves to it
// and no flag may name it, so no outside coordinator or agent can present
// it, and a manufactured run's segment, bound to it, refuses every other.
const ScriptedHarness = "scripted"

// ScriptDigest identifies a script's contents: every field, the carried
// ones included, in canonical JSON.
func ScriptDigest(s *ManufactureScript) string {
	all := map[string]any{}
	for k, v := range s.Carried {
		all[k] = v
	}
	b, _ := json.Marshal(s)
	var own map[string]json.RawMessage
	_ = json.Unmarshal(b, &own)
	for k, v := range own {
		all[k] = v
	}
	c, _ := json.Marshal(all)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(c))
}

// manufactureDriver is the in-process scripted coordinator of one drive.
type manufactureDriver struct {
	e    *Engine
	run  RunID
	s    *ManufactureScript
	o    ManufactureOptions
	rep  *ManufactureReport
	co   Coordinator
	n    int // synthetic children
	done bool

	queues   map[string][]int       // state path to the indexes of its queued steps
	used     map[string]int         // state path to queued steps used
	usedStep map[int]bool           // steps used, by index
	instLeft map[int]map[string]int // instance-matched steps' offers left, per instance (a fail's times)
	instN    map[int]int            // each instance-matched step's offers per instance

	defs map[string]string // instance to its definition path (display)
	// defObjs is each instance's definition, by the exact child names the
	// spawns carry, as the engine folds them (a name may hold '/').
	defObjs map[string]*Def
	seen    int // journal records read
	plans   map[string]*EdgePlan
	visits  map[string]int // instance and local state to completed visits
	issued  map[string]int64
	// inputs is each instance and local state's last issued inputs: a
	// handed-out unit instruction carries neither them nor its token, and
	// its templates read them.
	inputs map[string]map[string]string
	// expect is the step each answered offer consumed, by instance, local
	// state and visit: its transition is checked against that step's edge.
	expect map[string]*ManufactureStep
	// target is the target's instance, local state and visit once reached;
	// triggerLeft counts the trigger's remaining offers (a fail with times).
	target      string
	triggerLeft int
	thenLeft    bool // the trigger's then step is still to answer the target's recovery offer
	targetSeen  bool // the target's own transition was read
}

// Manufacture drives run from its start to the script's target offer (or,
// for a predecessor script, to its end), answering each offer from the
// script through the engine's own verbs. run must be a fresh manufactured
// run of this very script: its run_started carries the script's mark and no
// coordinator has driven it. The check and the binding note that makes the
// driver's session its only coordinator are one locked cycle, before any
// hand-out.
func (e *Engine) Manufacture(run RunID, s *ManufactureScript, o ManufactureOptions) (*ManufactureReport, error) {
	// A script built in code is held to what a parsed one is, before the run
	// is bound.
	if err := s.staticCommands(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	if o.MaxRounds == 0 {
		o.MaxRounds = 10000
	}
	if o.Advance == nil {
		o.Advance = func(time.Duration) {}
	}
	if o.Do == nil {
		for _, st := range s.Steps {
			if st.Step != nil && len(st.Step.Do) > 0 {
				return nil, fmt.Errorf("%w: manufacture: step %s has world actions and no boundary to run them in", ErrRejected, st.State)
			}
		}
		if s.Trigger != nil && len(s.Trigger.Do) > 0 {
			return nil, fmt.Errorf("%w: manufacture: the trigger has world actions and no boundary to run them in", ErrRejected)
		}
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	co := Coordinator{Harness: ScriptedHarness, Session: "manufacture-" + hex.EncodeToString(nonce[:])}
	if err := e.withRun(run, func(c *cycle) error { return c.bindManufacture(s, co) }); err != nil {
		return nil, err
	}
	if h := manufactureAdmittedHook; h != nil {
		h(run)
	}
	d := &manufactureDriver{e: e, run: run, s: s, o: o,
		rep:    &ManufactureReport{Edge: s.Edge, Registration: s.Registration, Run: run, Until: s.Until, Carried: s.Carried},
		co:     co,
		queues: map[string][]int{}, used: map[string]int{}, usedStep: map[int]bool{}, instLeft: map[int]map[string]int{}, instN: map[int]int{},
		defs: map[string]string{"": s.Registration}, defObjs: map[string]*Def{"": e.registeredDef(s.Registration)}, plans: map[string]*EdgePlan{},
		visits: map[string]int{}, issued: map[string]int64{}, inputs: map[string]map[string]string{}, expect: map[string]*ManufactureStep{}}
	for i, st := range s.Steps {
		n := 1
		if st.Step != nil && st.Step.Op == "fail" && st.Step.Times > 1 {
			n = st.Step.Times // a fail step answers that many successive offers
		}
		if st.Instance != "" {
			d.instN[i] = n // of each instance it matches
			continue
		}
		for k := 0; k < n; k++ {
			d.queues[st.State] = append(d.queues[st.State], i)
		}
	}
	if err := d.drive(); err != nil {
		return d.rep, err
	}
	d.finish()
	return d.rep, nil
}

// manufactureAdmittedHook, when set by a test, runs between the admission
// and the driver's first hand-out.
var manufactureAdmittedHook func(RunID)

// bindManufacture admits run to one manufacture drive and binds it, in one
// locked cycle: the run is manufactured for this very script (its mark's
// edge, until, definition hash and script digest), fresh (no coordinator,
// hand-out, submission or start), and its current harness segment is then
// bound to the driver's scripted coordinator. From then on the segment
// refuses every coordinator and agent of another harness, inside each
// verb's own lock, and only the engine presents scripted; a fork opens a
// new segment its own coordinator binds.
func (c *cycle) bindManufacture(s *ManufactureScript, co Coordinator) error {
	m := c.st.started.Manufactured
	switch {
	case m == nil:
		return fmt.Errorf("%w: manufacture: run %s is not a manufactured run", ErrRejected, c.run)
	case c.st.def.name != s.Registration:
		return fmt.Errorf("%w: manufacture: run %s is of %s, the script of %s", ErrRejected, c.run, c.st.def.name, s.Registration)
	case m.Hash != s.Hash || m.Edge != s.Edge || m.Until != s.Until || m.Script != ScriptDigest(s) || s.Hash != c.st.def.Hash():
		return fmt.Errorf("%w: manufacture: run %s was started for another script", ErrRejected, c.run)
	}
	for _, r := range c.recs {
		if r.Kind == JournalCoordinator || r.Kind == JournalHandedOut || r.Kind == JournalSubmitted || r.Kind == JournalAttemptStarted {
			return fmt.Errorf("%w: manufacture: run %s is not fresh (%s at seq %d)", ErrRejected, c.run, r.Kind, r.Seq)
		}
	}
	if cur := c.st.segment(); cur == nil || cur.Harness != "" {
		return fmt.Errorf("%w: manufacture: run %s has no unbound harness segment", ErrRejected, c.run)
	}
	return c.bindSegment(co)
}

func (d *manufactureDriver) drive() error {
	for round := 0; round < d.o.MaxRounds && !d.done; round++ {
		ins, err := d.e.HandOutAs(d.run, 64, d.co)
		if err != nil {
			return err
		}
		if err := d.scan(); err != nil || d.done {
			return err
		}
		acted := false
		for _, in := range ins {
			ok, err := d.answer(in)
			if err != nil {
				return err
			}
			acted = acted || ok
			if d.done {
				return nil
			}
			if err := d.scan(); err != nil || d.done {
				return err
			}
		}
		if !acted {
			why := "no instruction the script can answer"
			if len(ins) == 0 {
				why = "the engine offered nothing"
			}
			d.stuck("", "none", why)
		}
	}
	if !d.done {
		d.stuck("", "rounds", fmt.Sprintf("not reached in %d rounds", d.o.MaxRounds))
	}
	return nil
}

func (d *manufactureDriver) stuck(state, kind, why string) {
	if d.rep.Stuck == nil {
		d.rep.Stuck = &ManufactureStuck{State: state, Kind: kind, Why: why}
	}
	d.done = true
}

// statePath is the inventory's path of an instance's local state: the
// registration, each child definition's name, then the state.
func (d *manufactureDriver) statePath(instance, local string) string {
	return d.defs[instance] + "/" + local
}

// stateNode is the node of local state in instance's definition, or nil.
func (d *manufactureDriver) stateNode(instance, local string) *node {
	if def := d.defObjs[instance]; def != nil {
		return def.find(local)
	}
	return nil
}

// declaresReads reports whether n declares what it reads from the world: an
// observation or a consumed artifact.
func declaresReads(n *node) bool { return len(nodeReads(n)) > 0 || len(n.consumes) > 0 }

func localState(qualified string) string { return qualified[strings.LastIndex(qualified, "/")+1:] }

// visitKey names one visit of an instance's local state: the visit is its
// completed visits plus one.
func (d *manufactureDriver) visitKey(instance, local string) string {
	return fmt.Sprintf("%s\x00%s\x00%d", instance, local, d.visits[instance+"\x00"+local]+1)
}

// next is the step for an offer of state at instance (nil when none),
// whether the offer is the target, and records the step it consumed as the
// one the offer's transition is checked against.
func (d *manufactureDriver) next(state, instance, local string) (*ManufactureStep, bool) {
	key := d.visitKey(instance, local)
	if d.target != "" && key == d.target && d.triggerLeft > 0 {
		d.triggerLeft--
		return &ManufactureStep{State: state, Edge: &d.s.Edge, Step: d.s.Trigger}, false
	}
	st := d.instanceStep(state, instance)
	if st == nil {
		q := d.queues[state]
		if d.target == "" && d.s.Until == "" && state == d.s.TargetState && d.used[state] >= len(q) && d.targetInstance(instance, local) {
			d.target = key
			return nil, true
		}
		switch {
		case d.used[state] < len(q):
			i := q[d.used[state]]
			d.used[state]++
			d.usedStep[i] = true
			st = &d.s.Steps[i]
		case len(q) > 0:
			st = &d.s.Steps[q[len(q)-1]]
		}
	}
	if st != nil {
		d.expect[key] = st
	}
	return st, false
}

func (d *manufactureDriver) targetInstance(instance, local string) bool {
	t := d.s.Target
	if t == nil {
		return true
	}
	if t.Instance != "" && !globMatch(t.Instance, instance) {
		return false
	}
	return t.Visit == 0 || d.visits[instance+"\x00"+local]+1 == t.Visit
}

// globMatch matches an instance id against pattern, whose only wildcard is
// "*" (any run of characters): ids hold brackets, which stay literal.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == "" || pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// answer acts on one instruction; acted is false when it needed nothing.
func (d *manufactureDriver) answer(in Instruction) (acted bool, err error) {
	switch in.Kind {
	case InstructionAgent:
		if d.waits(in) {
			return false, nil
		}
		return true, d.agent(in)
	case InstructionHumanGate:
		if d.waits(in) {
			return false, nil
		}
		return true, d.gate(in)
	case InstructionStopped:
		if len(in.Answers) > 0 {
			// A gate reserved for a person: the script answers as the person.
			return true, d.gate(in)
		}
	case InstructionRecovery:
		local := localState(in.State)
		state := d.statePath(in.Child, local)
		var st *ManufactureStep
		if d.thenLeft && d.triggerLeft == 0 && d.visitKey(in.Child, local) == d.target {
			// The trigger's failures led here: its then step answers.
			d.thenLeft = false
			st = &ManufactureStep{State: state, Edge: &d.s.Edge, Step: d.s.Trigger.Then}
		} else {
			st, _ = d.next(state, in.Child, local)
		}
		if st == nil || st.Step == nil || st.Step.Op != "recover" {
			d.stuck(state, string(in.Kind), "a recovery offer and no recover step: "+firstLine(in.Prompt))
			return true, nil
		}
		v, err := stepString(st.Step.Value)
		if err != nil {
			return true, err
		}
		return true, d.coordinator().Recover(d.run, in.Name, v)
	case InstructionWait:
		wait := time.Duration(in.RetryAfterSeconds) * time.Second
		if wait <= 0 {
			wait = time.Second
		}
		d.o.Advance(wait)
		return true, nil
	case InstructionEnded:
		d.rep.Ended = &ManufactureEnded{Terminal: in.Terminal, Outcome: fmt.Sprint(in.Outcome), Cause: in.Cause}
		if d.s.Until == "ended" {
			d.rep.Reached = true
		}
		d.done = true
		return true, nil
	}
	d.stuck(in.State, string(in.Kind), "an instruction the script cannot answer: "+firstLine(in.Prompt))
	return true, nil
}

// waits reports whether in is a sibling of the pinned target (an offer of
// the target state at another instance or visit) with no step of its own:
// it is left unanswered while the target is pending, never given the
// trigger.
func (d *manufactureDriver) waits(in Instruction) bool {
	local := localState(in.State)
	state := d.statePath(in.Child, local)
	if d.target == "" || state != d.s.TargetState || d.visitKey(in.Child, local) == d.target {
		return false
	}
	for i, st := range d.s.Steps {
		if st.Instance == "" || st.State != state || !globMatch(st.Instance, in.Child) {
			continue
		}
		left, seen := d.instLeft[i][in.Child]
		if !seen || left > 0 {
			return false // it has a step of its own left
		}
	}
	return len(d.queues[state]) == 0
}

func firstLine(s string) string { s, _, _ = strings.Cut(s, "\n"); return s }

func stepString(raw json.RawMessage) (string, error) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("manufacture: step value %s is not a string", raw)
	}
	return v, nil
}

// reached records the target offer. Without ApplyTrigger the drive stops
// there; with it, the trigger must be applied to this very offer and its
// transition read (finish).
func (d *manufactureDriver) reached(in Instruction) bool {
	d.rep.CoverageTier = "model"
	d.rep.Target = &ManufactureReached{Unit: in.Name, Instance: in.Child, Seq: d.issued[in.Child+"\x00"+localState(in.State)]}
	if !d.o.ApplyTrigger {
		d.rep.Reached, d.done = true, true
		return false
	}
	if d.s.Trigger == nil {
		d.stuck(d.s.TargetState, "trigger", "--apply-trigger, and the script has no trigger")
		return false
	}
	d.rep.TriggerApplied = true
	d.thenLeft = d.s.Trigger.Then != nil
	d.triggerLeft = 1
	if d.s.Trigger.Op == "fail" && d.s.Trigger.Times > 1 {
		d.triggerLeft = d.s.Trigger.Times
	}
	return true
}

// gate answers one gate offer at its exact instance and visit.
func (d *manufactureDriver) gate(in Instruction) error {
	local := localState(in.State)
	state := d.statePath(in.Child, local)
	st, target := d.next(state, in.Child, local)
	if target {
		if !d.reached(in) {
			return nil
		}
		st, _ = d.next(state, in.Child, local)
	}
	if st == nil || st.Step == nil || st.Step.Op != "answer" {
		d.stuck(state, string(in.Kind), "no answer step for this gate")
		return nil
	}
	v, err := stepString(st.Step.Value)
	if err != nil {
		return err
	}
	return d.coordinator().AnswerVisit(d.run, GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: v})
}

// agent answers one agent offer: a synthetic child claims it and answers it
// by its step; the target stops the drive or, with ApplyTrigger, is answered
// by the trigger.
func (d *manufactureDriver) agent(in Instruction) error {
	local := localState(in.State)
	state := d.statePath(in.Child, local)
	st, target := d.next(state, in.Child, local)
	if target {
		if !d.reached(in) {
			return nil
		}
		st, _ = d.next(state, in.Child, local)
	}
	if st == nil || st.Step == nil {
		d.stuck(state, string(in.Kind), "no step for this agent state")
		return nil
	}
	step := st.Step
	if step.Op == "stall" {
		d.stuck(state, "stall", "stall needs the scripted harness's transcript (lands with harness segments)")
		return nil
	}
	d.n++
	// The synthetic child is an identified caller held to the engine's own
	// child-claim checks: it fetches with the brief's claim token, and its
	// done or failed goes through an engine of its own carrying that token
	// and identity, as a child's CLI call would; the shared engine's caller
	// is never changed.
	caller := harness.Caller{Harness: d.co.Harness, Session: d.co.Session, Agent: fmt.Sprintf("manufacture-%d", d.n), Is: harness.IsChild, FromEnv: true}
	claim := briefClaim(in.Brief)
	if _, err := d.e.PromptFetchWhole(d.run, in.Name, claim, caller); err != nil {
		return fmt.Errorf("manufacture: claiming %s: %w", in.Name, err)
	}
	child := d.childEngine(caller, claim)
	switch step.Op {
	case "submit", "refuse":
		out, err := d.render(in, step)
		if err != nil {
			kind := step.Op
			if errors.As(err, new(templateError)) {
				kind = "template"
			}
			d.stuck(state, kind, err.Error())
			return nil
		}
		res, err := child.Done(d.run, in.Name, Submission{Output: out})
		if err != nil {
			return fmt.Errorf("manufacture: done %s: %w", in.Name, err)
		}
		if res.Class == Refused && step.Op == "submit" {
			d.rep.RefusedStep = &ManufactureRefused{State: state, Unit: in.Name, Reasons: res.Refusal}
			d.done = true
		}
		if res.Class != Refused && step.Op == "refuse" {
			d.stuck(state, "refuse", "the engine accepted an output the script expected refused")
		}
	case "fail":
		class, err := ParseFailureClass(step.Class)
		if err != nil {
			return fmt.Errorf("manufacture: %s: %w", state, err)
		}
		if _, err := child.FailedAs(d.run, in.Name, "manufactured "+step.Class+" failure", class); err != nil {
			return fmt.Errorf("manufacture: failed %s: %w", in.Name, err)
		}
	default:
		d.stuck(state, step.Op, "op "+step.Op+" cannot answer an agent offer")
	}
	return nil
}

// coordinator is an engine over the same journal and adapters whose calls
// come from the driver's admitted scripted coordinator, never from the
// process that invoked the drive: its recover and gate answers pass the
// segment check, inside each verb's lock, as that coordinator's.
func (d *manufactureDriver) coordinator() *Engine {
	return d.childEngine(harness.Caller{Harness: d.co.Harness, Session: d.co.Session, Is: harness.IsSelf, FromEnv: true}, "")
}

// childEngine is an engine over the same journal and adapters whose calls
// come from caller with claim: one synthetic child's CLI.
func (d *manufactureDriver) childEngine(caller harness.Caller, claim string) *Engine {
	o := d.e.opts
	o.caller, o.childClaim = caller, claim
	return NewEngine(d.e.j, d.e.reg, o).(*Engine)
}

// briefClaim is the claim token a spawn brief carries ("--claim <token>").
func briefClaim(brief string) string {
	_, rest, ok := strings.Cut(brief, " --claim ")
	if !ok {
		return ""
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return ""
	}
	return strings.TrimRight(f[0], ",")
}

// render runs the step's world actions through the caller's boundary and
// expands its output's templates.
func (d *manufactureDriver) render(in Instruction, step *ScriptStep) (json.RawMessage, error) {
	// Instruction paths may carry the journal's root tokens: resolve them
	// against this engine's roots.
	roots := strings.NewReplacer(workspacePathToken, d.e.opts.Workspace, statePathToken, d.e.opts.StateRoot)
	// The offer's inputs are its issued record's; one the instruction
	// itself carries is the same offer's.
	inputs := map[string]string{}
	for k, v := range d.inputs[in.Child+"\x00"+localState(in.State)] {
		inputs[k] = v
	}
	for k, v := range in.Inputs {
		inputs[k] = v
	}
	vars := map[string]string{"workdir": roots.Replace(in.Workspace)}
	if w := inputs["workdir"]; w != "" {
		vars["workdir"] = roots.Replace(w)
	}
	for k, v := range inputs {
		vars["input."+k] = roots.Replace(v)
	}
	if raw := inputs["item"]; raw != "" {
		// Numbers stay as written: an integer id past float64's range
		// must reach its template exactly.
		var item map[string]any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&item) == nil {
			for k, v := range item {
				if s, ok := v.(string); ok {
					vars["item."+k] = s
				} else if n, ok := v.(json.Number); ok {
					vars["item."+k] = n.String()
				} else if b, err := json.Marshal(v); err == nil {
					vars["item."+k] = string(b)
				}
			}
		}
	}
	// One pass over the template: a value is never expanded again, and a
	// {{name}} that names no variable known at that point refuses the step
	// (templateError) before anything runs with it.
	var unknown []string
	expand := func(s string) string {
		return templateVar.ReplaceAllStringFunc(s, func(m string) string {
			if v, ok := vars[m[2:len(m)-2]]; ok {
				return v
			}
			unknown = append(unknown, m)
			return m
		})
	}
	for i, c := range step.Do {
		if len(c.Argv) == 0 {
			return nil, fmt.Errorf("do %d has no argv", i)
		}
		argv := make([]string, len(c.Argv))
		for j, a := range c.Argv {
			argv[j] = expand(a)
		}
		dir := expand(c.Dir)
		if len(unknown) > 0 {
			return nil, templateError{fmt.Sprintf("do %d: unknown template %s", i, strings.Join(unknown, ", "))}
		}
		if dir == "" {
			dir = vars["workdir"]
		}
		stdout, err := d.o.Do(argv, dir)
		if err != nil {
			return nil, fmt.Errorf("do %d (%s): %v", i, strings.Join(argv, " "), err)
		}
		vars[fmt.Sprintf("do.%d.stdout", i)] = strings.TrimSpace(stdout)
	}
	if strings.Contains(string(step.Output), "{{git.head}}") {
		head, err := d.gitHead(vars["workdir"])
		if err != nil {
			return nil, fmt.Errorf("{{git.head}} in %s: %v", vars["workdir"], err)
		}
		vars["git.head"] = head
	}
	if len(step.Output) == 0 {
		return nil, fmt.Errorf("a %s step with no output", step.Op)
	}
	var v any
	if err := json.Unmarshal(step.Output, &v); err != nil {
		return nil, err
	}
	v = expandJSON(v, expand)
	if len(unknown) > 0 {
		return nil, templateError{"output: unknown template " + strings.Join(unknown, ", ")}
	}
	return json.Marshal(v)
}

// templateVar is one {{name}} in a step's argv, dir or output.
var templateVar = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// templateError is a step whose template names no known variable: the
// drive stops as stuck, kind template.
type templateError struct{ why string }

func (e templateError) Error() string { return e.why }

// gitHead reads HEAD of dir through the git adapter's own observation.
func (d *manufactureDriver) gitHead(dir string) (string, error) {
	raw, err := d.e.reg.Read(d.e.opts.Workspace, ObservationDesc{Adapter: "git", Op: "resolve_commit", Args: map[string]any{"ref": "HEAD", "worktree": dir}})
	if err != nil {
		return "", err
	}
	var head string
	if err := json.Unmarshal(raw, &head); err != nil {
		return "", fmt.Errorf("git.resolve_commit: %s", raw)
	}
	return head, nil
}

func expandJSON(v any, expand func(string) string) any {
	switch t := v.(type) {
	case string:
		return expand(t)
	case []any:
		for i := range t {
			t[i] = expandJSON(t[i], expand)
		}
	case map[string]any:
		for k := range t {
			t[k] = expandJSON(t[k], expand)
		}
	}
	return v
}

// scan reads the journal records appended since the last scan: child
// definitions, offers, and every transition's edge, checked against the
// script.
func (d *manufactureDriver) scan() error {
	var recs []JournalRecord
	if err := d.e.withRecords(d.run, func(rs []JournalRecord) { recs = rs }); err != nil {
		return err
	}
	for _, r := range recs[min(d.seen, len(recs)):] {
		switch r.Kind {
		case JournalChildSpawned:
			var c ChildSpawnedData
			if json.Unmarshal(r.Data, &c) == nil {
				d.defs[c.ID] = d.defs[r.Instance] + "/" + c.Def
				if parent := d.defObjs[r.Instance]; parent != nil {
					d.defObjs[c.ID] = parent.child(c.Def)
				}
			}
		case JournalGateOffered:
			// A person gate's offer: its target is this record and the
			// checkpoint before it, as an agent's is its issue.
			d.issued[r.Instance+"\x00"+r.State] = r.Seq
		case JournalInstructionIssued:
			d.issued[r.Instance+"\x00"+r.State] = r.Seq
			var is InstructionIssuedData
			if json.Unmarshal(r.Data, &is) == nil {
				d.inputs[r.Instance+"\x00"+r.State] = is.Inputs
			}
		case JournalAccepted, JournalCompletedByObservation, JournalRefused, JournalRouteOffCandidates,
			JournalPromptOffCandidates, JournalAutoAnswered, JournalGateAnswered, JournalRetried:
			var p struct{ Plan *EdgePlan }
			if json.Unmarshal(r.Data, &p) == nil {
				d.plans[r.Instance] = p.Plan
			}
		case JournalTransition:
			plan := d.plans[r.Instance]
			delete(d.plans, r.Instance)
			d.transition(r, plan)
		}
	}
	d.seen = len(recs)
	return nil
}

// instanceStep takes one offer from the first step matching instance that
// has offers left for this instance (a fail with times answers that many of
// each matching instance's offers), nil when none.
func (d *manufactureDriver) instanceStep(state, instance string) *ManufactureStep {
	for i := range d.s.Steps {
		s := &d.s.Steps[i]
		if s.Instance == "" || s.State != state || !globMatch(s.Instance, instance) {
			continue
		}
		if d.instLeft[i] == nil {
			d.instLeft[i] = map[string]int{}
		}
		left, ok := d.instLeft[i][instance]
		if !ok {
			left = d.instN[i]
		}
		if left > 0 {
			d.instLeft[i][instance] = left - 1
			d.usedStep[i] = true
			return s
		}
	}
	return nil
}

// stepFor is the step a transition of a state with no offer (a compute,
// command or wait) is checked against: an instance's own unused step, else
// the state's next queued one, the last repeating.
func (d *manufactureDriver) stepFor(state, instance string) *ManufactureStep {
	if st := d.instanceStep(state, instance); st != nil {
		return st
	}
	q := d.queues[state]
	switch {
	case d.used[state] < len(q):
		i := q[d.used[state]]
		d.used[state]++
		d.usedStep[i] = true
		return &d.s.Steps[i]
	case len(q) > 0:
		return &d.s.Steps[q[len(q)-1]]
	}
	return nil
}

// transition checks one transition against the edge of the step its visit
// consumed, and notes the target's own transition and an engine target's
// arrival.
func (d *manufactureDriver) transition(r JournalRecord, plan *EdgePlan) {
	var t TransitionData
	if json.Unmarshal(r.Data, &t) != nil {
		return
	}
	from := d.statePath(r.Instance, r.State)
	key := d.visitKey(r.Instance, r.State)
	d.visits[r.Instance+"\x00"+r.State]++
	taken := ""
	if plan != nil && len(plan.Steps) > 0 && plan.Steps[0].From == r.State {
		taken = fmt.Sprintf("%s/edge[%d]", from, plan.Steps[0].Index)
	}
	if d.target != "" && key == d.target {
		d.targetSeen = true
		d.rep.TriggerEdgeTaken = taken
		if taken == "" {
			d.rep.Unverified = append(d.rep.Unverified, ManufactureUnverified{State: from, Instance: r.Instance, Expected: d.s.Edge})
		}
		d.done = true
		return
	}
	if d.done {
		return
	}
	st, offered := d.expect[key]
	delete(d.expect, key)
	if !offered {
		st = d.stepFor(from, r.Instance)
	}
	if st != nil && st.Edge != nil {
		switch {
		case taken == "":
			d.rep.Unverified = append(d.rep.Unverified, ManufactureUnverified{State: from, Instance: r.Instance, Expected: *st.Edge})
		case taken != *st.Edge:
			div := &ManufactureDivergence{State: from, Instance: r.Instance, Expected: *st.Edge, Taken: taken}
			if st.Step != nil {
				switch st.Step.Op {
				case "settle":
					div.NeedsStub = true
				case "compute":
					if n := d.stateNode(r.Instance, r.State); n != nil && !declaresReads(n) {
						div.NoDeclaredReads = true
					} else {
						div.NeedsStub = true
					}
				}
			}
			d.rep.Divergence = div
			d.done = true
			return
		}
	}
	if d.s.Until == "" && d.target == "" && plan != nil && len(plan.Steps) > 1 {
		// A visit budget replaced this transition's target: each later plan
		// step leaves a state the run arrived at without a transition of its
		// own, so no offer was made there. Arriving at the target that way
		// is reached only by a visit trigger (the budget is the trigger),
		// and the step's edge is the edge it took; an offer trigger never
		// ran, and the drive stops there, not reached.
		for _, step := range plan.Steps[1:] {
			path := d.statePath(r.Instance, step.From)
			if path != d.s.TargetState || !d.targetInstance(r.Instance, step.From) {
				continue
			}
			if d.s.Trigger == nil || d.s.Trigger.Op != "visit" {
				d.stuck(path, "budget", "the visit budget took the target's arrival, so its offer was never made and the script's trigger never ran (a budget edge's trigger is visit)")
				return
			}
			d.rep.CoverageTier = "engine"
			d.rep.Target = &ManufactureReached{Instance: r.Instance, Seq: r.Seq}
			d.done = true
			if !d.o.ApplyTrigger {
				d.rep.Reached = true
				return
			}
			d.rep.TriggerApplied, d.targetSeen = true, true
			d.rep.TriggerEdgeTaken = fmt.Sprintf("%s/edge[%d]", path, step.Index)
			return
		}
	}
	if d.s.Until == "" && d.s.TargetState != "" && d.statePath(r.Instance, t.To) == d.s.TargetState && !d.offerState() && d.targetInstance(r.Instance, t.To) {
		// An engine state (compute, command, wait) is the target: the run
		// has arrived; with ApplyTrigger the drive goes on to its edge.
		d.rep.CoverageTier = "engine"
		d.rep.Target = &ManufactureReached{Instance: r.Instance, Seq: r.Seq}
		if !d.o.ApplyTrigger {
			d.rep.Reached, d.done = true, true
			return
		}
		d.rep.TriggerApplied = true
		if d.target == "" {
			d.target = d.visitKey(r.Instance, t.To)
		}
	}
}

// offerState reports whether the script's target is answered through an
// offer (an agent or a gate), which the offer itself marks.
func (d *manufactureDriver) offerState() bool {
	if d.s.Trigger == nil {
		return false
	}
	switch d.s.Trigger.Op {
	case "submit", "refuse", "fail", "stall", "answer":
		return true
	}
	return false
}

// finish settles the verdict once the drive has stopped, failing closed: a
// requested trigger counts only when it ran on the exact target visit and
// that visit's transition took the script's edge.
func (d *manufactureDriver) finish() {
	if d.o.ApplyTrigger && d.s.Until == "" {
		d.rep.Reached = d.rep.TriggerApplied && d.targetSeen && d.rep.TriggerEdgeTaken != "" && d.rep.TriggerEdgeTaken == d.s.Edge
	}
	if t := d.rep.Target; t != nil && t.SnapshotSeq == 0 && t.Seq > 1 {
		_ = d.e.withRecords(d.run, func(rs []JournalRecord) {
			if prev := rs[t.Seq-2]; prev.Kind == JournalWorldSnapshot && prev.Seq+1 == t.Seq {
				var w WorldSnapshotData
				if json.Unmarshal(prev.Data, &w) == nil && w.Unavailable == "" {
					t.SnapshotSeq = prev.Seq
				}
			}
		})
	}
	for i, st := range d.s.Steps {
		if d.usedStep[i] || st.Step == nil {
			continue
		}
		switch st.Step.Op {
		case "submit", "refuse", "fail", "stall", "answer", "recover":
			d.rep.UnusedSteps = appendOnce(d.rep.UnusedSteps, st.State)
		}
	}
	if div := d.rep.Divergence; div != nil && div.NeedsStub {
		d.rep.NeedsStub = appendOnce(d.rep.NeedsStub, div.State)
	}
}

func appendOnce(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// registeredDef is the definition registered under name, or nil.
func (e *Engine) registeredDef(name string) *Def {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.defs[name]
}
