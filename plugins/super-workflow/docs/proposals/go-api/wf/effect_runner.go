package wf

import (
	"fmt"
	"slices"
	"time"
)

// DetachedProcess is a launched detached runner: as journaled in effect_started
// or read from its proc record.
type DetachedProcess struct {
	Pid, Pgid  int
	StartTicks uint64    // /proc/<pid>/stat field 22 of the leader; 0 on a legacy record
	Start      time.Time // legacy only: the wall start time an old effect_started carried
}

// DetachedStatus is what a detached effect's result file and process group say.
type DetachedStatus int

// Detached statuses.
const (
	DetachedRunning  DetachedStatus = iota + 1 // leader alive, ticks match, not state Z
	DetachedPresent                            // the result file exists
	DetachedDeadLive                           // leader gone, no result file, group members still live
	DetachedDead                               // leader gone, no result file, group empty (or recycled)
)

// DetachedRequest is one detached launch.
type DetachedRequest struct {
	// Context identifies the owning state instance on Start; path-only polls omit it.
	Context                       EffectContext `json:",omitempty"`
	Argv                          []string
	Dir                           string // absolute
	ResultPath, ProcPath, LogPath string // <run dir>/effects/<intent-seq>.{json,proc,log}
	// Mode is the detach mode to launch on: DetachSystemd or DetachProcess, and
	// only that one; "" is the runner's own probed mode.
	Mode string `json:",omitempty"`
}

// DetachedReport is what Status found.
type DetachedReport struct {
	Status DetachedStatus
	Result RunResult // DetachedPresent only
	Reason string    // tail (4 KiB) of LogPath when not running and not present, else ""
}

// The detach modes of a host (escalation-and-recovery G1): how a detached
// command can outlive the command that launched it.
const (
	DetachSystemd = "systemd" // a transient user unit: a user bus is present
	DetachProcess = "process" // a double fork survives its command and is visible from the next
	DetachNone    = "none"    // nothing survives its command (a per-command PID namespace)
)

// detachLadder is the order the modes step down in. A run starts on its host's
// mode; a launch that fails steps the run down one mode for good (journaled as
// a degraded record of kind detach), and DetachNone runs the command inline.
var detachLadder = []string{DetachSystemd, DetachProcess, DetachNone}

// stepBelow is the mode under mode on the ladder, "" when there is none.
func stepBelow(mode string) string {
	if i := slices.Index(detachLadder, mode); i >= 0 && i+1 < len(detachLadder) {
		return detachLadder[i+1]
	}
	return ""
}

// lowerMode is the lower of two modes on the ladder; one not on it ("") loses.
func lowerMode(a, b string) string {
	if slices.Index(detachLadder, b) > slices.Index(detachLadder, a) {
		return b
	}
	return a
}

// ErrNotLaunched wraps a Start error that launched nothing: the requested mode
// cannot launch on this host (systemd-run missing, unrunnable or refusing), so
// the engine steps down to the next mode at once.
var ErrNotLaunched = fmt.Errorf("%w: nothing launched", ErrUnavailable)

// DetachFact is a host's detach mode with the probe's inputs, so a misread is
// diagnosable from the journal: the field count of /proc/self/status's NSpid
// line (more than one is a nested PID namespace), pid 1's name in that
// namespace and what the systemd probe found.
type DetachFact struct {
	Mode        string
	NSpidFields int    `json:",omitempty"`
	Pid1        string `json:",omitempty"` // /proc/1/comm: which sandbox owns a nested namespace
	Systemd     string `json:",omitempty"` // DetachSetBy first: the mode was stated, not probed
}

// DetachSetBy opens a DetachFact's Systemd when the user stated the mode
// (WF_DETACH_MODE): a choice, so it journals no degraded record.
const DetachSetBy = "set by "

// DetachProber is an EffectRunner that knows its host's detach mode, probed
// once per process. The engine journals it at run start, and with
// DetachNone it runs .Detached() commands inline: a launch would die with the
// command that made it.
type DetachProber interface {
	DetachMode() DetachFact
}

// EffectRunner runs a .Detached() shell command so it outlives the CLI call
// that started it.
//
// Owns: starting the command in its own session and process group, the
// pre-launch proc record (pgid and leader start ticks, written before the
// command executes), the atomic result file with tailed output, the .log of the
// runner's own failures, and the operations drive calls: Status, Launched,
// Terminate and GroupEmpty (before an automatic re-run). Ownership is the
// process group's (Terminate signals -pgid whenever the group is this launch's
// and non-empty, leader alive or not); running is the leader's.
// Does not: journal, decide recovery, classify outcomes, or run an effect that
// is not declared .Detached(): the Engine journals effect_started and
// effect_result, and recovery chooses between the result file, a running launch
// (a wait instruction), a re-run, Check and Uncertain. wf never imports shell.
// Extend by: a new execution environment is one implementation wired in by
// the binary (MainConfig.EffectRunner); a nil runner runs detached commands
// inline (MemJournal, wftest).
type EffectRunner interface {
	// Start launches on r.Mode only and returns once ProcPath exists; a failure
	// is Unavailable, and wraps ErrNotLaunched when nothing was launched.
	Start(r DetachedRequest) (DetachedProcess, error)
	Launched(r DetachedRequest) (DetachedProcess, bool, error) // reads ProcPath; ok false when absent
	Status(p DetachedProcess, r DetachedRequest) (DetachedReport, error)
	// Terminate stops the launch: -pgid whenever the group is this launch's
	// and non-empty, or, for a runner in another PID namespace, a stop request
	// beside its result file that the runner answers. r names the launch's
	// paths. A zero p (no launch known) fences r's paths: a runner that starts
	// on them later exits before running its command, and a runner whose proc
	// record is already there keeps running (the fence is withdrawn for it, as
	// the engine adopts it).
	Terminate(p DetachedProcess, r DetachedRequest) error
	// GroupEmpty is true when the group is empty or recycled (not this
	// launch's); for a runner in another PID namespace, when its heartbeat has
	// gone stale.
	GroupEmpty(p DetachedProcess, r DetachedRequest) (bool, error)
}
