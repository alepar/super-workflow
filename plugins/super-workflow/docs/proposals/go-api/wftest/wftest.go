// Package wftest is the simulation API for workflow definitions: canned
// submission scripts, a fake world of adapters, crash injection and assertions
// over the journal and the route taken.
//
// Sim.Run drives the real wf.Engine over wf.MemJournal and a registry whose
// one runtime is the World, so a scenario exercises the same fold, checks,
// routes, effects and crash recovery a real run does.
package wftest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Sim runs one definition against a script of steps and a fake world.
//
// Owns: the definition under test, its typed options and params, the World of
// fakes and the testing.TB a script mismatch is reported to.
// Does not: execute anything itself; Run drives wf.Engine.
// Extend by: a new knob is an Option constructor.
type Sim struct {
	journalCheck func([]wf.JournalRecord, wf.PathRoots)
	workspace    string
	assets       string
	def          *wf.Def
	options      any
	params       map[string]string
	world        *World
	tb           testing.TB
	runner       wf.EffectRunner
	clock        func() time.Time // WithClock: the engine's clock for wave play
	cap          int              // Cap: the paged coordinator's harness cap; 0 is wave play
	recover      string           // WithRecovery: how the simulated coordinator answers a recovery offer no Recover step names
	recoverSet   bool
	paged        bool // PagedPrompts: the run starts with paged prompt delivery
}

// Option configures a Sim. It is sealed: build one with the With constructors.
type Option struct{ apply func(*Sim) }

// New builds a Sim for d. With no options the run has zero-valued options, no
// params, an empty World, and the current directory as workspace. Each Run
// has a temporary run directory; WithTB
// keeps it until test cleanup, otherwise Run removes it before returning.
func New(d *wf.Def, opts ...Option) *Sim {
	workspace, err := os.Getwd()
	if err != nil {
		panic("wftest workspace: " + err.Error())
	}
	s := &Sim{def: d, world: NewWorld(), workspace: workspace}
	for _, o := range opts {
		o.apply(s)
	}
	return s
}

// WithOptions sets the run's typed RunOptions value (the definition's options
// struct).
func WithOptions(v any) Option { return Option{func(s *Sim) { s.options = v }} }

// WithAssets selects a fixture asset root without making it a run parameter.
func WithAssets(root string) Option { return Option{func(s *Sim) { s.assets = root }} }

// WithWorkspace selects the simulated run workspace for path ownership checks.
func WithWorkspace(root string) Option {
	return Option{func(s *Sim) {
		if root == "" {
			root = "."
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			panic("wftest workspace: " + err.Error())
		}
		s.workspace = absolute
	}}
}

// WithParams sets the run parameters by name.
func WithParams(p map[string]string) Option { return Option{func(s *Sim) { s.params = p }} }

// WithRecovery sets how the simulated coordinator answers a recovery offer
// (B2, F1) that no Recover step names: wf.RecoveryGiveUp (the default: the
// unit takes its failure route), wf.RecoveryRestart, or "" to leave offers
// open so the script's own Recover steps decide each one.
func WithRecovery(choice string) Option {
	return Option{func(s *Sim) { s.recover = choice; s.recoverSet = true }}
}

// WithWorld sets the fake adapters the run reads and writes.
func WithWorld(w *World) Option { return Option{func(s *Sim) { s.world = w }} }

// WithEffectRunner runs the definition's .Detached() commands under r (a
// FakeRunner) instead of inline. Result files use the Sim's temporary run directory.
func WithEffectRunner(r wf.EffectRunner) Option { return Option{func(s *Sim) { s.runner = r }} }

// WithClock sets the engine's clock for the wave play, so a time-bound effect
// (a detached command's maximum duration, measured from its intent's stamp) is
// scripted without sleeping. With Cap the coordinator's clock still wins.
func WithClock(now func() time.Time) Option { return Option{func(s *Sim) { s.clock = now }} }

// Cap makes the Sim play a paged coordinator with a harness cap of n running
// subagents (n >= 1) instead of the wave play of Next and Submit. The
// coordinator has a session id ("wftest-1", then "wftest-2" after each
// NewCoordinator), picks its own n from that cap and the units it holds and
// passes it to the engine's hand-out, starts, prompts and finishes
// each unit through the Started, Prompt and Done verbs, and runs on a fake clock
// and a fake transcript mtime the Sim owns, so windows, stalls and losses are
// scripted without sleeping.
func Cap(n int) Option {
	if n < 1 {
		panic("wftest: Cap needs n >= 1")
	}
	return Option{func(s *Sim) { s.cap = n }}
}

// WithTB sets the testing.TB a script mismatch or engine error is reported to
// (Errorf, after which Run returns the Trace so far). Without one Run panics
// with the same message.
func WithTB(tb testing.TB) Option { return Option{func(s *Sim) { s.tb = tb }} }

type stepKind int

const (
	stepSubmit stepKind = iota + 1
	stepAnswer
	stepFail
	stepSettle
	stepRecover
	stepValidate
	stepCrash
	stepCrashAt
	stepResume
	stepKill // the steps from here on drive the paged coordinator and need Cap
	stepNewCoordinator
	stepStall
	stepLoseSession
)

// Step is one canned submission or injected event in a script. It is sealed:
// build one with Submit, SubmitAt, SubmitTimes, ValidateAt, Answer, AnswerAt, Settle,
// SettleExit, CrashAfterIntent, CrashAt, Resume, KillCoordinator, NewCoordinator, Stall
// or LoseSession.
type Step struct {
	kind    stepKind
	path    string // state path (qualified suffix) for submit and answer
	desc    string // how the step reads in a mismatch message
	files   map[string]string
	failure wf.FailureClass
	reason  string
	value   any           // canned output, or the answer as a string
	applied bool          // settle
	result  *wf.RunResult // SettleExit: what the human observed of the command
	times   int
	adapter string
	op      string
	crashes []Step // crash steps that follow this action step in the script
	at      KillPoint
	jk      wf.JournalKind // CrashAt: the record kind
	// Fetching: the pages the subagent fetches before a Submit or Fail in a
	// paged Sim; fetchSet with no fetches fetches nothing more.
	fetches  []Fetch
	fetchSet bool
}

// markerName is the state name derived from the owning marker type S.
func markerName[S any]() string { return reflect.TypeFor[S]().Name() }

// Submit scripts the agent a answering its next instruction with out.
func Submit[S, O any](a *wf.Agent[S, O], out O) Step {
	return SubmitTimes(a, 1, out)
}

// SubmitTimes scripts the agent a answering with the same out n times; with
// identical outputs the attempts make no progress, which is how a test reaches
// a stalled route.
func SubmitTimes[S, O any](a *wf.Agent[S, O], n int, out O) Step {
	name := markerName[S]()
	return Step{kind: stepSubmit, path: name, desc: fmt.Sprintf("Submit at %q", name), value: out, times: n}
}

// SubmitAt scripts the agent at the state path answering its next instruction
// with out. The path matches the instruction's qualified state by suffix, so
// "Merge" matches "task/Merge". It is for states whose marker types the test
// cannot name.
func SubmitAt(path string, out any) Step {
	return Step{kind: stepSubmit, path: path, desc: fmt.Sprintf("SubmitAt %q", path), value: out, times: 1}
}

// ValidateAt scripts the agent at the state path checking out with validate
// (C5), as a child does before done: nothing is journaled and no attempt is
// spent. Trace.Validations has the verdict.
func ValidateAt(path string, out any) Step {
	return Step{kind: stepValidate, path: path, desc: fmt.Sprintf("ValidateAt %q", path), value: out, times: 1}
}

// Validation is one ValidateAt's verdict on the state it validated.
type Validation struct {
	State  string
	Result wf.SubmitResult
}

// Answer scripts a human answering gate g with a.
func Answer[S any, A comparable](g *wf.Gate[S, A], a A) Step {
	return AnswerAt(markerName[S](), fmt.Sprint(a))
}

// AnswerAt scripts a human answering the open gate at the state path with
// value (matched by qualified suffix, as in SubmitAt).
func AnswerAt(path, value string) Step {
	return Step{kind: stepAnswer, path: path, desc: fmt.Sprintf("AnswerAt %q = %q", path, value), value: value, times: 1}
}

// Settle scripts the settlement of the open manual-verification instruction:
// applied true continues the run, false classes the step Blocked. A command's
// routing reads its exit code, so settling one as applied is SettleExit.
func Settle(applied bool) Step {
	return Step{kind: stepSettle, applied: applied, desc: fmt.Sprintf("Settle(applied=%v)", applied), times: 1}
}

// SettleExit scripts the settlement of the open manual verification of a
// command as applied with the exit code and stdout a human observed.
func SettleExit(code int, stdout string) Step {
	return Step{kind: stepSettle, applied: true, result: &wf.RunResult{Exit: code, Stdout: stdout},
		desc: fmt.Sprintf("SettleExit(%d, %q)", code, stdout), times: 1}
}

// Recover answers the open recovery offer of the unit at path (a state path,
// matched like Submit's) with choice: wf.RecoveryResume, wf.RecoveryRestart
// or wf.RecoveryGiveUp.
func Recover(path, choice string) Step {
	return Step{kind: stepRecover, path: path, value: choice, desc: fmt.Sprintf("Recover(%q, %s)", path, choice), times: 1}
}

// CrashAfterIntent kills the engine after the effect (adapter, op) has
// journaled its intent and before its result, so the outcome is uncertain. It
// arms the crash for the action step before it in the script (the Submit,
// Answer or Settle whose cycle applies the effect), or, when it follows no
// action, for the engine's next drive.
func CrashAfterIntent(adapter, op string) Step {
	return Step{kind: stepCrash, adapter: adapter, op: op, desc: fmt.Sprintf("CrashAfterIntent(%s.%s)", adapter, op)}
}

// CrashAt kills the engine right after a record of kind has been appended: the
// record is durable and the rest of the cycle is lost, as in a process death
// after that write. Like CrashAfterIntent it arms the action step before it in
// the script, or, when it follows no action, the engine's next drive; at the
// start of the script with kind run_started it kills the engine inside Run.
func CrashAt(kind wf.JournalKind) Step {
	return Step{kind: stepCrashAt, jk: kind, desc: fmt.Sprintf("CrashAt(%s)", kind)}
}

// Resume restarts the engine on the same journal.
func Resume() Step { return Step{kind: stepResume, desc: "Resume()"} }

// KillPoint is a point of the attempt state machine where the coordinator dies.
type KillPoint int

// Kill points, in the order a unit passes them.
const (
	AfterNext    KillPoint = iota + 1 // units handed out, none started
	AfterSpawn                        // the next unit spawned, not started
	AfterStarted                      // started, prompt not fetched
	WhileRunning                      // started and fetched
	AfterDone                         // a done journaled, wait not returned
)

var killPointNames = [...]string{"", "AfterNext", "AfterSpawn", "AfterStarted", "WhileRunning", "AfterDone"}

// String returns the constant's name.
func (k KillPoint) String() string {
	if k >= AfterNext && int(k) < len(killPointNames) {
		return killPointNames[k]
	}
	return fmt.Sprintf("KillPoint(%d)", int(k))
}

// KillCoordinator kills the paged coordinator at the point of its next unit.
// The first four points fire in this step: the coordinator hands out up to its
// own cap and dies at the point on the first unit it hands out (the units
// after it stay handed and unprocessed). AfterDone arms the death for the next
// Submit, which dies right after its Done is journaled. The engine is untouched:
// whatever the dead session held is recovered by NewCoordinator and the clock.
// It needs Cap.
func KillCoordinator(at KillPoint) Step {
	return Step{kind: stepKill, at: at, desc: fmt.Sprintf("KillCoordinator(%s)", at)}
}

// NewCoordinator starts a fresh coordinator session. For each dead session it
// reports Heartbeat(died, Session: old), which loses that session's orphans
// without spending a retry, and then runs the paged loop. It needs Cap.
func NewCoordinator() Step { return Step{kind: stepNewCoordinator, desc: "NewCoordinator()"} }

// Stall makes the running unit's transcript stop changing and passes the clock
// beyond its stall threshold. name is the unit name or a state path (matched by
// suffix, as in SubmitAt). The engine reports the stall, the coordinator
// answers it with Heartbeat(active), and the unit goes on: no loss. It needs
// Cap.
func Stall(name string) Step {
	return Step{kind: stepStall, path: name, desc: fmt.Sprintf("Stall(%q)", name)}
}

// LoseSession makes the current coordinator session and its subagents vanish
// now. The units it held stay claimed in the engine until NewCoordinator reports
// the session died. It needs Cap.
func LoseSession() Step { return Step{kind: stepLoseSession, desc: "LoseSession()"} }

// crashSignal is the panic a World raises from Apply for an armed crash.
type crashSignal struct{ adapter, op string }

// World is the fake runtime behind a Sim: canned read results and scripted
// effect outcomes. It implements wf.AdapterRuntime for every op ("*").
//
// Owns: what reads return (keyed by canonical JSON of adapter, op and args), which
// reads are unavailable or stale, command exit codes, how effects report their
// prior state and which applied effect crashes the engine.
// Does not: model adapter behaviour beyond what a test scripts. It holds
// resolved descriptors: a test sets bd.Ready("e1"), not bd.Ready("{epic}").
// Extend by: a new fake behaviour is one function here.
type World struct {
	fuzzResult          func(wf.EffectContext, wf.ExternalEffect) (json.RawMessage, bool)
	fuzzInjected        func() // invoked only for an actual returned fault, never dry validation
	fuzzObservation     func(wf.EffectContext, wf.ObservationDesc) string
	observationProbe    bool
	observationSnapshot map[string][]observationFaultSnapshot

	fuzzUncertain *fuzzUncertain
	fuzzPrelude   bool
	fuzzGood      bool
	fuzzFault     func(wf.EffectContext, wf.ExternalEffect) string
	fuzzRecover   func(wf.EffectContext, string) bool
	mu            sync.Mutex
	reads         map[string]json.RawMessage
	down          map[string]bool
	stale         map[string]int // reads served so far, for keys that go stale
	exits         []exitRule
	outs          []outRule
	effects       map[string]wf.EffectStatus // adapter.op|keyID to scripted check answer
	applied       []wf.ExternalEffect
	crash         map[string]bool   // adapter.op to armed
	failing       map[string]string // adapter.op to the error its Apply returns
	failIf        []func(wf.ExternalEffect) string
	onRead        func(wf.ObservationDesc) (json.RawMessage, bool)
	onApply       func(wf.ExternalEffect)
	missing       []string                     // read keys the world had no value for, in order
	unknown       map[string]string            // adapter.op to the error a read with no value returns
	real          map[string]wf.AdapterRuntime // adapter.op to the Real runtime that performs it
	failed        map[int]bool                 // indexes into applied whose Apply a script failed or whose command exited non-zero
}

// failedApply marks the effect just applied as failed by a script (FailApply,
// FailApplyIf, or a command scripted to exit non-zero): it stays in Applied,
// but Check does not count it as done.
func (w *World) failedApply(msg string) error {
	if w.failed == nil {
		w.failed = map[int]bool{}
	}
	w.failed[len(w.applied)-1] = true
	return errors.New(msg)
}

type exitRule struct {
	prefix []string
	code   int
	once   bool // spent by the first command it answers
}

type outRule struct {
	prefix []string
	text   string
}

// NewWorld returns an empty World.
func NewWorld() *World {
	return &World{
		reads:   map[string]json.RawMessage{},
		down:    map[string]bool{},
		stale:   map[string]int{},
		effects: map[string]wf.EffectStatus{},
		crash:   map[string]bool{},
		failing: map[string]string{},
	}
}

// Set makes the read o return v.
func Set[T any](w *World, o wf.Observation[T], v T) {
	b, err := json.Marshal(v)
	if err != nil {
		panic("wftest: Set: " + err.Error())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads[readKey(o.Desc())] = b
}

// Unavailable makes the read o classified unavailable.
func Unavailable(w *World, o wf.Reader) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.down[readKey(o.Desc())] = true
}

// Stale makes o return a new value on every read, so a declared observation
// that changes between the instruction being issued and the submission arriving
// classifies the submission stale. The values are the JSON strings "stale-1",
// "stale-2", ..., so it suits string-valued observations such as git.Head.
func Stale(w *World, o wf.Reader) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stale[readKey(o.Desc())] = 0
}

// readKey identifies a read by the canonical JSON of its adapter, op and args
// (Args arrive as generic JSON after binding, so the key is built from JSON,
// not from the Go values).
func readKey(d wf.ObservationDesc) string {
	b, err := json.Marshal(struct {
		Adapter, Op string
		Args        map[string]any
	}{d.Adapter, d.Op, d.Args})
	if err != nil {
		panic("wftest: read key: " + err.Error())
	}
	return string(b)
}

// OnRead installs a hook consulted for every read after the unavailable and
// stale scripts and before the canned values; ok false falls through to them.
// It is how a stateful fake answers (a tracker whose bd.ready changes after bd.close).
func OnRead(w *World, f func(d wf.ObservationDesc) (json.RawMessage, bool)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onRead = f
}

// WrapRead replaces the world's read hook with wrap(previous), so a test can
// layer a variant over a fixture's own hook. previous answers false when the
// world had no hook.
func WrapRead(w *World, wrap func(next func(wf.ObservationDesc) (json.RawMessage, bool)) func(wf.ObservationDesc) (json.RawMessage, bool)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	next := w.onRead
	if next == nil {
		next = func(wf.ObservationDesc) (json.RawMessage, bool) { return nil, false }
	}
	w.onRead = wrap(next)
}

// OnApply installs a hook that sees every effect the world applies, after it
// is recorded. A hook that changes what OnRead returns makes the fake stateful.
func OnApply(w *World, f func(e wf.ExternalEffect)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onApply = f
}

// Exit scripts the exit code of a command whose argv starts with argvPrefix; an
// empty prefix matches every command. The latest matching Exit wins, and a
// command no Exit matches exits 0.
func Exit(w *World, code int, argvPrefix ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.exits = append(w.exits, exitRule{prefix: argvPrefix, code: code})
}

// ExitOnce is Exit for the next matching command only: that command exits with
// code, and later ones fall back to the earlier rules (exit 0 by default). It
// scripts a step that fails once and then succeeds, such as a merge check that
// misses until the task is rebased.
func ExitOnce(w *World, code int, argvPrefix ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.exits = append(w.exits, exitRule{prefix: argvPrefix, code: code, once: true})
}

// Stdout scripts the stdout of a command whose argv starts with argvPrefix; the
// latest matching Stdout wins and a command no Stdout matches prints nothing.
// It is how a test answers a script whose route reads its output.
func Stdout(w *World, text string, argvPrefix ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.outs = append(w.outs, outRule{prefix: argvPrefix, text: text})
}

// UnknownFails makes a read of (adapter, op) the world has no value for fail
// with msg, as the real adapter fails on a value that does not exist (a ref
// git cannot resolve). Without it such a read is a gap in the fixture, which
// the fuzz driver reports.
func UnknownFails(w *World, adapter, op, msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.unknown == nil {
		w.unknown = map[string]string{}
	}
	w.unknown[adapter+"."+op] = msg
}

// FailApply makes every Apply of the effect (adapter, op) fail with msg, as an
// adapter whose tool exited non-zero would. The failed effect is still recorded
// in Applied. A command (shell.run) fails by Exit instead.
func FailApply(w *World, adapter, op, msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failing[adapter+"."+op] = msg
}

// FailApplyIf makes an Apply fail with the message f returns for it, as a
// tool that refused this one call would; "" lets it apply. f runs under the
// world's lock, once per Apply, so it can refuse a call the first time only
// or refuse what the world's own state says the tool would (a tracker
// refusing an edge onto a bead's descendant). The failed effect is still
// recorded in Applied.
func FailApplyIf(w *World, f func(e wf.ExternalEffect) string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failIf = append(w.failIf, f)
}

// Real makes rt perform the effects of the ops it serves: its own Apply and
// Check run, against whatever the test pointed rt at (a stub binary on PATH,
// a temporary directory), so an adapter's exactly-once logic is exercised
// under the engine instead of being replaced by the fake world. The world's
// fault targets still apply around it:
//   - CrashAfterIntent and the fuzz effect-uncertain fault run rt's Apply
//     first and then kill the engine: the effect landed and its result was
//     lost, so rt's Check must find it on recovery;
//   - FailApply and the other fuzz effect faults fail the call without
//     reaching rt;
//   - EffectDone and EffectUnknown script the check instead of rt.
//
// Observations of rt's ops stay scripted (Set, Unavailable). Making an op
// real twice is a test bug and panics.
func Real(w *World, rt wf.AdapterRuntime) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.real == nil {
		w.real = map[string]wf.AdapterRuntime{}
	}
	for _, op := range rt.Ops() {
		if _, dup := w.real[op]; dup || op == "*" {
			panic("wftest.Real: op " + op + " is already real or is not adapter.op")
		}
		w.real[op] = rt
	}
}

// landBeforeCrash runs a Real runtime's Apply before an injected crash; its
// outcome is lost with the engine, as the crash models.
func (w *World) landBeforeCrash(rt wf.AdapterRuntime, ws string, at wf.EffectContext, e wf.ExternalEffect) {
	if rt == nil {
		return
	}
	w.applied = append(w.applied, e)
	_, _ = realApply(rt, ws, at, e)
}

func realApply(rt wf.AdapterRuntime, ws string, at wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, error) {
	if c, ok := rt.(wf.ContextualEffectRuntime); ok {
		return c.ApplyAt(ws, at, e)
	}
	return rt.Apply(ws, e)
}

func realCheck(rt wf.AdapterRuntime, ws string, at wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, json.RawMessage, error) {
	switch c := rt.(type) {
	case wf.CheckResultRuntime:
		return c.CheckResultAt(ws, at, e)
	case wf.ContextualCheckRuntime:
		st, err := c.CheckAt(ws, at, e)
		return st, nil, err
	}
	st, err := rt.Check(ws, e)
	return st, nil, err
}

// Applied lists the effects the world has applied, in order. An effect that
// crashed the engine before its result is not in it, unless a Real runtime
// performed it before the crash.
func Applied(w *World) []wf.ExternalEffect {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.applied)
}

// EffectDone makes the effect (adapter, op) with key k report already_done
// when the engine checks for prior completion.
func EffectDone(w *World, adapter, op string, k wf.Key) {
	w.setEffect(adapter, op, k, wf.EffectDone)
}

// EffectUnknown makes the effect (adapter, op) with key k have an uncertain
// outcome when the engine checks it after a crash.
func EffectUnknown(w *World, adapter, op string, k wf.Key) {
	w.setEffect(adapter, op, k, wf.EffectUnknown)
}

func (w *World) setEffect(adapter, op string, k wf.Key, st wf.EffectStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.effects[effectKey(adapter, op, wf.ExternalEffect{Key: k}.KeyID())] = st
}

func effectKey(adapter, op, keyID string) string { return adapter + "." + op + "|" + keyID }

// arm makes the next Apply of (adapter, op) panic with crashSignal.
func (w *World) arm(adapter, op string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.crash[adapter+"."+op] = true
}

// Ops implements wf.AdapterRuntime: the world serves every op.
func (w *World) Ops() []string { return []string{"*"} }

// Read implements wf.AdapterRuntime.
func (w *World) Read(_ string, d wf.ObservationDesc) (json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.read(d)
}

// ReadAt implements observation faults at the owning state. A dry validation
// retains its selected fault until Done consumes it; validation cannot recover
// the world before the fault has reached the journal.
func (w *World) ReadAt(_ string, at wf.EffectContext, d wf.ObservationDesc) (json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := string(at.Run) + "\x00" + at.Instance + "\x00" + at.State + "\x00" + at.Purpose + "\x00" + readKey(d)
	var class string
	snapshots := w.observationSnapshot[key]
	retained := !w.observationProbe && len(snapshots) > 0
	if retained {
		snapshot := snapshots[0]
		class, w.fuzzPrelude = snapshot.class, snapshot.prelude
		if len(snapshots) == 1 {
			delete(w.observationSnapshot, key)
		} else {
			w.observationSnapshot[key] = snapshots[1:]
		}
	} else if !w.fuzzGood && w.fuzzObservation != nil {
		class = w.fuzzObservation(at, d)
	}
	if w.observationProbe {
		// Each occurrence makes its own fault selection. Retain healthy choices
		// too, so Done never shifts a later fault into an earlier read.
		w.observationSnapshot[key] = append(w.observationSnapshot[key], observationFaultSnapshot{class: class, prelude: w.fuzzPrelude})
	}
	if class != "" {
		if !w.observationProbe {
			if !w.fuzzPrelude && w.fuzzRecover != nil && w.fuzzRecover(at, class) {
				w.fuzzGood = true
			}
		}
		if !w.observationProbe && w.fuzzInjected != nil && (class == "observation-unavailable" || class == "observation-error") {
			w.fuzzInjected()
		}
		switch class {
		case "observation-unavailable":
			return nil, fmt.Errorf("%w: fuzz %s.%s unavailable", wf.ErrUnavailable, d.Adapter, d.Op)
		case "observation-error":
			return nil, fmt.Errorf("fuzz %s.%s observation failed", d.Adapter, d.Op)
		}
	}
	return w.read(d)
}

type observationFaultSnapshot struct {
	class   string
	prelude bool
}

// observationValidation brackets a validate/done pair without changing normal
// World reads. End-of-action cleanup also covers a validation that panics.
func (w *World) observationValidation(probe bool, reset bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.observationProbe = probe
	if reset {
		w.observationSnapshot = map[string][]observationFaultSnapshot{}
	}
}
func (w *World) observationFaultPending() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, snapshots := range w.observationSnapshot {
		for _, snapshot := range snapshots {
			if snapshot.class != "" {
				return true
			}
		}
	}
	return false
}

func (w *World) read(d wf.ObservationDesc) (json.RawMessage, error) {
	key := readKey(d)
	if w.down[key] {
		return nil, fmt.Errorf("%w: %s.%s is down in the world", wf.ErrUnavailable, d.Adapter, d.Op)
	}
	if n, ok := w.stale[key]; ok {
		w.stale[key] = n + 1
		return json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("stale-%d", n+1))), nil
	}
	if w.onRead != nil {
		if v, ok := w.onRead(d); ok {
			return v, nil
		}
	}
	if v, ok := w.reads[key]; ok {
		return v, nil
	}
	if msg, ok := w.unknown[d.Adapter+"."+d.Op]; ok {
		return nil, fmt.Errorf("%s: %s", msg, key)
	}
	w.missing = append(w.missing, key)
	return nil, fmt.Errorf("wftest: the world has no value for %s", key)
}

// Apply implements wf.AdapterRuntime. An armed crash panics before the effect
// is recorded; a command (shell.run) answers its scripted exit code.
func (w *World) Apply(workspace string, e wf.ExternalEffect) (json.RawMessage, error) {
	return w.ApplyAt(workspace, wf.EffectContext{}, e)
}

// ApplyAt implements wf.ContextualEffectRuntime so fuzz switches follow the
// effect's owning state, including when an older intent is resumed.
func (w *World) ApplyAt(ws string, at wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rt := w.real[e.Adapter+"."+e.Op]
	if k := e.Adapter + "." + e.Op; w.crash[k] {
		delete(w.crash, k)
		w.landBeforeCrash(rt, ws, at, e)
		panic(crashSignal{e.Adapter, e.Op})
	}
	fault := w.nextFuzzFault(at, e)
	if fault == "effect-uncertain" {
		w.fuzzUncertain = &fuzzUncertain{at: at, key: effectKey(e.Adapter, e.Op, e.KeyID()), prelude: w.fuzzPrelude}
		w.landBeforeCrash(rt, ws, at, e)
		panic(crashSignal{e.Adapter, e.Op})
	}
	var injected json.RawMessage
	if fault != "" {
		var err error
		injected, err = effectFaultResult(fault, e)
		if w.fuzzInjected != nil {
			w.fuzzInjected()
		}
		if err != nil {
			return nil, err
		}
	}
	w.applied = append(w.applied, e)
	if w.onApply != nil {
		w.onApply(e)
	}
	if injected != nil {
		return w.commandResult(e, injected), nil
	}
	if rt != nil {
		if msg, ok := w.failing[e.Adapter+"."+e.Op]; ok {
			return nil, w.failedApply(msg)
		}
		return realApply(rt, ws, at, e)
	}
	if raw, ok := w.nextFuzzResult(at, e); ok {
		return w.commandResult(e, raw), nil
	}
	if w.fuzzGood {
		if e.Adapter == "shell" && e.Op == "run" {
			return json.Marshal(wf.RunResult{Exit: 0, Stdout: w.stdoutOf(argvOf(e.Args))})
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	if msg, ok := w.failing[e.Adapter+"."+e.Op]; ok {
		return nil, w.failedApply(msg)
	}
	for _, f := range w.failIf {
		if msg := f(e); msg != "" {
			return nil, w.failedApply(msg)
		}
	}
	if e.Adapter == "shell" && e.Op == "run" {
		argv := argvOf(e.Args)
		raw, err := json.Marshal(wf.RunResult{Exit: w.exitOf(argv), Stdout: w.stdoutOf(argv)})
		if err != nil {
			return nil, err
		}
		return w.commandResult(e, raw), nil
	}
	return json.RawMessage(`{"ok":true}`), nil
}

// commandResult marks the shell.run just applied as failed when its result
// exits non-zero, so Check answers not done for it and the engine runs the
// command again, as the real shell adapter's check does. raw is returned as
// is.
func (w *World) commandResult(e wf.ExternalEffect, raw json.RawMessage) json.RawMessage {
	if e.Adapter != "shell" || e.Op != "run" {
		return raw
	}
	var r wf.RunResult
	if json.Unmarshal(raw, &r) == nil && r.Exit != 0 {
		_ = w.failedApply("")
	}
	return raw
}

// setFuzzGood serializes phase changes with effect application.
func (w *World) setFuzzGood(good bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fuzzGood = good
}

func argvOf(args map[string]any) []string {
	switch v := args["argv"].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, len(v))
		for i, x := range v {
			out[i] = fmt.Sprint(x)
		}
		return out
	}
	return nil
}

func (w *World) exitOf(argv []string) int {
	for i := len(w.exits) - 1; i >= 0; i-- {
		if r := w.exits[i]; len(argv) >= len(r.prefix) && slices.Equal(argv[:len(r.prefix)], r.prefix) {
			if r.once {
				w.exits = slices.Delete(w.exits, i, i+1)
			}
			return r.code
		}
	}
	return 0
}

func (w *World) stdoutOf(argv []string) string {
	for i := len(w.outs) - 1; i >= 0; i-- {
		if r := w.outs[i]; len(argv) >= len(r.prefix) && slices.Equal(argv[:len(r.prefix)], r.prefix) {
			return r.text
		}
	}
	return ""
}

// CheckAt is Check with the run's context, which a Real runtime receives;
// the fake world itself reaches nothing, so a sandboxed run's check is
// answered as any other.
func (w *World) CheckAt(ws string, at wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, error) {
	st, _, err := w.CheckResultAt(ws, at, e)
	return st, err
}

// Check implements wf.AdapterRuntime: the scripted answer for the effect's
// key (EffectDone, EffectUnknown), else a Real runtime's own check, else done
// when the world already applied an effect with that op and key and no
// script failed that apply (a command that exited non-zero counts as failed),
// else not done.
func (w *World) Check(ws string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	return w.CheckAt(ws, wf.EffectContext{}, e)
}

// CheckResultAt implements wf.CheckResultRuntime, so a Real runtime's result
// for a done effect reaches the journal as it would in production.
func (w *World) CheckResultAt(ws string, at wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id := e.KeyID()
	rt := w.real[e.Adapter+"."+e.Op]
	if pending := w.fuzzUncertain; pending != nil && pending.key == effectKey(e.Adapter, e.Op, id) {
		w.finishUncertain()
		if rt == nil {
			return wf.EffectUnknown, nil, nil
		}
		// A Real runtime's effect did land before the crash: its own check
		// must find it.
	}
	if st, ok := w.effects[effectKey(e.Adapter, e.Op, id)]; ok {
		return st, nil, nil
	}
	if rt != nil {
		return realCheck(rt, ws, at, e)
	}
	for i, a := range w.applied {
		if a.Adapter == e.Adapter && a.Op == e.Op && a.KeyID() == id && !w.failed[i] {
			return wf.EffectDone, nil, nil
		}
	}
	return wf.EffectNotDone, nil, nil
}

// Trace is what a run did: the states visited, the journal and the terminal
// reached. It is filled by Run from the journal.
type Trace struct {
	runDir      string
	route       []string
	journal     []wf.JournalRecord
	terminal    string
	sessions    []string
	crashed     []wf.JournalKind
	validations []Validation
}

// Run plays steps against the definition on the real engine and returns the
// Trace. It starts the run, then for each step takes the engine's open
// instructions: Submit and SubmitAt need an agent instruction whose state
// matches, Answer and AnswerAt an open human gate, Settle the open
// manual-verification instruction. A step that does not match what the engine
// issued fails with both shown. Run ends when the steps do, so a human gate
// with no step left is where a test stops.
func (s *Sim) Run(steps ...Step) *Trace {
	cj := NewCrashJournal(wf.MemJournal())
	r := &runner{s: s, steps: foldCrashes(steps), j: cj, cj: cj}
	if s.tb != nil {
		r.dir = s.tb.TempDir()
	} else {
		var err error
		r.dir, err = os.MkdirTemp("", "wftest-*")
		if err != nil {
			panic("wftest: " + err.Error())
		}
		defer os.RemoveAll(r.dir)
	}
	r.reg = wf.NewAdapters()
	if err := r.reg.Register(s.world); err != nil {
		panic("wftest: " + err.Error())
	}
	if s.cap > 0 {
		r.pc = newCoordinator(r, s.cap)
	} else if s.paged {
		r.fail("PagedPrompts needs Cap(n): the paged coordinator fetches the pages")
		return r.trace()
	}
	for i, st := range steps {
		if st.fetchSet && !s.paged {
			r.fail("script step %d (%s): Fetching needs PagedPrompts()", i+1, st.desc)
			return r.trace()
		}
	}
	r.e = r.engine()
	if len(r.steps) > 0 && r.steps[0].kind == stepCrashAt && r.steps[0].jk == wf.JournalRunStarted {
		// The engine dies inside Run, before it names the run: the id is the
		// journal's only run.
		cj.Arm(wf.JournalRunStarted)
		r.steps = r.steps[1:]
		r.guard(func() { r.e.Run(s.def, s.params, s.options) })
		if runs, err := cj.Runs(); err != nil || len(runs) != 1 {
			r.fail("CrashAt(run_started): the run did not start (runs %v, error %v)", runs, err)
			return r.trace()
		}
		r.id = mustRuns(cj)
		r.play()
		return r.trace()
	}
	id, err := r.e.Run(s.def, s.params, s.options)
	if err != nil {
		r.fail("starting the run: %v", err)
		return r.trace()
	}
	r.id = id
	r.play()
	return r.trace()
}

func mustRuns(j wf.Journal) wf.RunID {
	runs, _ := j.Runs()
	return runs[0]
}

// foldCrashes attaches each CrashAfterIntent that directly follows an action
// step to that step, so the crash is armed before the cycle the action runs.
func foldCrashes(steps []Step) []Step {
	var out []Step
	for _, st := range steps {
		if (st.kind == stepCrash || st.kind == stepCrashAt) && len(out) > 0 {
			if last := &out[len(out)-1]; last.kind == stepSubmit || last.kind == stepFail || last.kind == stepAnswer || last.kind == stepSettle || last.kind == stepRecover {
				last.crashes = append(last.crashes, st)
				continue
			}
		}
		out = append(out, st)
	}
	return out
}

type runner struct {
	s     *Sim
	steps []Step
	j     wf.Journal
	cj    *CrashJournal // j, to arm a CrashAt
	reg   *wf.Adapters
	e     wf.Executor
	id    wf.RunID
	dead  bool         // the engine crashed and awaits Resume
	stop  bool         // a failure was reported
	dir   string       // the run's result directory, with an effect runner
	pc    *coordinator // the paged coordinator, with Cap
	// validations are the ValidateAt verdicts, in script order.
	validations []Validation
}

func (r *runner) engine() wf.Executor {
	opts := wf.EngineOptions{Assets: r.s.assets, Defs: []*wf.Def{r.s.def}, RunDir: func(wf.RunID) string { return r.dir }}
	if r.s.workspace != "" {
		opts.Workspace = r.s.workspace
	}
	if r.s.runner != nil {
		opts.EffectRunner = r.s.runner
	}
	opts.Clock = r.s.clock
	opts.PagedPrompts = r.s.paged
	if r.pc != nil {
		opts.Clock, opts.TranscriptMtime = r.pc.now, r.pc.mtime
	}
	return wf.NewEngine(r.j, r.reg, opts)
}

// arm arms the crash a CrashAfterIntent or CrashAt step describes.
func (r *runner) arm(st Step) {
	if st.kind == stepCrashAt {
		r.cj.Arm(st.jk)
		return
	}
	r.s.world.arm(st.adapter, st.op)
}

// fail reports a failure through the Sim's TB, or panics when it has none, and
// ends the run.
func (r *runner) fail(format string, args ...any) {
	r.stop = true
	msg := "wftest: " + fmt.Sprintf(format, args...) + "\ntrace:\n" + r.trace().String()
	if tb := r.s.tb; tb != nil {
		tb.Helper()
		tb.Errorf("%s", msg)
		return
	}
	panic(msg)
}

// guard runs f and reports whether it was cut short by the world's armed
// crash. Any other panic is re-raised.
func (r *runner) guard(f func()) (crashed bool) {
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(crashSignal); !ok {
				panic(v)
			}
			crashed, r.dead = true, true
		}
	}()
	f()
	return false
}

func (r *runner) next() (ins []wf.Instruction, ok bool) {
	var err error
	if r.guard(func() { ins, err = r.e.Next(r.id, "") }) {
		return nil, false
	}
	if err != nil {
		r.fail("next: %v", err)
		return nil, false
	}
	// An ended run hands out one ended instruction, not work: the script
	// ends there as it did when an ended run handed out nothing.
	ins = slices.DeleteFunc(slices.Clone(ins), func(in wf.Instruction) bool { return in.Kind == wf.InstructionEnded })
	return ins, true
}

func (r *runner) play() {
	if r.pc != nil {
		r.pc.play()
		return
	}
	for i := 0; i < len(r.steps) && !r.stop; i++ {
		st := r.steps[i]
		switch {
		case st.kind == stepResume:
			r.e, r.dead = r.engine(), false
			r.next() // re-fold the journal and drive what the crash left
		case r.dead:
			r.fail("script step %d (%s): the engine crashed and the script has no Resume() before it", i+1, st.desc)
		case st.kind == stepCrash || st.kind == stepCrashAt:
			r.arm(st)
			r.next()
		case st.kind >= stepKill:
			r.fail("script step %d (%s) drives a paged coordinator and needs Cap(n)", i+1, st.desc)
		default:
			for n := 0; n < st.times && !r.stop && !r.dead; n++ {
				r.act(i, st)
			}
		}
	}
	// The script is spent: the coordinator still answers what is offered.
	for !r.stop && !r.dead {
		var st wf.RunStatus
		var err error
		if r.guard(func() { st, err = r.e.Status(r.id) }) || err != nil || len(st.Recoveries) == 0 {
			break
		}
		ins, ok := r.next()
		if !ok || !r.policyRecover(nil, ins) {
			break
		}
	}
}

// policyRecover answers, with the Sim's recovery policy, every open recovery
// offer that st does not address, and reports whether it answered one.
func (r *runner) policyRecover(st *Step, ins []wf.Instruction) bool {
	choice := wf.RecoveryGiveUp
	if r.s.recoverSet {
		choice = r.s.recover
	}
	if choice == "" {
		return false
	}
	answered := false
	for _, in := range ins {
		if in.Kind != wf.InstructionRecovery || (st != nil && st.kind == stepRecover && pathMatches(st.path, in.State)) {
			continue
		}
		var err error
		if r.guard(func() { err = r.e.Recover(r.id, in.Name, choice) }) {
			return true
		}
		if err != nil {
			r.fail("recovery policy %s for %s: %v", choice, in.Name, err)
			return false
		}
		answered = true
	}
	return answered
}

// act performs one repetition of an action step, arming its folded crashes only once its instruction is issued.
func (r *runner) act(i int, st Step) {
	ins, ok := r.next()
	if !ok {
		return
	}
	for r.policyRecover(&st, ins) && !r.dead {
		if ins, ok = r.next(); !ok {
			return
		}
	}
	in, found := pick(st, ins)
	if !found {
		r.fail("script step %d does not match the issued instruction\n  expected: %s\n  issued:   %s", i+1, st.desc, describe(ins))
		return
	}
	for _, c := range st.crashes {
		r.arm(c)
	}
	var err error
	r.guard(func() {
		switch st.kind {
		case stepFail:
			_, err = r.e.FailedAs(r.id, in.Name, st.reason, st.failure)
		case stepSubmit, stepValidate:
			if err = r.writeArtifacts(st, in); err != nil {
				return
			}
			var out []byte
			if out, err = json.Marshal(st.value); err != nil {
				return
			}
			if st.kind == stepValidate {
				var res wf.SubmitResult
				if res, err = r.e.Validate(r.id, in.Name, wf.Submission{Output: out}); err == nil {
					r.validations = append(r.validations, Validation{State: in.State, Result: res})
				}
				return
			}
			_, err = r.e.Submit(r.id, wf.Submission{Token: in.Token, Child: in.Child, Output: out})
		case stepAnswer:
			err = r.e.Answer(r.id, in.State, st.value.(string))
		case stepRecover:
			err = r.e.Recover(r.id, in.Name, st.value.(string))
		case stepSettle:
			if st.result != nil {
				err = simSettle(r.e, r.id, in.Effect, true, st.result)
			} else {
				err = simSettle(r.e, r.id, in.Effect, st.applied, nil)
			}
		}
	})
	if err != nil {
		r.fail("script step %d (%s): %v", i+1, st.desc, err)
	}
}

// pick finds the open instruction the step addresses.
func pick(st Step, ins []wf.Instruction) (wf.Instruction, bool) {
	for _, in := range ins {
		switch {
		case (st.kind == stepSubmit || st.kind == stepValidate || st.kind == stepFail) && in.Kind == wf.InstructionAgent && pathMatches(st.path, in.State):
			return in, true
		case st.kind == stepAnswer && in.Kind == wf.InstructionHumanGate && pathMatches(st.path, in.State):
			return in, true
		case st.kind == stepSettle && in.Kind == wf.InstructionManualVerification:
			return in, true
		case st.kind == stepRecover && in.Kind == wf.InstructionRecovery && pathMatches(st.path, in.State):
			return in, true
		}
	}
	return wf.Instruction{}, false
}

// pathMatches compares a script path with a qualified state by suffix on whole
// path elements, so "Merge" matches "task/Merge" but "erge" does not.
func pathMatches(path, state string) bool {
	return state == path || strings.HasSuffix(state, "/"+path)
}

func describe(ins []wf.Instruction) string {
	if len(ins) == 0 {
		return "nothing (no open instruction)"
	}
	parts := make([]string, len(ins))
	for i, in := range ins {
		parts[i] = fmt.Sprintf("%s %q (attempt %d)", in.Kind, in.State, in.Attempt)
	}
	return strings.Join(parts, "; ")
}

// trace folds the journal into a Trace.
func (r *runner) trace() *Trace {
	t := &Trace{runDir: r.dir}
	if r.id == "" {
		return t
	}
	unlock, err := r.j.Lock(r.id)
	if err != nil {
		panic("wftest: " + err.Error())
	}
	defer unlock()
	recs, err := r.j.Records(r.id)
	if err != nil {
		panic("wftest: " + err.Error())
	}
	t.journal = recs
	if r.pc != nil {
		t.sessions = slices.Clone(r.pc.sessions)
	}
	t.crashed = r.cj.Fired()
	t.validations = slices.Clone(r.validations)
	t.route = routeOf(wf.Inspect(r.s.def), recs)
	for _, rec := range recs {
		if rec.Kind == wf.JournalRunEnded && rec.Instance == "" {
			var d wf.RunEndedData
			if json.Unmarshal(rec.Data, &d) == nil {
				t.terminal = d.Terminal
			}
		}
	}
	return t
}

// routeOf lists the qualified state of every arrival: each instance's start
// state, then the target of each of its transitions.
func routeOf(root wf.DefView, recs []wf.JournalRecord) []string {
	var route []string
	if len(root.States) > 0 {
		route = append(route, root.States[0].Name)
	}
	prefix := map[string][]string{"": nil}
	qualify := func(p []string, state string) string { return strings.Join(append(slices.Clone(p), state), "/") }
	for _, r := range recs {
		switch r.Kind {
		case wf.JournalChildSpawned:
			var d wf.ChildSpawnedData
			if json.Unmarshal(r.Data, &d) != nil {
				continue
			}
			p := append(slices.Clone(prefix[r.Instance]), d.Def)
			prefix[d.ID] = p
			if cv, ok := findChild(root, d.Def); ok && len(cv.States) > 0 {
				route = append(route, qualify(p, cv.States[0].Name))
			}
		case wf.JournalTransition:
			var d wf.TransitionData
			if json.Unmarshal(r.Data, &d) == nil {
				route = append(route, qualify(prefix[r.Instance], d.To))
			}
		}
	}
	return route
}

func findChild(v wf.DefView, name string) (wf.DefView, bool) {
	for _, c := range v.Children {
		if c.Name == name {
			return c, true
		}
		if f, ok := findChild(c, name); ok {
			return f, true
		}
	}
	return wf.DefView{}, false
}

// Route returns the qualified names of the states visited, in order.
func (t *Trace) Route() []string { return t.route }

// Journal returns the run's journal records in sequence order.
func (t *Trace) Journal() []wf.JournalRecord { return t.journal }

// Validations returns the ValidateAt verdicts, in script order.
func (t *Trace) Validations() []Validation { return t.validations }

// Terminal returns the name of the terminal state reached, "" if the run did
// not end.
func (t *Trace) Terminal() string { return t.terminal }

// Lost lists the units the engine lost, in order, each as "<unit>: <cause>"
// (the wf.LostCause* text). A loss spends no retry.
func (t *Trace) Lost() []string {
	var out []string
	for _, r := range t.journal {
		if r.Kind != wf.JournalAttemptLost {
			continue
		}
		var d wf.AttemptLostData
		if json.Unmarshal(r.Data, &d) == nil {
			out = append(out, d.Name+": "+d.Cause)
		}
	}
	return out
}

// RetriesSpent is the number of attempts that counted against a retry budget:
// the refusals the journal marks Spent. (wf.RunStatus does not carry the
// per-instance count, so it is read from the same journal the fold reads.)
func (t *Trace) RetriesSpent() int {
	n := 0
	for _, r := range t.journal {
		if r.Kind != wf.JournalRefused {
			continue
		}
		var d wf.RefusedData
		if json.Unmarshal(r.Data, &d) == nil && d.Spent {
			n++
		}
	}
	return n
}

// Return is one send-back the run took (wf SendBack): the instance, the
// sending state and edge, the producer it returned to, and the journaled
// round, budget and reasons.
type Return struct {
	Instance, From, Label, To string
	Round, Of                 int
	Reasons                   []string
	Auto                      bool // planned for a wf.Reject with no declared edge
}

// Returns lists the send-backs the run took, in journal order, read from
// the plans the engine journaled.
func (t *Trace) Returns() []Return {
	var out []Return
	for _, r := range t.journal {
		var d struct{ Plan *wf.EdgePlan }
		if json.Unmarshal(r.Data, &d) != nil || d.Plan == nil {
			continue
		}
		for _, s := range d.Plan.Steps {
			if s.SendBack != nil {
				out = append(out, Return{Instance: r.Instance, From: s.From, Label: s.Label, To: s.To,
					Round: s.SendBack.Round, Of: s.SendBack.Of, Reasons: s.SendBack.Reasons, Auto: s.Auto})
			}
		}
	}
	return out
}

// Crashes lists the record kinds whose append crashed the engine (CrashAt), in
// order. A CrashAt that never fires is a scenario that did not reach its point.
func (t *Trace) Crashes() []wf.JournalKind { return t.crashed }

// Sessions lists the coordinator session ids the run used, in order; empty
// without Cap.
func (t *Trace) Sessions() []string { return t.sessions }

// AssertRoute fails tb unless the states visited, qualified for children, are
// exactly states.
func AssertRoute(tb testing.TB, t *Trace, states ...string) {
	tb.Helper()
	if !reflect.DeepEqual(t.route, states) {
		tb.Errorf("route = %v, want %v", t.route, states)
	}
}

// AssertJournal fails tb unless the journal's record kinds are exactly kinds,
// in order.
func AssertJournal(tb testing.TB, t *Trace, kinds ...wf.JournalKind) {
	tb.Helper()
	got := make([]wf.JournalKind, len(t.journal))
	for i, r := range t.journal {
		got[i] = r.Kind
	}
	if !reflect.DeepEqual(got, kinds) {
		tb.Errorf("journal kinds = %v, want %v", got, kinds)
	}
}

// AssertTerminal fails tb unless the run ended in term.
func AssertTerminal[S any](tb testing.TB, t *Trace, term *wf.Terminal[S]) {
	tb.Helper()
	if want := markerName[S](); t.terminal != want {
		tb.Errorf("terminal = %q, want %q", t.terminal, want)
	}
}

// AssertClass fails tb unless some journal record for state carries outcome
// class c.
func AssertClass(tb testing.TB, t *Trace, state string, c wf.OutcomeClass) {
	tb.Helper()
	for _, r := range t.journal {
		if r.State == state && r.Class == c {
			return
		}
	}
	tb.Errorf("no journal record for state %q with class %v", state, c)
}

// simSettle settles a simulated run's open effect the way its scenario says,
// as an operator with evidence: the engine settles nothing anonymously
// , and an effect outside the run is an operator's.
func simSettle(e wf.Executor, run wf.RunID, effect int64, applied bool, rr *wf.RunResult) error {
	op := wf.Operator{Name: "wftest", Reason: "the scenario's settlement", Harness: "generic"}
	return e.(*wf.Engine).SettleAs(run, effect, applied, rr, "simulated: the scenario settles it", op)
}
