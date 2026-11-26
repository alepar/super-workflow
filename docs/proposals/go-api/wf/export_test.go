package wf

import (
	"runtime/debug"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// HarnessEnvVars is the test helper harnessEnvVars, for wf_test.
var HarnessEnvVars = harnessEnvVars

// DetachNoneAdvice is the bootstrap's detach-none line, for tests.
const DetachNoneAdvice = detachNoneAdvice

// SetFileWatchPoll sets how often a file journal's Watch polls, for tests; the
// returned func restores the previous value.
func SetFileWatchPoll(d time.Duration) (restore func()) {
	old := fileWatchPoll
	fileWatchPoll = d
	return func() { fileWatchPoll = old }
}

// SetHarnessTierModel overrides one harnessTiers entry, model and effort, for
// tests; the returned func restores it.
func SetHarnessTierModel(harness string, t Tier, tm TierModel) (restore func()) {
	old, had := harnessTiers()[harness][t]
	harnessTiers()[harness][t] = tm
	return func() {
		if had {
			harnessTiers()[harness][t] = old
		} else {
			delete(harnessTiers()[harness], t)
		}
	}
}

// Depths is the Engine's critical depth of each state of d, for tests.
func Depths(e Executor, d *Def) map[string]int { return e.(*Engine).depths(d) }

// SetOneVerb makes e skip a child-verb check that already passed in it, as
// the CLI's one-verb engine does, for tests.
func SetOneVerb(e *Engine) { e.opts.oneVerb = true }

// SetChildClaim gives e the claim token a child verb's CLI call carried
// (--claim), for tests of a verb's own check under its lock.
func SetChildClaim(e *Engine, claim string) { e.opts.childClaim = claim }

// SetSnapshotRunDataHook runs h just before and just after a capture copies
// the run data, for tests; the returned func removes it.
func SetSnapshotRunDataHook(h func(when string)) (restore func()) {
	snapshotRunDataHook = h
	return func() { snapshotRunDataHook = nil }
}

// SetUnstartedGrace sets UnstartedGrace's default, for CLI tests; the
// returned func restores it.
func SetUnstartedGrace(d time.Duration) (restore func()) {
	old := unstartedGrace
	unstartedGrace = d
	return func() { unstartedGrace = old }
}

// Driver is the harness run is driven from (J1), for tests.
func Driver(e *Engine, run RunID) (string, error) {
	return e.driver(run)
}

// StateNameOf is the state name a marker type S derives, for tests.
func StateNameOf[S any]() string { return stateName[S]() }

// WithManufactured is opts marking the runs it starts as manufactured, as
// wf dev manufacture does, for tests.
func WithManufactured(opts EngineOptions, m *ManufacturedData) EngineOptions {
	opts.manufactured = m
	return opts
}

// SetBetweenLocksHook runs f where run and done have committed under one
// run-lock acquisition and not yet taken the next, until restore.
func SetBetweenLocksHook(f func(cmd string, run RunID)) (restore func()) {
	betweenLocksHook = f
	return func() { betweenLocksHook = nil }
}

// SetYieldedHook runs f each time a drive has the run lock back after letting
// it go to a waiting call, until restore.
func SetYieldedHook(f func(RunID)) (restore func()) {
	yieldedHook = f
	return func() { yieldedHook = nil }
}

// SetFileLockWait sets the lock wait of a caller with no budget, until restore.
func SetFileLockWait(d time.Duration) (restore func()) {
	old := fileLockWait
	fileLockWait = d
	return func() { fileLockWait = old }
}

// ExitBusy is the exit code of a call that found the run busy and changed
// nothing.
const ExitBusy = exitBusy

// SetManufactureAdmittedHook runs f between a manufacture drive's admission
// and its first hand-out, until restore.
func SetManufactureAdmittedHook(f func(RunID)) (restore func()) {
	manufactureAdmittedHook = f
	return func() { manufactureAdmittedHook = nil }
}

// ObserveReplay makes every replay hand f its replayed journal; the returned
// func restores the default.
func ObserveReplay(f func([]JournalRecord)) func() {
	replayObserved = f
	return func() { replayObserved = nil }
}

// UninheritedCoordinator exposes a snapshot fork's coordinator rewrite.
var UninheritedCoordinator = uninheritedCoordinator

// OutputReserveBytes is the hand-out sizing's reserve for non-offer text.
const OutputReserveBytes = outputReserveBytes

// WfLine is the named binary's command line (wfLine), for tests.
func WfLine(bin, stateDir string, run RunID, verb string, args ...string) string {
	return wfLine(bin, stateDir, run, verb, args...)
}

// WorkspaceCommand anchors a printed command at the run root, for tests.
func WorkspaceCommand(workspace, command string) string {
	return workspaceCommand("", workspace, command)
}

// RepeatedFailed reports whether a failed by caller with claim, reason and
// class would be told the claim's unit already ended by an identical failed,
// for tests.
func RepeatedFailed(e Executor, run RunID, claim, reason, class string, caller harness.Caller) (bool, error) {
	en := e.(*Engine)
	old := en.opts.caller
	en.opts.caller = caller
	defer func() { en.opts.caller = old }()
	repeated := false
	err := en.withDryRun(run, func(c *cycle) error {
		parsed, err := ParseFailureClass(class)
		repeated = err == nil && (parsed == FailApproval || c.unitByClaim(claim) == "") &&
			c.repeatedFailed("", claim, reason, parsed) != nil
		return nil
	})
	return repeated, err
}

// SetBuildInfo replaces the build info the stamp reads, for tests; the
// returned func restores it.
func SetBuildInfo(bi *debug.BuildInfo) (restore func()) {
	old := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, bi != nil }
	return func() { readBuildInfo = old }
}
