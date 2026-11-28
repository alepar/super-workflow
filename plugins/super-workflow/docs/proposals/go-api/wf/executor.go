package wf

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// RunID identifies one run of a definition.
type RunID string

// Executor is the Run/Next/Submit boundary.
//
// Owns: the shapes of starting a run, asking for the ready instructions,
// submitting a result, abandoning an attempt and reading status. Its
// implementation is Engine (NewEngine).
// Does not: leak into definitions: definitions never depend on executor
// internals.
//
// Declared obligations: A route closure (Route or OnRoute) that returns an
// edge outside its declared candidates is not followed: the step is classed
// Blocked and journaled as a JournalRouteOffCandidates record naming the state
// and the returned edge. The walk never runs the closure, so only the executor
// can check it.
//
// A PromptFor selector that returns a reference outside the state's candidates
// is not used: the step is classed Blocked and journaled as a
// JournalPromptOffCandidates record. A candidate whose file or pointer is
// missing at issue time classes the attempt Unavailable.
//
// Scoped counters (Def.Counter with ScopedTo) are zeroed when their scope
// counter is incremented, journaled as a JournalScopeReset record; on one edge
// the scope Inc and the zeroing apply before any scoped Inc. A Map child
// instance the executor stops on the run budget is reported through
// Instance.BudgetStopped in the MapResult, never as Refused, and journaled as a
// JournalBudgetStopped record. Each Sub entry and each Map item starts a fresh
// child instance: its counters, MaxVisits arrivals and OutputOf values start
// empty.
//
// Extend by: a new implementation of the interface; the shapes change only
// with a new definition version.
type Executor interface {
	// Run starts (or resumes, by run key or supplied id) a run of d. options is
	// the value of the definition's typed options struct.
	Run(d *Def, params map[string]string, options any) (RunID, error)
	// Next returns every ready instruction of the run, or of one child
	// instance. child "" means every instance. It is idempotent: a repeat call
	// shows the same tokens.
	Next(run RunID, child string) ([]Instruction, error)
	// HandOut hands out up to n ready agent units (n < 0: every ready unit),
	// in handOutOrder, journaling handed_out for each, and returns them with
	// every due instruction that needs no subagent (human gates, waits, manual
	// verifications, backoff waits). A handed unit is not handed again until
	// its hand-out window expires unclaimed. The engine sets no cap of its own:
	// the coordinator picks n from its harness's limit and its running work.
	HandOut(run RunID, n int) ([]Instruction, error)
	// Submit hands back the result of an instruction and returns how it was
	// classified and where the run goes next.
	Submit(run RunID, s Submission) (SubmitResult, error)
	// Fail abandons an attempt; it spends the retry budget as Refused.
	Fail(run RunID, token, reason string) (SubmitResult, error)
	// Started claims a handed unit or attaches compatible ownership metadata
	// (journals attempt_started). Conflicting metadata is ErrRejected;
	// a repeat with no additions journals nothing and never restarts the clocks.
	Started(run RunID, name string, c Claim) error
	// StartedNote is Started that also reports the state of a unit that was
	// already claimed or ended when the call arrived ("" for a first claim).
	// Compatible metadata attaches as metadata_attached with late: true.
	StartedNote(run RunID, name string, c Claim) (state string, err error)
	// Prompt claims a handed unit if necessary, then renders its prompt and
	// journals prompt_fetched; a retired, pending or unknown name is ErrRejected.
	Prompt(run RunID, name string) (string, error)
	// PromptClaim is Prompt for a child that carries the claim token of the
	// hand-out it was spawned from. The first tokened fetch binds its token to
	// the unit; the same token re-renders and any other is ErrRejected
	// ("another child already owns this unit"). An empty claim is Prompt.
	PromptClaim(run RunID, name, claim string) (string, error)
	// Done submits the unit's output: the name is resolved to its token and the
	// Submit path classifies it. A retired or unknown name is ErrRejected.
	Done(run RunID, name string, s Submission) (SubmitResult, error)
	// Validate classifies s as Done would, through the same token check,
	// schema, artifacts, check observations and route, and records nothing:
	// no journal record, no edge effect, no attempt spent. Accepted means
	// done would accept it now.
	Validate(run RunID, name string, s Submission) (SubmitResult, error)
	// Failed abandons the unit's attempt, as Fail does: it spends a retry.
	Failed(run RunID, name, reason string) (SubmitResult, error)
	// FailedAs is Failed with a FailureClass. Blocked routes the failure.
	// Approval holds with reason as the exact action. Neither spends a retry.
	FailedAs(run RunID, name, reason string, class FailureClass) (SubmitResult, error)
	// Answer answers an open human gate by its qualified state; a value outside
	// the gate's Enum is an error and journals nothing.
	Answer(run RunID, gate, value string) error
	// AnswerVisit answers a gate bound to the instance and the arrival the
	// instruction named: a stale visit or a gate open on another child is
	// ErrRejected and journals nothing.
	AnswerVisit(run RunID, a GateAnswer) error
	// Settle settles an Uncertain effect (the intent Seq a manual-verification
	// instruction carries) as applied or not. Applied continues the run; not
	// applied classes the step Blocked. It names no settler and no evidence,
	// so the Engine refuses it: a settlement goes through
	// Engine.SettleAs.
	Settle(run RunID, effect int64, applied bool) error
	// SettleRun settles an Uncertain command intent as applied with the exit
	// code and stdout a human observed; its routing reads them. Refused by
	// the Engine as Settle is.
	SettleRun(run RunID, effect int64, r RunResult) error
	// Retry returns a halted instance (child "" is the run itself) to ready at
	// the next attempt and drives it. An instance that is not halted, or a
	// child whose spawner already decided (the Map's join, the Sub's exit), is
	// ErrRejected; a halted child its spawner settled with the join still open
	// is retried. An instance whose unit a blocked failure holds is released
	// the same way: the held attempt ends failed and the next attempt is
	// queued, with no retry spent. An ended run is ErrRejected before anything
	// else. stop answers an exhaustion halt with its fallback edge instead of
	// one more attempt; stop on any other halt or hold is ErrRejected.
	Retry(run RunID, child string, stop bool) error
	// ReleaseApproval releases the hold identified by HeldUnit.ApprovalHold
	// for the given instance (empty child means the root). A missing or
	// stale sequence is ErrRejected. The release spends no retry.
	ReleaseApproval(run RunID, child string, holdSeq int64) error
	// Recover answers the recovery offer of a unit (B2, F1) with
	// RecoveryResume, RecoveryRestart or RecoveryGiveUp; ErrRejected when the
	// unit has no open offer or the choice does not apply to it.
	Recover(run RunID, unit, choice string) error
	// Status reports where the run and its instances are.
	Status(run RunID) (RunStatus, error)
	// Wait blocks until something needs the coordinator: a record after since
	// that maps to an Event, a deadline coming due, the run ending, or timeout
	// (zero: 540s). since < 0 means the Seq of the run's last handed_out or
	// attempt_started. It drives the run as every other call does and never
	// polls the harness.
	Wait(run RunID, since int64, timeout time.Duration) (WaitResult, error)
	// Heartbeat answers a stall: alive clears it (heartbeat active, the
	// threshold restarts); dead marks the attempt lost (no retry spent). With
	// Session set, no Name and dead, every claimed unit of that session is
	// lost. A unit that is not started, running or stalled, and alive with a
	// Session, are ErrRejected.
	Heartbeat(run RunID, t HeartbeatTarget, alive bool) error
}

// HeartbeatTarget names one unit, or every orphan of a coordinator session.
type HeartbeatTarget struct {
	Name, Session string
	// Note is, on an active answer, what the coordinator saw the child do
	// since the last question (new output, a running command): journaled with
	// the answer, it is evidence of work and starts the no-progress count over.
	Note string
}

// WaitResult is what Wait returns: the events (or one timeout event), the
// journal's last Seq (pass it as since next time) and the unit counts.
type WaitResult struct {
	Events []Event
	Seq    int64
	UnitCounts
	Binding Binding // what limits the run; BindingNone when nothing does
}

// UnitCounts are the subagent unit counts a coordinator can act on: ready to
// hand out (never offered, or a hand-out that expired unclaimed), handed out
// and not yet claimed, and claimed (started, running or stalled).
type UnitCounts struct {
	Ready, Handed, Running int
}

// Claim is who started a handed unit: the coordinator's harness and session,
// the subagent's agent id and its transcript path, and the context mode the
// coordinator reports it spawned the subagent with (empty: not reported).
type Claim struct {
	Harness, Session, Agent, Transcript string
	Context                             ContextMode
	// Root is where the harness writes transcripts: the engine searches it
	// for the child's transcript by claim token (package harness).
	Root string `json:",omitempty"`
	// Env marks a claim made by a prompt fetch that read the child's
	// environment (A1); a manual started never sets it.
	Env bool `json:",omitempty"`
}

// InstructionKind is the closed set of instruction kinds.
type InstructionKind string

// Instruction kinds.
const (
	InstructionAgent              InstructionKind = "agent"
	InstructionHumanGate          InstructionKind = "human-gate"
	InstructionWait               InstructionKind = "wait"
	InstructionManualVerification InstructionKind = "manual-verification"
	InstructionEnded              InstructionKind = "ended"    // the run has ended: Terminal, Outcome and Cause say how
	InstructionWaiting            InstructionKind = "waiting"  // every remaining unit is held for a human: Holds lists each cause and fix
	InstructionStopped            InstructionKind = "stopped"  // wf stop: the run starts nothing until wf resume; the loop ends for now
	InstructionRecovery           InstructionKind = "recovery" // a unit the coordinator recovers by its choice (B2, F1): Recovery has the evidence and the commands
)

// Instruction is one actionable step the executor hands to the agent.
type Instruction struct {
	IntValues       []GateIntValueView `json:"intValues,omitempty"`
	PromptHash      string             `json:",omitempty"`
	PromptShapeHash string             `json:",omitempty"`
	Run             RunID              `json:"run"`
	State           string             `json:"state"`            // child def names joined with "/", then the state; the root def is omitted
	Kind            InstructionKind    `json:"kind"`             // the closed set above
	Attempt         int                `json:"attempt"`          // 1-based attempt number
	Name            string             `json:"name,omitempty"`   // the unit name (paged agent units)
	Output          string             `json:"output,omitempty"` // allocated result JSON file for file-backed units
	Class           string             `json:"class,omitempty"`  // the unit's class: child definition names, then the state
	Brief           string             `json:"brief,omitempty"`  // the fixed spawn brief the coordinator passes verbatim
	Token           string             `json:"token,omitempty"`  // token to echo in the submission; a paged unit has none
	Prompt          string             `json:"prompt,omitempty"` // resolved prompt, Inputs block and Submission block; a paged unit fetches it
	Inputs          map[string]string  `json:"inputs,omitempty"`
	Submit          string             `json:"submit,omitempty"` // how to submit
	Child           string             `json:"child,omitempty"`  // child instance id, "" for the run itself
	Dispatch        DispatchHints      `json:"dispatch"`         // per-agent hints the coordinator spawns the subagent with
	// Spawn is the spawn verb's command line for a unit of another harness
	// than the coordinator's (Dispatch.Family): the coordinator runs it and
	// it starts the child as a headless process; "" for an ordinary unit.
	Spawn              string            `json:"spawn,omitempty"`
	GitAuthority       *GitAuthority     `json:"gitAuthority,omitempty"` // agent: the git authority the state declares; absent when it declares none
	Context            ContextMode       `json:"context,omitempty"`      // agent: always ContextFresh
	Workspace          string            `json:"workspace,omitempty"`    // the run's workspace directory
	Effect             int64             `json:"effect,omitempty"`       // manual verification: the effect id to settle
	Transient          bool              `json:"transient,omitempty"`    // manual verification: the effect's Check failed transiently and is checked again on the next call
	Person             bool              `json:"person,omitempty"`       // manual verification: the effect is outside the run (wf.Outside), so a person settles it; otherwise the coordinator does
	RetryAfterSeconds  int               `json:"retryAfterSeconds,omitempty"`
	Question           string            `json:"question,omitempty"`
	Answers            []string          `json:"answers,omitempty"`
	Visit              int               `json:"visit,omitempty"`              // a human gate: which arrival at the gate this is; echo it to answer --visit
	Delegated          bool              `json:"delegated,omitempty"`          // a human gate the run's starter delegated: DelegatedAnswer is the answer to give
	DelegatedAnswer    string            `json:"delegatedAnswer,omitempty"`    // the delegated answer, journaled in run_started
	CoordinatorAnswers bool              `json:"coordinatorAnswers,omitempty"` // a fanout fork's gate target: the coordinator chooses the answer itself
	Deadline           time.Time         `json:"deadline,omitzero"`            // a timed gate: when it takes its timeout edge; RetryAfterSeconds counts down to it
	Terminal           string            `json:"terminal,omitempty"`           // ended: the terminal state the run reached
	Outcome            Outcome           `json:"outcome,omitempty"`            // ended: the terminal's outcome
	Cause              string            `json:"cause,omitempty"`              // ended: why, from the Run cause walk (see runCause)
	Resume             string            `json:"resume,omitempty"`             // ended: the command that continues the work as a successor run
	PreviousRefusal    []string          `json:"previousRefusal,omitempty"`    // agent, attempt > 1: the reasons the state's last refused output was refused
	NotStarted         bool              `json:"notStarted,omitempty"`         // agent: offered to this coordinator before and never started: passed over, or unfetched for UnstartedGrace
	ErrLog             string            `json:"errLog,omitempty"`             // agent: the file this hand-out's child writes its stderr to (adapters with StartLog)
	SameClaim          bool              `json:"sameClaim,omitempty"`          // agent, NotStarted: this is the offer made before, under the same claim token (a re-list): a second child of it would work under that claim and is not told to stop, so start one only if the earlier start is no longer running
	PrevErrLog         string            `json:"prevErrLog,omitempty"`         // agent, NotStarted: the earlier hand-out's stderr file
	PrevErrTail        []string          `json:"prevErrTail,omitempty"`        // agent, NotStarted: the last lines of PrevErrLog, when it holds any
	HandoffReceipt     int64             `json:"handoffReceipt,omitempty"`     // person handoff acknowledgement
	Breakpoint         *BreakpointStatus `json:"breakpoint,omitempty"`         // stopped at a run breakpoint
	Counts             string            `json:"counts,omitempty"`             // stopped, ended: the run's subagent totals exactly as status prints them, for the coordinator's report to quote
	Holds              []HoldView        `json:"holds,omitempty"`              // waiting: each held unit with its cause and fix
	Recovery           *RecoveryView     `json:"recovery,omitempty"`           // InstructionRecovery: where the unit stopped and the choices
	Advice             string            `json:"advice,omitempty"`             // a run that stepped down to detach none mid-run: the bootstrap's detach-none line
}

// GateAnswer is an answer to a human gate bound to the instruction that showed
// it. Child is the instance ("" is the run itself) and Visit the instruction's
// Visit; Visit 0 skips the visit check.
type GateAnswer struct {
	IntValue    *int `json:",omitempty"` // nil uses the declared proposal
	Gate, Child string
	Visit       int
	Value       string
}

// Submission is the agent's answer to an Instruction.
type Submission struct {
	Attempt   int
	Token     string
	Output    json.RawMessage   // typed output, decoded into the state's type
	Artifacts map[string]string // artifact name to produced path
	Child     string
	Tokens    int // tokens the attempt spent, for the run budget
	// Normalized notes how the engine normalized the result file before
	// decoding it (the one tolerated form: a file that is exactly one fenced
	// code block); "" when the file was used as written.
	Normalized string `json:",omitempty"`
	// textWrapped marks an Output the CLI wrapped as a JSON string because
	// the result file was not JSON.
	textWrapped bool
}

// SubmitResult is how a submission was classified and where the run goes next.
//
// A Refused result is a validation refusal of the output, and the CLI exits 5
// for it. From done it spends the attempt (C5): the engine issues a fresh
// attempt while retry remains, else the state takes its failure route. From
// validate nothing is recorded and the child fixes its output.
type SubmitResult struct {
	Class   OutcomeClass
	Refusal []string // check refusals shown to the agent
	Next    string   // name of the instance's next state
	// Failure is set when an accepted Failed/Fail says what happens next; the
	// CLI renders its text and exit code from it alone.
	Failure *FailureOutcome `json:",omitempty"`
}

// RunBudget bounds a whole run; a zero limit is no limit. The Engine journals
// it in run_started (EngineOptions.RunBudget: run --budget, else the
// registration's Budget), and a resume may raise it (resume --budget). Turns count instruction_issued records, less the
// units lost or withdrawn before their attempt_started, so a unit handed out and
// never started costs no turn. Tokens sum the submissions' Tokens and Wall runs
// from run_started's stamp. Once a limit is reached an instruction already open may finish, but an instance about
// to issue a new one is stopped (budget_stopped): a Map child settles as
// Instance.BudgetStopped, a Sub child stops its parent, and a stop that
// reaches the run itself stops the run (RunStatus.Stopped, BudgetStop): it
// has not ended, and an operator's resume --budget that raises the budget
// carries it on from where it stopped. Every ancestor of a stopped instance
// shows Instance.BudgetStoppedBelow.
type RunBudget struct {
	Turns  int           `json:",omitempty"`
	Tokens int           `json:",omitempty"`
	Wall   time.Duration `json:",omitempty"`
}

// Run phases reported by RunStatus.
const (
	RunRunning = "running"
	RunBlocked = "blocked"
	RunEnded   = "ended"
)

// InstanceStatus is one instance in a RunStatus.
type InstanceStatus struct {
	ID       string // "" for the run itself
	State    string // current state of the instance's definition
	Phase    string // ready, issued, transition, done, blocked, backoff, budget_stopped
	Attempt  int
	Reason   string // why a blocked instance halted
	Counters map[string]int
	// Returned is the send-back that entered the current state's visit
	// (SendBack); nil on any other visit.
	Returned *Return `json:",omitempty"`
}

// CoordinatorTurnStatus reports issued continuation decisions, including
// the current no-progress stall when one is recorded.
type CoordinatorTurnStatus struct {
	Continuations int
	LastSeq       int64
	StallSeq      int64 `json:",omitempty"`
}

// RunStatus is where a run stands, as folded from its journal.
type RunStatus struct {
	CoordinatorTurn *CoordinatorTurnStatus `json:",omitempty"`
	usesTracker     bool                   // selects the tracker guidance in the bootstrap
	PromptDelivery  []PromptDeliveryStatus `json:",omitempty"`
	PendingCleanups []PendingCleanup       `json:",omitempty"`
	Workspace       string                 // stable run workspace, used as the tool cwd for workflow commands
	Work            string                 `json:",omitempty"` // what the definition's units do (Def.Describe), for the bootstrap
	Harness         string                 // normalized harness recorded at creation
	LegacyHarness   bool                   `json:",omitempty"` // generic fallback for journals without a harness
	UnitCounts                             // ready, handed and running subagent units

	Run        RunID
	Def        string
	Phase      string            // RunRunning, RunBlocked or RunEnded
	Breakpoint *BreakpointStatus `json:",omitempty"` // active breakpoint, if stopped on arrival
	// Stopped: wf stop or a breakpoint stopped the run; its running children may still
	// finish, and nothing new starts until a person's resume.
	Stopped bool `json:",omitempty"`
	// BudgetStop is why the run budget stopped the run, while that stop
	// stands (Stopped is then set), and BudgetResume the command its
	// operator raises the budget with; Budget is the budget in force.
	BudgetStop   string     `json:",omitempty"`
	BudgetResume string     `json:",omitempty"`
	Budget       RunBudget  `json:",omitzero"`
	Held         []HeldUnit `json:",omitempty"` // units a blocked failure holds
	// Recoveries are the open recovery offers the coordinator answers with
	// wf recover (B2, F1).
	Recoveries []RecoveryView `json:",omitempty"`
	Outcome    Outcome
	Terminal   string
	Cause      string `json:",omitempty"` // an ended run: why it ended (see runCause)
	Instances  []InstanceStatus
	// Predecessor is the ended run this one succeeds, if any.
	Predecessor RunID `json:",omitempty"`
	// Manufactured: the run (or the run it was forked from) was built by
	// wf dev manufacture; its world is scripted, never a model's outcome.
	Manufactured *ManufacturedData `json:",omitempty"`
	// Build is the run's stamp from run_started; nil before stamps.
	Build *BuildStamp `json:",omitempty"`
	// BuildDiffers names how the binary reading the status differs from
	// Build: a note only, the freeze check is separate.
	BuildDiffers string `json:",omitempty"`
	// Detach is the run's detach mode: the one journaled at start (this
	// process's for a run from before G1) lowered by any step-down, "" when
	// none was probed. At DetachNone a long command runs inside next itself.
	Detach string `json:",omitempty"`
	// Children is the latest preflight of each child harness the
	// definition's family hints reach (harness_checked), by harness name.
	Children []HarnessCheckedData `json:",omitempty"`
	// Resume is, on an ended run, the command that continues its work as a
	// successor run.
	Resume string `json:",omitempty"`
	// Tokens is what the run's attempts reported spending: each done --usage
	// and each usage attached with started --usage after a child ended. A
	// child that reports nothing counts zero; no transcript is read for it.
	Tokens int `json:",omitempty"`
	// Degraded is every degraded record the run journaled, in journal order.
	Degraded []DegradedData `json:",omitempty"`
	// Segments are the run's harness segments in journal order.
	Segments []SegmentView `json:",omitempty"`
	// Models is what the current segment spawns with: its bound
	// models, or for a segment not bound yet what binding it from Harness
	// would choose.
	Models *SegmentModels `json:",omitempty"`

	// Options is the autonomy options value journaled in run_started, and
	// Delegations its gate delegations: what a fresh coordinator inherits.
	Options     json.RawMessage   `json:",omitempty"`
	Delegations map[string]string `json:",omitempty"`

	// Units lists every subagent unit in view order (see Engine.Status); the
	// human view renders from it and from the fields below.
	Units    []UnitStatus `json:",omitempty"`
	Progress Progress
	ETA      time.Duration `json:",omitempty"`
	ETAKnown bool          `json:",omitempty"` // some class has history
	Binding  Binding       `json:",omitempty"`
	Future   int           `json:",omitempty"` // agent steps ahead that are not counted in Units
	Started  time.Time     `json:",omitzero"`  // run_started's stamp
	Now      time.Time     `json:",omitzero"`  // the clock sample, so the renderer is a pure function of the status

	// Why the run stands where it does (see Engine.Status).
	Due                []DueTime    `json:",omitempty"` // every deadline the clock decides, earliest first
	OpenIntents        []OpenIntent `json:",omitempty"` // effect intents with no result
	LockHolder         *LockHolder  `json:",omitempty"` // the run's previous file-lock holder, when the journal records one
	NothingReadyReason string       `json:",omitempty"` // why HandOut has no agent work, when the run is not ended

	// Fix is the answer commands of each exhaustion halt the run is held at,
	// each a whole printed command: `retry [--child <id>]` (one more attempt)
	// and, when the state has a fallback edge, the same with `--stop`; then,
	// for each held unit, the `retry` that releases it once its cause is
	// cleared. wf status prints each on its own line under what it does.
	Fix []string `json:",omitempty"`
	// FixNotes says what each Fix command does, one per command in the
	// same order, as prose for its own line: "one more attempt", "its
	// fallback edge: <edge label>", "releases held <unit> once its cause is
	// cleared".
	FixNotes []string `json:",omitempty"`
}

// DueTime is one deadline of the run.
type DueTime struct {
	Kind     string // the deadline kind: "backoff", "hand-out", "lease", ...
	Instance string
	Unit     string `json:",omitempty"`
	Due      time.Time
}

// OpenIntent is an effect intent with no result.
type OpenIntent struct {
	Seq      int64
	Instance string
	Effect   string // "<adapter>.<op>"
	Detached bool   `json:",omitempty"` // has an effect_started
	// Due is the detached effect's maximum-duration due time (its intent's At
	// plus Max).
	Due time.Time `json:",omitzero"`
	// WaitGroup is the process group a re-run waits on to empty before it starts.
	WaitGroup int `json:",omitempty"`
	// Reason is why the launch this intent re-runs died; RerunOf is its intent.
	Reason  string `json:",omitempty"`
	RerunOf int64  `json:",omitempty"`
}

// UnitStatus is one subagent unit in a RunStatus, in view order.
// SegmentView is one harness segment of a run: the seqs it spans
// (End 0 while it is the current one), what opened it, the harness its
// first coordinator bound it to ("" while unbound) and the units handed out
// in it. A unit an earlier segment handed out and its child still finishes
// counts there.
type SegmentView struct {
	Start, End int64
	Cause      string
	Harness    string `json:",omitempty"`
	Units      int
	// ModelMap, Models and ModelsFrom are the segment's SegmentModels; empty
	// for a segment no coordinator bound yet.
	ModelMap   map[string]string `json:",omitempty"`
	Models     map[string]string `json:",omitempty"`
	ModelsFrom string            `json:",omitempty"`
}

type UnitStatus struct {
	PromptCoverage string `json:",omitempty"`
	Name           string
	State          string // UnitState.String(), or "future"
	Class          string
	Attempt        int
	Session        string        `json:",omitempty"` // the claiming coordinator session
	Context        string        `json:",omitempty"` // claimed and settled units: fresh, inherited or unrecorded
	Elapsed        time.Duration `json:",omitempty"` // running, started, stalled, handed; settled: the attempt's duration
	Estimate       time.Duration `json:",omitempty"` // running, pending, waiting, future: the class estimate
	Remaining      time.Duration `json:",omitempty"` // running: estimate - elapsed (negative: over)
	Quiet          time.Duration `json:",omitempty"` // stalled: since the last sign of life
	Window         time.Duration `json:",omitempty"` // handed: the hand-out window
	Note           string        `json:",omitempty"` // "(attempt 1 lost)", "deps", a withdrawal reason
	Observed       string        `json:",omitempty"` // claimed units: the coordinator's latest heartbeat active --note on this attempt's work
	// Harness is the child's harness when it is not the coordinator's (a
	// headless child started by the spawn verb); Pid its process, once
	// started; StartErr, for a handed unit whose child exited without
	// fetching its prompt, the end of that child's stderr.
	Harness  string `json:",omitempty"`
	Pid      int    `json:",omitempty"`
	StartErr string `json:",omitempty"`
}

// JournalKind is the closed set of journal record kinds.
type JournalKind int

// Journal record kinds. The Journal prefix avoids colliding with OutcomeClass's
// Accepted and Rejected.
const (
	JournalRunStarted JournalKind = iota + 1
	JournalInstructionIssued
	JournalSubmitted
	JournalAccepted
	JournalRejected
	JournalEffectIntent
	JournalEffectResult
	JournalObserved
	JournalAutoAnswered
	JournalGateAnswered
	JournalTransition
	JournalLeaseClaimed
	JournalLeaseExpired
	JournalMigrated
	JournalRunEnded
	JournalRefused
	JournalChildSpawned
	JournalChildSettled
	JournalPaused
	JournalResumed
	JournalNote
	JournalRouteOffCandidates
	JournalScopeReset
	JournalBudgetStopped
	JournalPromptOffCandidates
	JournalEffectSettled
	JournalCompletedByObservation
	JournalBackoffScheduled
	JournalHandedOut
	JournalAttemptStarted
	JournalPromptFetched
	JournalHeartbeat
	JournalAttemptLost
	JournalWithdrawn
	JournalLateVote
	JournalEffectStarted
	JournalRetried
	JournalUnitHeld
	JournalMetadataAttached
	JournalPostDoneMutation
	JournalSubmissionRefused
	JournalDegraded
	JournalCoordinator
	JournalRewound
	JournalRecoveryOffered
	JournalRecoveryAnswered
	JournalWorldSnapshot
	JournalUntilUnitBound
	JournalCleanupDeferred
	JournalCleanupApplied
	JournalSessionStopped
	JournalSessionCleared
	// JournalGateOffered is a person gate's offer, once per visit: history
	// only (the fold keeps no state of it), with the checkpoint before it a
	// fork of the gate starts from.
	JournalGateOffered
	JournalBreakpointHalted
	JournalCoordinatorAttached
	JournalCoordinatorDetached
	JournalCoordinatorHandoff
	JournalCoordinatorContinued
	JournalCoordinatorStopStalled
	// JournalHarnessChecked is a segment's preflight of one child harness
	// its definition's family hints reach (HarnessCheckedData).
	JournalHarnessChecked
	// JournalHeadlessStarted is the spawn verb's start of a unit's headless
	// child of another harness (HeadlessStartedData).
	JournalHeadlessStarted
)

var journalKindNames = [...]string{
	JournalCleanupDeferred:        "cleanup_deferred",
	JournalCleanupApplied:         "cleanup_applied",
	JournalSessionStopped:         "session_stopped",
	JournalSessionCleared:         "session_cleared",
	JournalGateOffered:            "gate_offered",
	JournalBreakpointHalted:       "breakpoint_halted",
	JournalCoordinatorAttached:    "coordinator_attached",
	JournalCoordinatorDetached:    "coordinator_detached",
	JournalCoordinatorHandoff:     "coordinator_handoff",
	JournalCoordinatorContinued:   "coordinator_continued",
	JournalCoordinatorStopStalled: "coordinator_stop_stalled",
	JournalHarnessChecked:         "harness_checked",
	JournalHeadlessStarted:        "headless_started",
	JournalUntilUnitBound:         "until_unit_bound",
	JournalWorldSnapshot:          "world_snapshot",
	JournalRunStarted:             "run_started",
	JournalInstructionIssued:      "instruction_issued",
	JournalSubmitted:              "submitted",
	JournalAccepted:               "accepted",
	JournalRejected:               "rejected",
	JournalEffectIntent:           "effect_intent",
	JournalEffectResult:           "effect_result",
	JournalObserved:               "observed",
	JournalAutoAnswered:           "auto_answered",
	JournalGateAnswered:           "gate_answered",
	JournalTransition:             "transition",
	JournalLeaseClaimed:           "lease_claimed",
	JournalLeaseExpired:           "lease_expired",
	JournalMigrated:               "migrated",
	JournalRunEnded:               "run_ended",
	JournalRefused:                "refused",
	JournalChildSpawned:           "child_spawned",
	JournalChildSettled:           "child_settled",
	JournalPaused:                 "paused",
	JournalResumed:                "resumed",
	JournalNote:                   "note",
	JournalRouteOffCandidates:     "route_off_candidates",
	JournalScopeReset:             "scope_reset",
	JournalBudgetStopped:          "budget_stopped",
	JournalPromptOffCandidates:    "prompt_off_candidates",
	JournalEffectSettled:          "effect_settled",
	JournalCompletedByObservation: "completed_by_observation",
	JournalBackoffScheduled:       "backoff_scheduled",
	JournalHandedOut:              "handed_out",
	JournalAttemptStarted:         "attempt_started",
	JournalPromptFetched:          "prompt_fetched",
	JournalHeartbeat:              "heartbeat",
	JournalAttemptLost:            "attempt_lost",
	JournalWithdrawn:              "withdrawn",
	JournalLateVote:               "late_vote",
	JournalEffectStarted:          "effect_started",
	JournalRetried:                "retried",
	JournalUnitHeld:               "unit_held",
	JournalMetadataAttached:       "metadata_attached",
	JournalPostDoneMutation:       "post_done_mutation",
	JournalSubmissionRefused:      "submission_refused",
	JournalDegraded:               "degraded",
	JournalCoordinator:            "coordinator",
	JournalRewound:                "rewound",
	JournalRecoveryOffered:        "recovery_offered",
	JournalRecoveryAnswered:       "recovery_answered",
}

// String returns the snake_case record kind.
func (k JournalKind) String() string {
	if k >= 1 && int(k) < len(journalKindNames) {
		return journalKindNames[k]
	}
	return fmt.Sprintf("JournalKind(%d)", int(k))
}

// JournalRecord is one append-only journal entry. State is the state's name in
// the definition of the instance named by Instance ("" is the run itself, a
// child is "<parent>/<state>#<n>" for a Sub entry or "<parent>/<state>[<key>]"
// for a Map item). At is stamped by the Engine from EngineOptions.Clock: it is
// how wall clock gets journaled, and the fold never reads a clock itself.
type JournalRecord struct {
	Roots         *PathRoots `json:",omitempty"`
	RelativePaths bool       `json:",omitempty"`

	Seq      int64
	Kind     JournalKind
	Run      RunID
	Instance string `json:",omitempty"`
	State    string
	Attempt  int
	Class    OutcomeClass
	DefHash  string
	At       time.Time       `json:",omitzero"`
	Data     json.RawMessage `json:",omitempty"`
}

// Migration maps states of an older definition hash onto a newer one so a run
// can resume across a definition change.
type Migration struct {
	FromHash, ToHash string
	States           map[string]string // old qualified state name to new
}

// ---------------------------------------------------------------- record payloads
//
// Every JournalKind has one typed Data payload, listed in JournalPayloads. The
// fold decodes through that table, so an undecodable payload or an unknown kind
// is ErrCorruptJournal. Fields are plain JSON; nothing here carries a closure.

// EffectData is one effect of a taken edge, as journaled: a counter increment
// or an external adapter write. In an effect_intent record the Args and Key are
// the resolved (bound) ones.
type EffectData struct {
	Kind           string              // "inc" or "external"
	Counter        string              `json:",omitempty"` // inc: the counter name
	Adapter, Op    string              `json:",omitempty"`
	Args           map[string]any      `json:",omitempty"`
	Key            string              `json:",omitempty"`
	NonIdempotent  bool                `json:",omitempty"`
	Reason         string              `json:",omitempty"` // NonIdempotent: why
	Outside        bool                `json:",omitempty"` // wf.Outside: a person settles it when it is uncertain
	BindingVersion int                 `json:",omitempty"`
	Literal        bool                `json:",omitempty"` // versioned runtime edge: only explicit templates bind
	ArgTemplates   []EffectArgTemplate `json:",omitempty"`
	KeyTemplate    []TextSpan          `json:",omitempty"`
}

// EdgeStep is one traversed edge: the source state, the edge's index among the
// source's declared edges, its target and the effects attached to this use, in
// the order they apply.
type EdgeStep struct {
	From     string
	Index    int
	To       string
	Label    string
	Effects  []EffectData `json:",omitempty"`
	Optional bool         `json:",omitempty"` // Edge.Optional: a failed effect does not stop the transition
	RoutedBy string       `json:",omitempty"` // Edge.OptionalRoutedBy: what routes a failed effect
	// SendBack is set on a step that returns work to its producer (SendBack).
	SendBack *SendBackStep `json:",omitempty"`
	// SendBackSpent names the send-back edge whose spent budget this step,
	// the edge's fallback, replaced.
	SendBackSpent string `json:",omitempty"`
	// Auto marks a return the engine planned for a wf.Reject with no
	// declared edge (Index -1).
	Auto bool `json:",omitempty"`
}

// EdgePlan is the edge the Engine chose, journaled so a crash between the
// choice and the transition resumes from the journal and no route closure
// re-runs. More than one step means a MaxVisits over edge replaced the target.
// The plan's target is the last step's To.
type EdgePlan struct{ Steps []EdgeStep }

// ManufacturedData names the script a manufactured run was built from.
type ManufacturedData struct {
	Edge  string `json:",omitempty"` // the script's target edge; "" for a predecessor
	Until string `json:",omitempty"` // "ended" for a predecessor script
	Hash  string // the definition hash the script was written for
	// Script is the script's digest (ScriptDigest): a drive is admitted
	// only for the very script the run was started for.
	Script string `json:",omitempty"`
}

// RunStartedData is the payload of run_started: everything a fresh process
// needs to resume the run. Options is the JSON of the typed options value.
type RunStartedData struct {
	// Manufactured marks a run built by wf dev manufacture (MANUFACTURE.md):
	// its world was scripted, never a model's. A fork keeps run_started, so
	// every fork of it carries the mark too.
	Manufactured *ManufacturedData `json:",omitempty"`
	PagedPrompts bool              `json:",omitempty"`
	HaltBefore   []string          `json:",omitempty"` // canonical state paths selected at run creation
	Snapshots    bool              `json:",omitempty"`
	// Assets and AssetRoots pin the run's selected filesystem roots.
	Assets     string            `json:",omitempty"`
	AssetRoots map[string]string `json:",omitempty"`

	// Shape is the definition's shape: "<definition>/<state>" to the
	// state's kind and result type, for every state of the definition and
	// its children, so a later binary can tell which states an adoption
	// keeps (E1). Journals from before it carry none.
	Shape map[string]string `json:",omitempty"`
	// StateIDs is each state's identity (stateIDs): "<definition>/<state>"
	// to its shape, prompt, checks, edges and whole-view hashes, so a later
	// binary can find the longest prefix of the run its definition leaves
	// unchanged (dev rewind --to unchanged). Journals from before it carry
	// none.
	StateIDs map[string]string `json:",omitempty"`
	// Harness is the normalized coordinator harness. A journal written before
	// slots were removed also carries "Slots"; decoding ignores it.
	Harness string `json:",omitempty"`
	// HarnessFromEnv marks a harness read from the starting agent's own
	// environment (no --harness): the run is driven from it from the start
	// (J1). A person naming --harness from a plain shell does not set it.
	HarnessFromEnv bool `json:",omitempty"`
	// Predecessor is the ended run this one succeeds: run on an ended run
	// starts a successor over the same durable state, with its records.
	Predecessor RunID `json:",omitempty"`
	// Worktrees is, on a successor, the git worktrees it carries on in: those
	// its predecessor chain added and did not remove, computed once when it
	// was created, so its checkpoints read no other run's journal and a fork
	// carries the set. nil on a run that succeeds none, or on a successor
	// from before the field (its capture reads the chain).
	Worktrees *[]string `json:",omitempty"`
	// Given names the params the caller gave (run --param), as against those
	// the registration derived: a resume line repeats only these.
	Given []string `json:",omitempty"`
	// self is the run's own id, set only while a successor is chosen.
	self RunID
	// inherited is, on an ended run while a successor is chosen, the
	// worktree set its successor records as Worktrees.
	inherited []string

	Def        string
	Version    int
	Params     map[string]string   `json:",omitempty"`
	ParamPaths map[string]PathBase `json:",omitempty"`
	Options    json.RawMessage     `json:",omitempty"`
	Workspace  string              `json:",omitempty"`
	ModelMap   map[string]string   `json:",omitempty"`
	// Models is the tier -> concrete model map resolved at creation (already
	// post --model-map). It governs the whole run, resume included; a journal
	// without it predates tiers.
	Models map[string]string `json:",omitempty"`
	// Efforts is the tier -> effort map resolved with Models; an empty effort is
	// journaled as EffortHarnessDefault. A journal without it (models only, as
	// plain strings) loads every tier at harness-default effort.
	Efforts map[string]string `json:",omitempty"`
	// ChildModels are the tier tables of the child harnesses the
	// definition's family hints reach from the start harness, resolved with
	// Models (harness.Headless children); nil when no state hints a family.
	ChildModels map[string]SegmentModels `json:",omitempty"`
	// Headless is the run's configuration of its cross-harness children.
	Headless *HeadlessConfig `json:",omitempty"`
	Budget   RunBudget
	// Delegations maps a qualified gate to the answer its starter delegated to
	// the coordinator; the gate instruction shows it and the engine never answers.
	Delegations map[string]string `json:",omitempty"`
	// Detach is the host's detach mode when the run was created, nil when the
	// effect runner does not probe one (no runner, or a runner from before G1).
	Detach *DetachFact `json:",omitempty"`
	// Build is the source identity of the binary that created the run
	// (BuildStamp). nil on a journal from before stamps. A fork keeps it.
	Build *BuildStamp `json:",omitempty"`
}

// InstructionIssuedData is the payload of instruction_issued: the whole
// instruction, so Next shows the same text and token on every call. Digest
// covers the state's declared observations at issue.
type InstructionIssuedData struct {
	Token    string
	Prompt   string
	Inputs   map[string]string `json:",omitempty"`
	Submit   string
	Dispatch DispatchHints
	Digest   string
	// InputDigest covers the agent's input observations (Agent.Observes) at
	// issue: the staleness token.
	InputDigest string `json:",omitempty"`
	Workspace   string `json:",omitempty"`
	// GitAuthority is the state's declared git authority, nil when it declares none.
	GitAuthority *GitAuthority `json:",omitempty"`
	// ArtifactHashes is no longer written or read: an allocated artifact that
	// exists satisfies done, whichever attempt wrote it. Journals from before
	// that carry the content hash, at issue, of each allocated artifact whose
	// file already existed, and still decode.
	ArtifactHashes map[string]string `json:",omitempty"`
}

// ArtifactRef is a produced artifact: its path and the SHA-256 of its content
// when it was submitted ("" if the file could not be read).
type ArtifactRef struct{ Path, Hash string }

// SubmittedData is the payload of submitted: the raw submission. Normalized
// is the submission's note of how its result file was normalized before
// parsing, if it was.
type SubmittedData struct {
	PromptHash      string `json:",omitempty"`
	PromptShapeHash string `json:",omitempty"`
	Token           string
	Output          json.RawMessage
	Artifacts       map[string]ArtifactRef `json:",omitempty"`
	Tokens          int                    `json:",omitempty"`
	Normalized      string                 `json:",omitempty"`
}

// AcceptedData is the payload of accepted: the normalized output (agent) or
// computed value (Computed), and the edge chosen for it.
type AcceptedData struct {
	Context  *CheckContexts `json:",omitempty"`
	Output   json.RawMessage
	Computed bool `json:",omitempty"`
	Plan     EdgePlan
}

// RejectedData is the payload of rejected: a token that is not the instance's
// latest issued attempt. The record changes no state.
type RejectedData struct{ Token, Reason string }

// RefusedData is the payload of refused, and the base of the other attempt
// failures. The record's Class is the failure class. Spent marks an attempt
// that counts against the retry budget; Plan, when set, is the fallback edge
// taken; Halt stops the instance with a blocked status. With neither Plan nor
// Halt the attempt is re-issued with a fresh token. Submitted marks the
// validation refusal of a submitted output (not an abandonment through wf
// failed): its Reasons are the ones a later attempt is shown. Exhausted marks
// an exhaustion halt (Halt set, no Plan): of an agent state, ExhaustedBudget
// for a spent budget or lost cap and ExhaustedStalled for a stall; of a state
// with no Blocked route whose not-applied effect was not re-issued,
// ExhaustedBudget. It is what wf retry --stop computes its fallback edge from.
type RefusedData struct {
	Reasons     []string  `json:",omitempty"`
	Fingerprint string    `json:",omitempty"`
	Spent       bool      `json:",omitempty"`
	Halt        bool      `json:",omitempty"`
	Plan        *EdgePlan `json:",omitempty"`
	Submitted   bool      `json:",omitempty"`
	Exhausted   string    `json:",omitempty"`
	// Recovery marks an exhaustion halt the coordinator answers (B2): the
	// recovery offer, not a halt for a human.
	Recovery bool `json:",omitempty"`
	// ChildFailed marks a failure the child's own failed verb recorded.
	ChildFailed bool `json:",omitempty"`
}

// RecoveryView is a recovery instruction's evidence and choices: the unit,
// why it is offered, where it stopped (the transcript and the engine's
// reading of its end, for a child that ended without done) and one literal
// command per choice that applies, keyed resume, restart and give-up.
type RecoveryView struct {
	Unit        string            `json:"unit"`
	Cause       string            `json:"cause"`
	Transcript  string            `json:"transcript,omitempty"`
	LastTool    string            `json:"lastTool,omitempty"`
	LastMessage string            `json:"lastMessage,omitempty"`
	Result      string            `json:"result,omitempty"`   // the allocated result file: written or missing
	Restarts    int               `json:"restarts,omitempty"` // restarts left for this unit in the run (an exhaustion offer)
	Resume      string            `json:"resume,omitempty"`   // the harness call that continues the child with the fixer text
	GiveUp      string            `json:"giveUp,omitempty"`   // what give-up does, on the offer of a failure with no route: the fallback edge it takes, or a halt for a person
	Commands    map[string]string `json:"commands"`
}

// RecoveryOfferedData is the payload of recovery_offered (F1): a claimed
// child ended without done or failed, and the coordinator chooses how to
// recover it. The attempt stays open until the answer.
type RecoveryOfferedData struct {
	Unit        string
	Cause       string
	Transcript  string `json:",omitempty"`
	LastTool    string `json:",omitempty"`
	LastMessage string `json:",omitempty"`
	Result      string `json:",omitempty"`
	Resume      string `json:",omitempty"`
}

// RecoveryAnsweredData is the payload of recovery_answered: the
// coordinator's choice for an ended child's offer (RecoveryResume,
// RecoveryRestart or RecoveryGiveUp). A degraded record names it too.
type RecoveryAnsweredData struct {
	Unit   string
	Choice string
	Mtime  string `json:",omitempty"` // resume: the transcript's mtime at the answer
}

// The recovery choices.
const (
	RecoveryResume  = "resume"
	RecoveryRestart = "restart"
	RecoveryGiveUp  = "give-up"
)

// The kinds of an exhaustion halt (RefusedData.Exhausted).
const (
	ExhaustedBudget  = "exhausted" // the retry budget or the lost cap is spent: stop takes the exhausted edge
	ExhaustedStalled = "stalled"   // NoProgressAfter: stop takes the stalled edge if declared, else the exhausted edge
)

// SubmissionRefusedData is the payload of submission_refused: a refusal of
// the submission of Token that left the attempt open, so the same child fixes
// its output and submits again. The engine journals it for the first done of
// an attempt with no output file; an engine before C5 journaled it for every
// validation refusal. Left is the re-submits left on the attempt after this
// refusal. Fingerprint is the one the refusing branch computed, which a later
// loss of the still-open attempt fails it with.
type SubmissionRefusedData struct {
	Token       string
	Reasons     []string
	Left        int
	Fingerprint string `json:",omitempty"`
}

// DegradedKind names what degraded. The other recovered-failure and
// unrecovered-loss kinds join these here.
type DegradedKind string

// DegradedChildCaller: a child verb holding the unit's claim token came from
// another caller than the engine expected (another harness, another
// coordinator's child, not the first fetcher). The call is accepted; older
// journals hold it for calls that were refused.
const DegradedChildCaller DegradedKind = "child caller mismatch"

const (
	// DegradedUsageUnparsed: a child's done passed a --usage that is not
	// tokens=N. The done is accepted and the attempt counts zero tokens: a
	// usage number never blocks a hand-back.
	DegradedUsageUnparsed DegradedKind = "usage unparsed"
	// DegradedEscalated: the sandbox blocked a command the unit needed and its
	// child or the coordinator requested the harness's out-of-sandbox run.
	DegradedEscalated DegradedKind = "escalated"
	// DegradedStoppedSessionRefused is no longer written: the session stop it
	// audited was removed. Journals from before hold it.
	DegradedStoppedSessionRefused DegradedKind = "stopped session refused"
	// DegradedDetach: a detached launch failed and the run stepped down one
	// detach mode. Unit is the command's state, Cause the failure and Action
	// "stepped down <from> -> <to>".
	DegradedDetach DegradedKind = "detach"
	// DegradedOrphan: a new coordinator session found a unit claimed under an
	// earlier session with no live child, and offered it again.
	DegradedOrphan DegradedKind = "orphan"
	// DegradedUnstarted: a unit handed to a coordinator was never fetched
	// while a child of a unit handed to it by a later next did fetch, and it
	// was offered to that coordinator again. Also the note of a whole wait
	// that ended with the unit still unfetched and its offer shown again
	// under the same claim: one per change of evidence (relist).
	DegradedUnstarted DegradedKind = "never started"
	// DegradedChildRefused: a first prompt fetch was refused because its
	// caller is the coordinator's own session (I2, an earlier revision). One record per
	// refusal, so run reports count them; the unit stays offered and comes
	// back to the coordinator if no child starts it. Journals
	// written before the claim token alone admitted a child also hold these
	// for callers the harness's records did not show.
	DegradedChildRefused DegradedKind = "child refused"
	// DegradedChildRefusalsCapped: one unit's identity refusals from one
	// caller passed the cap (refusalCap) and are no longer recorded one by
	// one: the count is at least the cap plus one.
	DegradedChildRefusalsCapped DegradedKind = "child refusals suppressed"
	// DegradedRootChanged: a coordinator of the segment's harness called
	// from another records root than the segment was bound under (a session
	// moved to another profile). The run goes on; one record per change,
	// whose Root is the segment's root from there on.
	DegradedRootChanged DegradedKind = "records root changed"
	// DegradedTiers: the harness has no model for some tiers the definition
	// uses; those children run on the session's model.
	DegradedTiers DegradedKind = "tiers"
	// DegradedHarnessHint: the starting environment carries a harness's weak
	// marker that likely means a missing setup (opencode without its
	// plugin); the harness adapter's sentence says what to do. Journaled once
	// at the run's start; the run goes on.
	DegradedHarnessHint DegradedKind = "harness hint"
	// DegradedEnded: a claimed child's transcript ended on its harness's end
	// marker with no done or failed; the attempt is lost as died and the unit
	// offered again (E2).
	DegradedEnded DegradedKind = "ended without done"
	// DegradedRecovery: the coordinator answered a recovery offer (B2, F1).
	// Cause is why the unit was offered, Action the choice and what followed.
	DegradedRecovery DegradedKind = "recovery"
	// DegradedOptional: an Optional edge went on past a failed effect. Unit
	// is the source state, Cause the effect and its error.
	DegradedOptional DegradedKind = "optional effect failed"
	// DegradedLost: a unit was lost (its child died, went quiet or its lease
	// passed). Action says whether it was offered again or failed at the cap.
	DegradedLost DegradedKind = "lost"
	// DegradedJoin: a Map join was met with seats missing: Cause names them.
	DegradedJoin DegradedKind = "join short"
	// DegradedExhausted: a state took its exhausted (or stalled) edge with no
	// recovery offer (NoRecovery, a non-agent state, or the visit's offer and
	// the unit's restarts already spent): an unrecovered failure.
	DegradedExhausted DegradedKind = "exhausted"
)

// DegradedData is the payload of degraded: one recovered failure or
// unrecovered loss of a unit. Cause is what went wrong (for escalated, the
// command and why the sandbox blocked it) and Action what was done about it
// (for escalated, the outcome of the request). status lists every one.
type DegradedData struct {
	Unit   string
	Kind   DegradedKind
	Cause  string
	Action string
	// Session is the caller session a child refusal is counted by
	// (DegradedChildRefused, DegradedChildRefusalsCapped); empty otherwise.
	Session string `json:",omitempty"`
	// SelfCaller is a first prompt fetch refused as a coordinator call.
	SelfCaller bool `json:",omitempty"`
	// Root is the records root the coordinator moved to
	// (DegradedRootChanged): the segment's current root from this record on.
	Root string `json:",omitempty"`
}

// RouteOffCandidatesData is the payload of route_off_candidates: the edge a
// route closure returned and the declared candidates. The step is Blocked.
type RouteOffCandidatesData struct {
	Returned   string
	Candidates []string
	RefusedData
}

// PromptOffCandidatesData is the payload of prompt_off_candidates: the
// reference a PromptFor selector returned and the declared candidates. The
// step is Blocked.
type PromptOffCandidatesData struct {
	Returned   string
	Candidates []string
	RefusedData
}

// EffectIntentData is the payload of effect_intent: the resolved effect, about
// to be applied. Index is its position among the plan's external effects; the
// record's Seq is the effect id.
//
// Max is set on a detached intent: the .Detached(max) the Engine decided, so
// the bound holds with no effect_started. Rerun is set on the fresh intent of
// an automatic re-run. Mode is the detach mode a detached intent launches on
// (DetachNone: a stepped-down re-run that runs inline); "" when the runner
// probes none.
type EffectIntentData struct {
	// Command marks an engine Command intent: only its bound Args.dir is a path.
	Command bool `json:",omitempty"`
	Index   int
	Effect  EffectData
	Max     time.Duration `json:",omitempty"`
	Rerun   *EffectRerun  `json:",omitempty"`
	Mode    string        `json:",omitempty"`
}

// EffectRerun is the dead launch an effect_intent re-runs: its intent seq and
// process (from its effect_started or, when that was lost, its proc record) and
// why it died (the tail of its .log, else a note naming the pid).
type EffectRerun struct {
	Of         int64
	Pid, Pgid  int
	StartTicks uint64 `json:",omitempty"`
	Reason     string
}

// EffectResultData is the payload of effect_result: the outcome of the intent
// with Seq Intent. A failed effect (OK false) skips the edge's remaining
// external effects.
//
// Outcome says how: "applied" (the adapter ran it), "failed" (it ran and
// errored), "unavailable" (no runtime or the tool is missing: it never ran),
// "uncertain" (its tool call timed out: it may have reached the world),
// "refused" (a sandboxed run refused it: ErrSandboxRefused), "done" (a check
// by its Key found the effect already complete and it was not applied; Result
// is what the check reported, if anything) or "settled" (a human settled a
// Command as applied). Before a keyed effect is applied again after a
// "failed" or "uncertain" result for the same Key in the run, it is checked.
type EffectResultData struct {
	Intent  int64
	Index   int
	OK      bool
	Outcome string          `json:",omitempty"`
	Error   string          `json:",omitempty"`
	Result  json.RawMessage `json:",omitempty"`
}

// EffectSettledData is the payload of effect_settled: a human settled an open
// (Uncertain) effect intent as applied or not. Refused carries what a
// not-applied settlement decided, journaled with the settlement so a crash
// cannot separate them: a keyed, idempotent command not yet re-issued on this
// visit is issued again with nothing spent (Reissued); any other not-applied
// effect classes the step Blocked, which takes the OnClass(Blocked) route, else
// an exhaustion halt answered by retry (or stop, when the state has a fallback
// edge).
type EffectSettledData struct {
	Intent   int64
	Applied  bool
	Refused  *RefusedData `json:",omitempty"`
	Reissued bool         `json:",omitempty"`
	// Result is the RunResult a human observed for a command settled as applied
	// (SettleRun); absent, the command completed with exit 0 and no output.
	Result *RunResult `json:",omitempty"`
	// Evidence is what the settler checked in the world to decide applied or
	// not applied (settle --evidence), kept with the settlement.
	Evidence string `json:",omitempty"`
	// Operator, Reason, Harness and Session are who settled it (SettleAs):
	// the name and reason it gave, if any, and its identity from its
	// environment.
	Operator string `json:",omitempty"`
	Reason   string `json:",omitempty"`
	Harness  string `json:",omitempty"`
	Session  string `json:",omitempty"`
}

// ObservedData is the payload of observed: one adapter read, by its resolved
// descriptor. Purpose is why it was read ("compute", "check", "done_when").
type ObservedData struct {
	Desc        ObservationDesc
	Value       json.RawMessage `json:",omitempty"`
	Error       string          `json:",omitempty"`
	Unavailable bool            `json:",omitempty"`
	Purpose     string          `json:",omitempty"`
	// Issue is the token of the instruction an input observation was read
	// for: at its issue, or at the first render of an attempt adopted onto
	// the input. That issue's prompt and staleness baseline are these reads.
	Issue string `json:",omitempty"`
	// Alias marks a copy of the issue's one read of the descriptor, journaled
	// under another purpose (a further input over it, its staleness token):
	// no adapter read was made, so replay feeds it to none.
	Alias bool `json:",omitempty"`
}

// AutoAnsweredData is the payload of auto_answered: a gate answered by an
// AutonomyPolicy rule, with the rule's park note.
type AutoAnsweredData struct {
	Answer string
	Park   string    `json:",omitempty"`
	Plan   *EdgePlan `json:",omitempty"`
}

// GateAnsweredData is the payload of gate_answered, including an answer relayed by a coordinator.
type GateAnsweredData struct {
	IntValue *int `json:",omitempty"` // resolved integer, including a default proposal
	// Session is the answering CLI process's agent session; omitted when none.
	Session string `json:",omitempty"`
	Answer  string
	Plan    *EdgePlan `json:",omitempty"`
}

// TransitionData is the payload of transition: the record's State is the
// source, To the target, Incs the counters incremented, in order. A
// transition is journaled only when every effect on the edge applied, or the
// failed one rode an Optional edge.
type TransitionData struct {
	To   string
	Incs []string `json:",omitempty"`
}

// LeaseClaimedData is the payload of lease_claimed: a Map child's lease.
type LeaseClaimedData struct {
	Holder  string
	Expires time.Time
}

// LeaseExpiredData is the payload of lease_expired: the lease ran out on the
// journaled clock and the attempt is re-issued.
type LeaseExpiredData struct{ Holder string }

// MigratedData is the payload of migrated: the run moved to a new definition
// hash, mapping old state names to new ones.
type MigratedData struct {
	AssetRoots map[string]string `json:",omitempty"`

	Assets           string            `json:",omitempty"`
	Carried          map[string]string `json:",omitempty"`
	FromHash, ToHash string
	States           map[string]string `json:",omitempty"`
	// Adopted marks a migration the engine made itself (resume --adopt): every
	// live state existed in the new definition unchanged in kind and result
	// type (E1). Shape is the new definition's shape (RunStartedData.Shape).
	Adopted bool              `json:",omitempty"`
	Shape   map[string]string `json:",omitempty"`
	// StateIDs is the new definition's state identities
	// (RunStartedData.StateIDs): the records after this one are judged by them.
	StateIDs map[string]string `json:",omitempty"`
}

// RunEndedData is the payload of run_ended: the terminal reached and its
// outcome.
type RunEndedData struct {
	Terminal string
	Outcome  Outcome
	Result   json.RawMessage `json:",omitempty"`
	Cause    string          `json:",omitempty"` // why a run with no terminal stopped ("blocked: <cause>")
}

// ChildSpawnedData is the payload of child_spawned: a Sub entry or a Map item
// started as a fresh instance. State is the parent's state, Def the child
// definition's name.
type ChildSpawnedData struct {
	ID    string
	Def   string
	State string
	Key   string          `json:",omitempty"`
	Input json.RawMessage `json:",omitempty"`
}

// ChildSettledData is the payload of child_settled: the child reached a
// terminal (or settled without one: Class not Accepted) and what it yielded.
type ChildSettledData struct {
	ID       string
	State    string
	Terminal string
	Outcome  Outcome
	Class    OutcomeClass
	Result   json.RawMessage `json:",omitempty"`
	// Cancelled marks a child its parent cancelled (Class stays Rejected): it
	// counts in no join, and a Rejected settle without the flag is a failure.
	Cancelled bool `json:",omitempty"`
	// Running marks a child settled for its join while its agent attempt goes
	// on: the unit and its name stay live, and the attempt's done is journaled
	// as a late_vote.
	Running bool `json:",omitempty"`
	// Judged marks a settle at a terminal that judged readiness
	// (PartialWhen): its Outcome is success or partial by that judgment. A
	// delivered settle without it (a plain Success terminal, or one recorded
	// before PartialWhen existed) reads "" through ChildOutcome.
	Judged bool `json:",omitempty"`
}

// childOutcome is what ChildOutcome reads for a settle: its outcome, or ""
// for a delivered one no terminal judged.
func (d ChildSettledData) childOutcome() Outcome {
	if d.Outcome.Delivered() && !d.Judged {
		return ""
	}
	return d.Outcome
}

// PausedData is the payload of paused, which wf stop journals: from then on
// next offers nothing and the run starts nothing new; children already
// running finish and submit. wf resume clears it (resumed).
type PausedData struct{ Reason string }

// BreakpointHaltedData records the arrival that paused the whole run before
// executing the state. Instance and halt sequence come from the journal record.
type BreakpointHaltedData struct {
	State      string
	ArrivalSeq int64
	Visit      int
	Reason     string
}

// BreakpointStatus identifies the arrival currently holding a run stopped.
type BreakpointStatus struct {
	BreakpointHaltedData
	Instance string `json:",omitempty"`
	Seq      int64
}

// ResumedData is the payload of resumed (wf resume).
type ResumedData struct {
	// ModelMap is the --model-map the person gave at this resume:
	// the segment it opens resolves it for its harness. Nil carries the
	// previous segment's map on.
	ModelMap map[string]string `json:",omitempty"`
	// Budget is the run budget from this resume on (resume --budget), nil
	// when the resume left it as it was. A resume that clears a budget stop
	// raises it past what the run has spent.
	Budget *RunBudget `json:",omitempty"`
	// Operator, Harness, Session and Reason are who cleared a stop and why:
	// the operator's name for itself, its identity from its environment
	// (Harness "generic" and no session for a plain shell) and its reason
	// (resume --operator --reason).
	Operator string `json:",omitempty"`
	Harness  string `json:",omitempty"`
	Session  string `json:",omitempty"`
	Reason   string `json:",omitempty"`
}

// CoordinatorData is the payload of coordinator: a named session takes over
// driving the run. First appearances order claimed-unit replacement (B4).
// Later returns by a known session update only who last drove the run.
type CoordinatorData struct {
	Harness string `json:",omitempty"`
	Session string
	// Root is where the coordinator's harness keeps its records, from its
	// environment; a segment's first coordinator call pins it (J1).
	Root string `json:",omitempty"`
	// SegmentStart is set on the record that binds a harness segment: the
	// seq of the boundary record that opened it. A coordinator
	// already known from an earlier segment writes such a record again.
	SegmentStart int64 `json:",omitempty"`
	// Models is set on a binding record (SegmentStart set) from an earlier revision on:
	// the tier -> model map the segment spawns with, resolved for its
	// harness. A binding record without it is from before per-segment maps:
	// the segment uses the start's Models, re-resolved for another harness.
	Models *SegmentModels `json:",omitempty"`
}

// SegmentModels is the tier -> model resolution one harness segment spawns
// with: what --model-map, carried or given at the boundary, made of
// the harness's tier table. Hides how it was chosen behind From.
type SegmentModels struct {
	// ModelMap is the --model-map in force: given at the boundary, carried
	// from the previous segment, or the start's; nil for harness defaults.
	ModelMap map[string]string `json:",omitempty"`
	Models   map[string]string `json:",omitempty"`
	Efforts  map[string]string `json:",omitempty"`
	From     string            // one of the ModelsFrom constants
	// Harness is the harness they were resolved for; a snapshot fork's note
	// keeps it with the models, so the fork's history keeps its harness
	// without inheriting its coordinator.
	Harness string `json:",omitempty"`
	// Children are the tier tables of the child harnesses the definition's
	// family hints reach from Harness, keyed by child harness: journaled with
	// the segment, so a resume spawns them as the binding did.
	Children map[string]SegmentModels `json:",omitempty"`
}

// Where a segment's models came from (SegmentModels.From).
const (
	ModelsFromStart    = "start"    // the run's creation: run_started's Models
	ModelsFromMap      = "map"      // the --model-map given at this boundary
	ModelsFromCarried  = "carried"  // the previous segment's, same harness
	ModelsFromDefaults = "defaults" // another harness and no map: its table
)

// NoteData is the payload of note: free text for the audit trail.
type NoteData struct {
	Text string
	// SegmentStart and Models keep a binding coordinator record's recorded
	// segment models when a snapshot fork turns that record into a note
	//: the fork's history spawned with them, and a later segment
	// carries them on. Unset on any other note.
	SegmentStart int64          `json:",omitempty"`
	Models       *SegmentModels `json:",omitempty"`
}

// ScopeResetData is the payload of scope_reset: the counters zeroed because
// Scope was incremented.
type ScopeResetData struct {
	Scope    string
	Counters []string
}

// BudgetStoppedData is the payload of budget_stopped: an instance the executor
// stopped on the run budget.
type BudgetStoppedData struct{ Reason string }

// CompletedByObservationData is the payload of completed_by_observation: the
// state's DoneWhen was met, so it completed without being issued. Output is
// the last accepted output (or the zero value) the route ran on.
type CompletedByObservationData struct {
	Output json.RawMessage
	Plan   EdgePlan
}

// BackoffScheduledData is the payload of backoff_scheduled: a class retry due
// at Due, Index being the position in the OnClassBackoff delay schedule.
type BackoffScheduledData struct {
	Class OutcomeClass
	Index int
	Delay time.Duration
	Due   time.Time
	// Reasons are the failure's own reasons (a missing working directory,
	// an unavailable adapter's message), so whoever drives the run sees
	// what to fix before the retry.
	Reasons []string `json:",omitempty"`
	// ChildFailed marks a backoff the child's own failed verb scheduled.
	ChildFailed bool `json:",omitempty"`
}

// HandedOutData is the payload of handed_out: the unit's name, the internal
// token it maps to, the coordinator session it was handed to and when the
// hand-out window closes (decided by the Engine from its clock). A hand-out
// whose window closed before any claim is journaled again for the same unit
// and name.
type HandedOutData struct {
	Name    string
	Token   string
	Session string `json:",omitempty"` // the coordinator session the unit was handed to
	// Harness is the calling coordinator's harness, read from its
	// environment on each next (A3a), not the run creator's.
	Harness string    `json:",omitempty"`
	Expires time.Time `json:",omitzero"`
	// Root is where the coordinator's harness keeps its records (transcripts,
	// delegation logs, rollouts), read from its environment at next; a child's
	// usage and liveness are read from them. Empty for a library hand-out.
	Root string `json:",omitempty"`
	// Claim is the claim token this hand-out minted; the spawn brief carries it
	// as wf prompt --claim. A re-offer of the same unit mints a fresh one.
	Claim string `json:",omitempty"`
	// Output is the allocated result path, relative to the run's out
	// directory. Empty in journals written before hand-outs recorded it: those
	// units keep the legacy spelling.
	Output string `json:",omitempty"`
	// Segment is the start seq of the harness segment the hand-out was made
	// in: a unit's child verbs are attributed to it, and must come
	// from its harness. Zero in journals from before segments.
	Segment int64 `json:",omitempty"`
	// Call is the Seq of the first hand-out of the next call that made this
	// one: every hand-out of one call carries it, and later calls have higher
	// ones whatever the clock says. Zero in journals written before hand-outs
	// recorded it: each such record is its own call, its own Seq.
	Call int64 `json:",omitempty"`
	// Spawn is what the unit is spawned with, fixed at this hand-out from
	// its segment's models. Every hand-out from an earlier revision on records
	// it, empty fields meaning the session's own model or the harness
	// default; nil is a hand-out from before, which recorded none.
	Spawn *HandOutSpawn `json:",omitempty"`
}

// HarnessCheckedData is a segment's preflight of a child harness: its CLI
// is on PATH, logged in, and (for a coordinator whose sandbox would confine
// the launch) the spawn verb is allowed to start it outside that sandbox.
// An unavailable harness fails its units with class Unavailable; it never
// blocks the run.
type HarnessCheckedData struct {
	Harness     string // the child harness
	Coordinator string // the coordinator harness that would launch it
	Available   bool
	Version     string `json:",omitempty"` // the CLI's --version, for harness identity records
	Why         string `json:",omitempty"` // why unavailable
	Fix         string `json:",omitempty"` // what a person does about it
}

// HeadlessStartedData is the spawn verb's start of a headless child: the
// unit and the claim it was started for, its harness, its process (pid and
// the kernel's start time of that pid, which guards against pid reuse), the
// session id its launch named ("" when the CLI chose its own), its stderr
// and stdout files, and its supervisor's status file (HeadlessProcess),
// through which every later verb reads its state and asks for its stop.
// A record without Status (an earlier binary's) is judged by its pid.
type HeadlessStartedData struct {
	Name    string
	Claim   string
	Harness string
	Pid     int
	Start   uint64 `json:",omitempty"`
	Session string `json:",omitempty"`
	ErrLog  string `json:",omitempty"`
	Out     string `json:",omitempty"`
	Status  string `json:",omitempty"`
}

// HandOutSpawn is the model and effort a hand-out spawned its child with.
type HandOutSpawn struct {
	Model  string `json:",omitempty"`
	Effort string `json:",omitempty"`
	ErrLog string `json:",omitempty"` // the claim's stderr path; empty for an adapter without StartLog
	// Harness is the child's harness when it is not the coordinator's: the
	// unit is started by the spawn verb as a headless process of it.
	Harness string `json:",omitempty"`
}

// AttemptStartedData is the payload of attempt_started: the claim of a handed
// unit, the lease expiry and the coordinator session. lease_claimed (old
// journals) folds into the same claim.
type AttemptStartedData struct {
	Name       string
	Harness    string      `json:",omitempty"`
	Session    string      `json:",omitempty"`
	Agent      string      `json:",omitempty"`
	Transcript string      `json:",omitempty"` // absolute, as wf started recorded it
	Context    ContextMode `json:",omitempty"` // as wf started --context reported it
	Root       string      `json:",omitempty"` // the harness's transcript root, for the engine's own search
	Env        bool        `json:",omitempty"` // claimed by a prompt fetch that read the child's environment
	Expires    time.Time   `json:",omitzero"`
}

// MetadataAttachedData is the payload of metadata_attached: a wf started that
// arrived after the unit was claimed or ended added compatible ownership
// metadata. Late is always true; State is the unit's state at the attach.
type MetadataAttachedData struct {
	Name       string
	Harness    string      `json:",omitempty"`
	Session    string      `json:",omitempty"`
	Agent      string      `json:",omitempty"`
	Transcript string      `json:",omitempty"`
	Context    ContextMode `json:",omitempty"`
	Root       string      `json:",omitempty"`
	Env        bool        `json:",omitempty"`
	State      string
	Late       bool
	// Tokens is usage attached after the child ended: by the coordinator
	// (started --usage), or by the engine from the transcript of a failed
	// attempt (Resolved); it counts toward the run's tokens like done --usage.
	Tokens int `json:",omitempty"`
	// Resolved marks metadata the engine found itself (the child's transcript
	// by its claim token), not a sign of the child's life.
	Resolved bool `json:",omitempty"`
}

// PromptFetchedData is the payload of prompt_fetched.
type PromptFetchedData struct {
	// Caller binds the first tokened fetch independently of resolved transcript metadata.
	Caller       *Claim        `json:",omitempty"`
	Packet       *PromptPacket `json:",omitempty"`
	PacketDigest string        `json:",omitempty"`
	Page         int           `json:",omitempty"`
	Reference    string        `json:",omitempty"`
	// PromptHash hashes the delivered body; PromptShapeHash ignores run-local identity.
	PromptHash      string `json:",omitempty"`
	PromptShapeHash string `json:",omitempty"`
	Name            string
	// Claim is the claim token the fetch carried; the first one binds to the
	// unit. Empty for a tokenless fetch.
	Claim string `json:",omitempty"`
	// PromptDir is the directory the prompt's body file (prompt.md) was
	// written to (E1); empty when there is no run dir and it was printed
	// inline. Older journals name a directory of part files.
	PromptDir string `json:",omitempty"`
	// Whole marks a fetch that handed the child the whole prompt (wf prompt
	// --json).
	Whole bool `json:",omitempty"`
}

// HeartbeatData is the payload of heartbeat: active (the subagent is working,
// Expires renews the lease; By "transcript" stamps a transcript write), stalled
// (the engine saw no transcript write past the class threshold), asked (a
// blocking next put the stall question to the coordinator: the clock of its
// rechecks) or died, and who reported it.
type HeartbeatData struct {
	Name    string
	Status  string    // "active", "stalled", "asked" or "died"
	By      string    `json:",omitempty"`
	Expires time.Time `json:",omitzero"`
	// Written is the transcript write a by-transcript heartbeat reports; zero
	// (any other heartbeat, or an old record) means the record's own At.
	Written time.Time `json:",omitzero"`
	// Note is the coordinator's account of the work it saw (HeartbeatTarget.Note).
	Note string `json:",omitempty"`
	// Unprogressed is, on a coordinator's active answer, how many active
	// answers in a row (this one included) came with no sign of life from
	// the child since the one before: 1 for the first after any progress.
	// Zero on every other heartbeat and on older records.
	Unprogressed int `json:",omitempty"`
}

// AttemptLostData is the payload of attempt_lost: the cause (one of the
// LostCause* constants). Below the lost cap the unit returns to pending at
// attempt+1; with cause LostCauseCap the attempt settles failed.
type AttemptLostData struct {
	Name  string
	Cause string
	// Reason is LostCauseCap's exhaustion reason: the refused record that
	// follows carries it, and a drive after a crash between the two
	// journals that refusal (the fold keeps it pending).
	Reason string `json:",omitempty"`
}

// Attempt-lost causes.
const (
	LostCauseSpawn  = "spawn expired"
	LostCauseLease  = "lease expired"
	LostCauseDied   = "died"
	LostCauseOrphan = "session orphan"
	LostCauseCap    = "lost cap"
	// LostCauseFork is a unit live at a fork point: its child ran
	// in the source run only. It spends no retry, is never failed as an open
	// refusal, and is not counted toward the lost cap.
	LostCauseFork = "fork orphan"
)

// WithdrawnData is the payload of withdrawn: a pending or handed unit that left
// its source, or a seat freed by an early decide.
type WithdrawnData struct {
	Name   string
	Reason string
}

// LateVoteData is the payload of late_vote: a started sibling's result after
// its join was decided early. It never reopens the decision. Tokens is the
// usage the sibling reported with done --usage, counted toward the run's
// tokens once, like a submission's.
type LateVoteData struct {
	Name       string
	Output     json.RawMessage `json:",omitempty"`
	Tokens     int             `json:",omitempty"`
	Normalized string          `json:",omitempty"` // as SubmittedData.Normalized
}

// EffectStartedData is the payload of effect_started: the detached runner of a
// .Detached() command, journaled against its effect_intent.
type EffectStartedData struct {
	Intent int64
	Pid    int
	Pgid   int
	// StartTicks is the leader's /proc start time in clock ticks.
	StartTicks uint64 `json:",omitempty"`
	// Start is the wall start time a legacy record carried; new launches leave
	// it zero (the maximum duration runs from the intent's At).
	Start time.Time `json:",omitzero"`
	// Max is the maximum duration the Engine decided at launch.
	Max time.Duration `json:",omitempty"`
}

// RetriedData is the payload of retried: an operator returned a halted
// instance to ready. Child is the instance ("" is the run itself) and Reason
// the halt reason it clears, kept for the audit. An exhaustion halt has two
// answers: retry (Answer "", one more attempt) and stop (Answer RetryStop,
// which takes Plan, the fallback edge computed from the halt's Exhausted when
// it was answered; Plan is set for stop only).
type RetriedData struct {
	Child  string
	Reason string
	Answer string    `json:",omitempty"`
	Plan   *EdgePlan `json:",omitempty"`
}

// RetryStop is the stop answer of an exhaustion halt (RetriedData.Answer).
const RetryStop = "stop"

// PostDoneMutationData is the payload of post_done_mutation: a task ref that
// no longer points at the commit merged for its unit (the ref moved after the
// agent reported done). git.Merge produces it by comparing TaskRef's target
// with Commit and reports it in its Apply result (PostDoneMutationReport); the
// engine journals it and fills Unit.
type PostDoneMutationData struct {
	Unit      string // set by the engine: the effect's instance path and state, as a unit name without its #k
	TaskRef   string // the task ref name, e.g. task-{id}
	RefTarget string // the commit TaskRef points at now
	Commit    string // the commit that was merged
}

// PostDoneMutationReport is how an adapter's Apply result reports a
// post_done_mutation: a result that is a JSON object with a non-null
// post_done_mutation field, returned with or without an error, makes the
// engine journal post_done_mutation right after the effect's effect_result.
// The adapter leaves Unit empty.
type PostDoneMutationReport struct {
	PostDoneMutation *PostDoneMutationData `json:"post_done_mutation,omitempty"`
}

// JournalPayloads maps every JournalKind to the type of its Data payload.
var JournalPayloads = map[JournalKind]reflect.Type{
	JournalCleanupDeferred:        reflect.TypeFor[CleanupDeferredData](),
	JournalCleanupApplied:         reflect.TypeFor[CleanupAppliedData](),
	JournalSessionStopped:         reflect.TypeFor[SessionStoppedData](),
	JournalSessionCleared:         reflect.TypeFor[SessionClearedData](),
	JournalGateOffered:            reflect.TypeFor[GateOfferedData](),
	JournalUntilUnitBound:         reflect.TypeFor[UntilUnitBoundData](),
	JournalRunStarted:             reflect.TypeFor[RunStartedData](),
	JournalInstructionIssued:      reflect.TypeFor[InstructionIssuedData](),
	JournalSubmitted:              reflect.TypeFor[SubmittedData](),
	JournalAccepted:               reflect.TypeFor[AcceptedData](),
	JournalRejected:               reflect.TypeFor[RejectedData](),
	JournalEffectIntent:           reflect.TypeFor[EffectIntentData](),
	JournalEffectResult:           reflect.TypeFor[EffectResultData](),
	JournalObserved:               reflect.TypeFor[ObservedData](),
	JournalAutoAnswered:           reflect.TypeFor[AutoAnsweredData](),
	JournalGateAnswered:           reflect.TypeFor[GateAnsweredData](),
	JournalTransition:             reflect.TypeFor[TransitionData](),
	JournalLeaseClaimed:           reflect.TypeFor[LeaseClaimedData](),
	JournalLeaseExpired:           reflect.TypeFor[LeaseExpiredData](),
	JournalMigrated:               reflect.TypeFor[MigratedData](),
	JournalRunEnded:               reflect.TypeFor[RunEndedData](),
	JournalRefused:                reflect.TypeFor[RefusedData](),
	JournalChildSpawned:           reflect.TypeFor[ChildSpawnedData](),
	JournalChildSettled:           reflect.TypeFor[ChildSettledData](),
	JournalPaused:                 reflect.TypeFor[PausedData](),
	JournalBreakpointHalted:       reflect.TypeFor[BreakpointHaltedData](),
	JournalCoordinatorAttached:    reflect.TypeFor[coordinatorAttachment](),
	JournalCoordinatorDetached:    reflect.TypeFor[coordinatorAttachment](),
	JournalCoordinatorHandoff:     reflect.TypeFor[CoordinatorHandoffData](),
	JournalCoordinatorContinued:   reflect.TypeFor[CoordinatorContinuationData](),
	JournalCoordinatorStopStalled: reflect.TypeFor[CoordinatorContinuationData](),
	JournalHarnessChecked:         reflect.TypeFor[HarnessCheckedData](),
	JournalHeadlessStarted:        reflect.TypeFor[HeadlessStartedData](),
	JournalResumed:                reflect.TypeFor[ResumedData](),
	JournalNote:                   reflect.TypeFor[NoteData](),
	JournalRouteOffCandidates:     reflect.TypeFor[RouteOffCandidatesData](),
	JournalScopeReset:             reflect.TypeFor[ScopeResetData](),
	JournalBudgetStopped:          reflect.TypeFor[BudgetStoppedData](),
	JournalPromptOffCandidates:    reflect.TypeFor[PromptOffCandidatesData](),
	JournalEffectSettled:          reflect.TypeFor[EffectSettledData](),
	JournalCompletedByObservation: reflect.TypeFor[CompletedByObservationData](),
	JournalBackoffScheduled:       reflect.TypeFor[BackoffScheduledData](),
	JournalHandedOut:              reflect.TypeFor[HandedOutData](),
	JournalAttemptStarted:         reflect.TypeFor[AttemptStartedData](),
	JournalPromptFetched:          reflect.TypeFor[PromptFetchedData](),
	JournalHeartbeat:              reflect.TypeFor[HeartbeatData](),
	JournalAttemptLost:            reflect.TypeFor[AttemptLostData](),
	JournalWithdrawn:              reflect.TypeFor[WithdrawnData](),
	JournalLateVote:               reflect.TypeFor[LateVoteData](),
	JournalEffectStarted:          reflect.TypeFor[EffectStartedData](),
	JournalRetried:                reflect.TypeFor[RetriedData](),
	JournalUnitHeld:               reflect.TypeFor[UnitHeldData](),
	JournalMetadataAttached:       reflect.TypeFor[MetadataAttachedData](),
	JournalPostDoneMutation:       reflect.TypeFor[PostDoneMutationData](),
	JournalSubmissionRefused:      reflect.TypeFor[SubmissionRefusedData](),
	JournalDegraded:               reflect.TypeFor[DegradedData](),
	JournalCoordinator:            reflect.TypeFor[CoordinatorData](),
	JournalRewound:                reflect.TypeFor[RewoundData](),
	JournalRecoveryOffered:        reflect.TypeFor[RecoveryOfferedData](),
	JournalRecoveryAnswered:       reflect.TypeFor[RecoveryAnsweredData](),
	JournalWorldSnapshot:          reflect.TypeFor[WorldSnapshotData](),
}

// CleanupDeferredData transfers a worktree removal from an edge to run cleanup.
// Intent is nonzero when recovering an already-recorded edge effect intent.
// The edge advances without claiming that this effect was applied.
type CleanupDeferredData struct {
	Index  int
	Intent int64 `json:",omitempty"`
	Effect EffectData
}

// CleanupAppliedData records a deferred removal verified done or applied.
type CleanupAppliedData struct{ Deferred int64 }

// PendingCleanup is a durable removal still awaiting a safe workspace.
type PendingCleanup struct {
	Deferred int64
	Instance string
	Effect   string // adapter.operation awaiting cleanup
}

// SessionStoppedData is the payload of session_stopped, which is no longer
// written: a coordinator verb that ends on an error stops no session, and
// the next call simply runs. Journals from before hold the record; it folds
// to no state.
type SessionStoppedData struct {
	Harness string `json:",omitempty"`
	Session string
	Verb    string
	Text    string
}

// GateOfferedData is the payload of gate_offered: the person gate State
// (qualified) offered at Visit, with its Question. It records the offer, so
// a fork can be cut just before it; it changes no state (no token, phase or
// turn), and a journal from before it has none for its gates.
type GateOfferedData struct {
	State    string
	Visit    int
	Question string
}

// SessionClearedData is the payload of session_cleared, which is no longer
// written (see SessionStoppedData). Journals from before hold the record;
// it folds to no state.
type SessionClearedData struct {
	Session string `json:",omitempty"`
}
