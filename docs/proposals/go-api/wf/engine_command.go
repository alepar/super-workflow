package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// commandOps is the typed half of a Command: it runs the argv through the
// adapters' shell runtime as a journaled effect (effect_intent, Apply,
// effect_result), and routes on the RunResult. A non-zero exit is data, not a
// failure.
type commandOps[S any] struct{ c *Command[S] }

func (o commandOps[S]) execute(c *cycle, in *instance, n *node) error {
	if cmd := in.Cmd; cmd != nil {
		if cmd.Result != nil {
			return o.finish(c, in, n, cmd.Result)
		}
		// An intent already launched detached settles through the runner,
		// whatever this process's mode: rerunning it inline while its runner
		// lives would run the command twice. The mode decides new launches only.
		if c.e.opts.EffectRunner != nil && c.resultDir() != "" {
			if cmd.Started != nil {
				return o.poll(c, in, n)
			}
			if cmd.Max > 0 {
				return o.resume(c, in, n)
			}
		}
		return o.recover(c, in, n, nil, "")
	}
	met, ok, err := c.doneWhenMet(in, n)
	if err != nil || !ok {
		return err
	}
	if met {
		return o.completeByObservation(c, in, n)
	}
	argv, err := c.scope(in).bindArgv(o.c.k.argv)
	if err != nil {
		return c.failErr(in, n, err)
	}
	dir, err := o.dir(c, in)
	if err != nil {
		return c.failErr(in, n, err)
	}
	in0 := map[string]any{"argv": argv, "dir": dir}
	if o.c.k.stdin != "" {
		if in0["stdin"], err = c.scope(in).bindString(o.c.k.stdin); err != nil {
			return c.failErr(in, n, err)
		}
	}
	args, err := genericArgs(in0)
	if err != nil {
		return c.failErr(in, n, err)
	}
	if err := c.checkForkValue(args, "command"); err != nil {
		return c.failErr(in, n, err)
	}
	eff := EffectData{Kind: "external", Adapter: "shell", Op: "run", Args: args}
	if parts := o.c.k.key; len(parts) > 0 {
		bound := make([]string, len(parts))
		for i, p := range parts {
			if bound[i], err = c.scope(in).bindString(p); err != nil {
				return c.failErr(in, n, err)
			}
		}
		eff.Key = strings.Join(bound, "/")
	}
	intent := EffectIntentData{Command: true, Effect: eff}
	if o.detachedOn(c) {
		intent.Max, intent.Mode = o.c.k.detached, c.runMode()
	}
	if err := c.emit(in, JournalEffectIntent, 0, intent); err != nil {
		return err
	}
	return o.launch(c, in, c.recs[len(c.recs)-1].Seq, eff, false)
}

// detachedNote is what a command's open detached effect told this cycle: wait
// is true while Next shows a wait instruction (why is its prompt); otherwise
// why is the visible reason the manual verification adds.
type detachedNote struct {
	wait bool
	why  string
}

func (c *cycle) noteDetached(in *instance, n detachedNote) {
	if c.detached == nil {
		c.detached = map[string]detachedNote{}
	}
	c.detached[in.ID] = n
}

// detachedOn reports whether the command runs under the detached runner: it is
// declared .Detached(), a runner is configured and the run has a directory for
// result files. Otherwise it runs inline.
// A run whose host was DetachNone when it started, or that stepped down to it,
// runs them inline too: the journaled mode, not this process's, so every
// command of one run agrees.
func (o commandOps[S]) detachedOn(c *cycle) bool {
	if c.runMode() == DetachNone {
		return false
	}
	return o.c.k.detached > 0 && c.e.opts.EffectRunner != nil && c.resultDir() != ""
}

// inlineProgress writes a start line for intent and an elapsed line every
// every until the returned stop is called, so the caller's command keeps
// printing while it blocks (G3).
func inlineProgress(w io.Writer, every time.Duration, intent int64, eff EffectData, now func() time.Time) (stop func()) {
	var args struct{ Argv []string }
	raw, _ := json.Marshal(eff.Args)
	_ = json.Unmarshal(raw, &args)
	what := strings.Join(args.Argv, " ")
	if len(what) > 80 {
		what = what[:80] + "…"
	}
	start := now()
	fmt.Fprintf(w, "effect %d running inline (detach: none): %s\n", intent, what)
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Fprintf(w, "effect %d still running inline: %s elapsed\n", intent, now().Sub(start).Round(time.Second))
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		fmt.Fprintf(w, "effect %d finished inline after %s\n", intent, now().Sub(start).Round(time.Second))
	}
}

// withTimeout is eff with its shell.run bounded by max (args "Timeout").
func withTimeout(eff EffectData, max time.Duration) EffectData {
	args := make(map[string]any, len(eff.Args)+1)
	for k, v := range eff.Args {
		args[k] = v
	}
	args["Timeout"] = max.String()
	eff.Args = args
	return eff
}

// runDetach is the detach mode the run journaled at start, else (a run from
// before G1) this process's, lowered by any step-down the run journaled.
func (c *cycle) runDetach() *DetachFact {
	var f *DetachFact
	if c.st.started != nil {
		f = c.st.started.Detach
	}
	if f == nil {
		f = c.e.detachFact()
	}
	if f != nil && lowerMode(f.Mode, c.st.detach) != f.Mode {
		g := *f
		g.Mode = c.st.detach
		return &g
	}
	return f
}

// runMode is runDetach's mode, "" when the runner probes none.
func (c *cycle) runMode() string {
	if f := c.runDetach(); f != nil {
		return f.Mode
	}
	return ""
}

// modeOf is the detach mode intent cmd launches on: its own, else (an intent
// from before the ladder) the one the run journaled at start.
func (c *cycle) modeOf(cmd *cmdIntent) string {
	if cmd.Mode != "" {
		return cmd.Mode
	}
	if c.st.started != nil && c.st.started.Detach != nil {
		return c.st.started.Detach.Mode
	}
	return c.runMode()
}

// detachFact is the effect runner's detach mode, nil when it probes none.
func (e *Engine) detachFact() *DetachFact {
	p, ok := e.opts.EffectRunner.(DetachProber)
	if !ok {
		return nil
	}
	f := p.DetachMode()
	return &f
}

// preflightDegraded is the degraded record of a run whose probe f picked a
// detach mode below systemd, the best one: journaled once, after run_started,
// under the root definition's name, so status and the client's friction show
// that detached commands start a rung down. nil when f is nil, systemd, or a
// mode the user set (a choice, not a degradation). A crash between
// run_started and this record loses it: an accepted gap, as in relaunch.
func preflightDegraded(root string, f *DetachFact) *DegradedData {
	if f == nil || (f.Mode != DetachProcess && f.Mode != DetachNone) || strings.HasPrefix(f.Systemd, DetachSetBy) {
		return nil
	}
	cause := f.Systemd
	switch {
	case f.Mode == DetachNone && f.Pid1 != "":
		cause = "per-command sandbox (pid1 " + f.Pid1 + ")"
	case cause == "":
		cause = "detach probe"
	}
	return &DegradedData{Unit: root, Kind: DegradedDetach, Cause: cause, Action: "detach " + f.Mode + " at preflight"}
}

// resultDir is the run's directory for detached result files, "" when none.
func (c *cycle) resultDir() string {
	if c.e.opts.RunDir == nil {
		return ""
	}
	return c.e.opts.RunDir(c.run)
}

// effectPaths is the detached launch of intent with its result, proc and log
// paths (Argv and Dir are the caller's).
func (c *cycle) effectPaths(intent int64) DetachedRequest {
	base := filepath.Join(c.resultDir(), "effects", strconv.FormatInt(intent, 10))
	return DetachedRequest{ResultPath: base + ".json", ProcPath: base + ".proc", LogPath: base + ".log"}
}

func processOf(s *EffectStartedData) DetachedProcess {
	return DetachedProcess{Pid: s.Pid, Pgid: s.Pgid, StartTicks: s.StartTicks, Start: s.Start}
}

// startGrace is how long after its effect_intent a detached intent with neither
// a proc record nor a result file may still be starting: a launch whose proc
// write is delayed is not launched again inside it.
const startGrace = 10 * time.Second

// runnerFailedExit is the exit code of a step whose detached runner died with
// no mode left to step down to (or, under a runner that probes no mode, three
// times), as timeout(1) reports its own failure.
const runnerFailedExit = 125

// timedOutExit is the exit code of a detached command killed at its maximum
// duration, as timeout(1) reports it.
const timedOutExit = 124

// launch applies the command effect of intent: inline, or under the detached
// runner on the intent's mode. The launch of a re-run intent first waits until
// the dead launch's process group is empty (a live group is terminated and
// checked again on the next pass), but never past the intent's maximum
// duration (an inline re-run, which carries none, by the command's). A Start
// that launched nothing steps down one mode at once; a systemd-run that timed
// out is left to resume after the start grace.
func (o commandOps[S]) launch(c *cycle, in *instance, intent int64, eff EffectData, reapply bool) error {
	runner := c.e.opts.EffectRunner
	if r := in.Cmd.Rerun; r != nil && in.Cmd.Seq == intent && runner != nil {
		p := DetachedProcess{Pid: r.Pid, Pgid: r.Pgid, StartTicks: r.StartTicks}
		if p.Pgid == 0 {
			// A stepped-down Start failure: a timed-out systemd-run's unit may
			// have started late, and then its proc record names its group.
			lp, ok, err := runner.Launched(c.effectPaths(r.Of))
			if err != nil {
				c.markTransient(in, err)
				return nil
			}
			if ok {
				p = lp
			}
		}
		if p.Pgid > 0 {
			empty, err := runner.GroupEmpty(p, c.effectPaths(r.Of))
			if err != nil {
				c.markTransient(in, err)
				return nil
			}
			start, max := in.Cmd.detachedClock()
			if max <= 0 {
				start, max = in.Cmd.At, o.c.k.detached
			}
			if !empty && (max <= 0 || c.now().Before(start.Add(max))) {
				if err := runner.Terminate(p, c.effectPaths(r.Of)); err != nil {
					c.markTransient(in, err)
					return nil
				}
				c.noteDetached(in, detachedNote{wait: true, why: fmt.Sprintf("Waiting for process group %d of dead launch %d to end before the re-run; call next again after the retry-after.", p.Pgid, r.Of)})
				return nil
			}
		}
	}
	// Resolve into a fresh map only at the operational boundary. The journal
	// and in.Cmd.Effect retain their typed argv for future recovery or forks.
	argsValue, err := c.resolvePathValues(eff.Args)
	if err != nil {
		return c.failErr(in, in.node(), err)
	}
	eff.Args = argsValue.(map[string]any)
	if reapply {
		eff = c.relocateCommandEffect(intent, eff)
	}
	// Every launch, first or recovered, inline or detached, checks the
	// working directory first: a missing one is unavailable, named, where the
	// host would report the program as not found (fork/exec <argv0>: no such
	// file or directory).
	cwd, _ := eff.Args["dir"].(string)
	if err := c.e.reg.checkCommandDirectory(c.workspace(), cwd); err != nil {
		return c.emit(in, JournalEffectResult, 0, EffectResultData{Intent: intent, OK: false, Outcome: "unavailable", Error: err.Error()})
	}
	if !o.detachedOn(c) {
		if o.c.k.detached > 0 && c.e.opts.Progress != nil {
			defer inlineProgress(c.e.opts.Progress, c.e.opts.ProgressEvery, intent, eff, c.now)()
		}
		if o.c.k.detached > 0 {
			// Inline, a .Detached(max) command is bounded by its max, not by the
			// runtime's default timeout, and its expiry is a timed-out exit as on
			// the detached path.
			eff = withTimeout(eff, o.c.k.detached)
		}
		return c.applyEffect(in, intent, 0, eff)
	}
	var args struct {
		Argv []string
		Dir  string
	}
	raw, _ := json.Marshal(eff.Args)
	if err := json.Unmarshal(raw, &args); err != nil {
		return c.fail(in, in.node(), Blocked, []string{err.Error()}, "")
	}
	// The request's Dir is absolute: the runner's own working directory is not
	// the run's workspace, and the inline runtime resolves the same way.
	dir := args.Dir
	if dir == "" || dir == "." {
		dir = c.workspace()
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(c.workspace(), dir)
	}
	req := c.effectPaths(intent)
	mode := c.modeOf(in.Cmd)
	req.Argv, req.Dir, req.Mode = args.Argv, dir, mode
	req.Context = EffectContext{Run: c.run, Instance: in.ID, State: c.st.qualified(in), Sandboxed: c.st.forked, SandboxBin: c.sandboxBin()}
	p, err := runner.Start(req)
	switch {
	case err != nil && errors.Is(err, ErrNotLaunched) && stepBelow(mode) != "":
		return o.relaunch(c, in, &EffectRerun{Of: intent, Reason: err.Error()}, lowerMode(stepBelow(mode), c.runMode()))
	case err != nil && mode == DetachSystemd && errors.Is(err, ErrUnavailable):
		// A systemd-run that timed out may still start its unit. Nothing is
		// journaled: past the start grace resume decides from this intent's own
		// proc record, adopting a late unit or stepping down when none ran.
		c.noteDetached(in, detachedNote{wait: true, why: fmt.Sprintf("Detached effect %d may still be starting; call next again after the retry-after.", intent)})
		return nil
	case err != nil:
		return c.emit(in, JournalEffectResult, 0, EffectResultData{Intent: intent, OK: false, Outcome: "unavailable", Error: err.Error()})
	}
	return o.started(c, in, intent, p)
}

// relocateCommandEffect handles pre-marker journals only after the fold has
// identified the intent as a Command, never an arbitrary shell edge effect.
func (c *cycle) relocateCommandEffect(intent int64, eff EffectData) EffectData {
	var saved EffectIntentData
	if intent > 0 && intent <= int64(len(c.recs)) {
		_ = json.Unmarshal(c.recs[intent-1].Data, &saved)
	}
	if !saved.Command && len(c.recs) > 0 && c.recs[0].Roots != nil && c.e.opts.StateRoot != "" {
		dir, _ := eff.Args["dir"].(string)
		eff.Args = maps.Clone(eff.Args)
		eff.Args["dir"] = relocateString(dir, *c.recs[0].Roots, PathRoots{Workspace: c.workspace(), State: c.e.opts.StateRoot})
	}
	return eff
}

// started journals the effect_started of intent's launch p and waits for it.
func (o commandOps[S]) started(c *cycle, in *instance, intent int64, p DetachedProcess) error {
	if err := c.emit(in, JournalEffectStarted, 0, EffectStartedData{Intent: intent, Pid: p.Pid, Pgid: p.Pgid, StartTicks: p.StartTicks, Max: o.c.k.detached}); err != nil {
		return err
	}
	c.noteDetached(in, detachedNote{wait: true, why: waitPrompt(intent, p.Pid)})
	return nil
}

func waitPrompt(intent int64, pid int) string {
	return fmt.Sprintf("Waiting for detached effect %d (pid %d); call next again after the retry-after.", intent, pid)
}

// fileResult journals the result file's RunResult as the effect's result.
func (c *cycle) fileResult(in *instance, intent int64, rr RunResult) error {
	raw, err := json.Marshal(rr)
	if err != nil {
		return err
	}
	return c.emit(in, JournalEffectResult, 0, EffectResultData{Intent: intent, OK: true, Outcome: "applied", Result: raw})
}

// poll takes the open detached effect's status, once per cycle: the result
// file wins over everything (even a due maximum duration); a live process
// within its maximum (measured from the intent's At) is waited for; past it the
// process group is terminated and a timed-out result is journaled; a dead one
// goes to dead.
func (o commandOps[S]) poll(c *cycle, in *instance, n *node) error {
	if _, ok := c.detached[in.ID]; ok {
		return nil
	}
	s := in.Cmd.Started
	p := processOf(s)
	runner := c.e.opts.EffectRunner
	rep, err := runner.Status(p, c.effectPaths(s.Intent))
	if err != nil {
		c.markTransient(in, err)
		return nil
	}
	switch rep.Status {
	case DetachedPresent:
		return c.fileResult(in, s.Intent, rep.Result)
	case DetachedRunning:
		start, max := in.Cmd.detachedClock()
		if max <= 0 || c.now().Before(start.Add(max)) {
			c.noteDetached(in, detachedNote{wait: true, why: waitPrompt(s.Intent, s.Pid)})
			return nil
		}
		if err := runner.Terminate(p, c.effectPaths(s.Intent)); err != nil {
			c.markTransient(in, err)
			return nil
		}
		raw, err := json.Marshal(RunResult{Exit: timedOutExit, Stderr: "timed out after " + max.String()})
		if err != nil {
			return err
		}
		return c.emit(in, JournalEffectResult, 0, EffectResultData{Intent: s.Intent, OK: true, Outcome: "timed out", Result: raw})
	case DetachedDead, DetachedDeadLive:
		return o.dead(c, in, n, p, rep.Reason)
	}
	return fmt.Errorf("effect runner returned unknown status %d", rep.Status)
}

// resume handles a detached intent with no effect_started (the launching call
// died, or a re-run is waiting for its dead launch's group). The proc record
// says whether a launch happened: a result file wins, a running launch gets its
// effect_started back, a dead one goes to dead. With neither proc record nor
// result file the intent was not launched, or not yet: inside the start grace
// (measured from the intent's own At, a re-run's included, so a CLI lost
// mid-launch never gets a second runner on the same paths) it is waited for;
// past it (and, for a re-run, once the dead group is empty or the maximum has
// passed) it is launched under the same intent, whose paths are unused. Past
// it, a run that has stepped down to none recovers the intent instead (an
// inline run leaves no proc record to resume from), and a systemd intent steps
// down: its systemd-run timed out, or its call died, with no unit started.
func (o commandOps[S]) resume(c *cycle, in *instance, n *node) error {
	if _, ok := c.detached[in.ID]; ok {
		return nil
	}
	runner := c.e.opts.EffectRunner
	cmd := in.Cmd
	req := c.effectPaths(cmd.Seq)
	if done, err := o.adopt(c, in, n, req); done {
		return err
	}
	if c.now().Before(cmd.At.Add(startGrace)) {
		c.noteDetached(in, detachedNote{wait: true, why: fmt.Sprintf("Detached effect %d may still be starting; call next again after the retry-after.", cmd.Seq)})
		return nil
	}
	if !o.detachedOn(c) {
		return o.recover(c, in, n, nil, "")
	}
	if cmd.Mode == DetachSystemd {
		// Fence the intent's paths before stepping down, then look once more: a
		// unit that wrote its proc record first is this launch, adopted with
		// the fence withdrawn (the runner's Terminate does that); one that
		// starts later finds the stop request and exits before its command.
		if err := runner.Terminate(DetachedProcess{}, req); err != nil {
			c.markTransient(in, err)
			return nil
		}
		if done, err := o.adopt(c, in, n, req); done {
			return err
		}
		why := fmt.Sprintf("systemd launch of effect %d wrote no proc record within the %v start grace", cmd.Seq, startGrace)
		return o.relaunch(c, in, &EffectRerun{Of: cmd.Seq, Reason: why}, lowerMode(DetachProcess, c.runMode()))
	}
	return o.launch(c, in, cmd.Seq, cmd.Effect, true)
}

// adopt settles the open intent from its paths req when they show a launch: a
// result file wins, a running launch gets its effect_started, a dead one goes
// to dead. done is false when there is neither proc record nor result file; a
// runner error is transient (done, nothing journaled).
func (o commandOps[S]) adopt(c *cycle, in *instance, n *node, req DetachedRequest) (done bool, err error) {
	runner := c.e.opts.EffectRunner
	p, launched, err := runner.Launched(req)
	var rep DetachedReport
	if err == nil {
		rep, err = runner.Status(p, req)
	}
	switch {
	case err != nil:
		c.markTransient(in, err)
		return true, nil
	case rep.Status == DetachedPresent:
		return true, c.fileResult(in, in.Cmd.Seq, rep.Result)
	case rep.Status == DetachedRunning && launched:
		return true, o.started(c, in, in.Cmd.Seq, p)
	case launched:
		return true, o.dead(c, in, n, p, rep.Reason)
	}
	return false, nil
}

// dead handles a detached launch that died with no result file (never a timeout:
// its group was terminated, or is waited for, by the re-run). Recovery decides,
// and a re-run is a fresh effect_intent one detach mode down: never on the mode
// that just failed. A dead launch with no mode below it (under a runner that
// probes no mode: the third dead launch of a step attempt) settles a
// runner-failed result carrying its reason, which routes like any non-zero exit.
func (o commandOps[S]) dead(c *cycle, in *instance, n *node, p DetachedProcess, reason string) error {
	if reason == "" {
		reason = fmt.Sprintf("detached runner pid %d died with no result file", p.Pid)
	}
	c.noteDetached(in, detachedNote{why: reason})
	rerun := &EffectRerun{Of: in.Cmd.Seq, Pid: p.Pid, Pgid: p.Pgid, StartTicks: p.StartTicks, Reason: reason}
	from := c.modeOf(in.Cmd)
	switch next := stepBelow(from); {
	case next != "":
		return o.recover(c, in, n, rerun, lowerMode(next, c.runMode()))
	case from == "" && in.Cmd.Dead < 2:
		return o.recover(c, in, n, rerun, "")
	}
	if err := c.e.opts.EffectRunner.Terminate(p, c.effectPaths(in.Cmd.Seq)); err != nil {
		c.markTransient(in, err)
		return nil
	}
	raw, err := json.Marshal(RunResult{Exit: runnerFailedExit, Stderr: reason})
	if err != nil {
		return err
	}
	return c.emit(in, JournalEffectResult, 0, EffectResultData{Intent: in.Cmd.Seq, OK: true, Outcome: "runner failed", Result: raw})
}

// dir is where the command runs: its own In directory, else the bound {workdir},
// else "" (the workspace root).
func (o commandOps[S]) dir(c *cycle, in *instance) (string, error) {
	if d := o.c.k.dir; d != "" {
		return c.scope(in).bindString(d)
	}
	w, _ := c.workdirBound(in)
	return w, nil
}

// recover handles a command intent with no result, in order: DoneWhen is
// re-checked (met: the command completed by observation); an Idempotent command
// is applied again; a keyed one is asked through the runtime's Check (done:
// journaled as an exit-0 result, not done: applied again, unknown: Uncertain).
// A Check error is transient: nothing is journaled and the check is repeated by
// the next drive. dead, when set, is the detached launch whose death sent the
// effect here: its reason is shown by the manual verification, and a re-run is a
// fresh effect_intent naming it, on detach mode mode. Otherwise the effect is
// Uncertain and never re-applied. The probe is not journaled unless it is met,
// so a held Uncertain command appends nothing.
func (o commandOps[S]) recover(c *cycle, in *instance, n *node, dead *EffectRerun, mode string) error {
	if n.doneWhen != nil {
		d, err := c.scope(in).bindDesc(n.doneWhen.obs)
		if err != nil {
			return c.failErr(in, n, err)
		}
		od, err := c.readObs(in, d, "done_when", false)
		if err != nil {
			return err
		}
		if od.Error == "" && !od.Unavailable && truthy(od.Value) {
			if err := c.emitObserved(in, &od); err != nil {
				return err
			}
			return o.completeByObservation(c, in, n)
		}
	}
	k := o.c.k
	switch {
	case k.idempotent:
		return o.rerun(c, in, dead, mode)
	case len(k.key) > 0:
		eff := c.relocateCommandEffect(in.Cmd.Seq, in.Cmd.Effect)
		args, err := c.resolvePathValues(eff.Args)
		if err != nil {
			return c.failErr(in, n, err)
		}
		st, err := c.e.reg.Check(c.workspace(), ExternalEffect{Adapter: "shell", Op: "run", Args: args.(map[string]any), Key: keyOfData(eff)})
		switch {
		case err != nil:
			c.markTransient(in, err)
			return nil
		case st == EffectDone:
			res, _ := json.Marshal(RunResult{})
			return c.emit(in, JournalEffectResult, 0, EffectResultData{Intent: in.Cmd.Seq, OK: true, Outcome: "done", Result: res})
		case st == EffectNotDone:
			return o.rerun(c, in, dead, mode)
		}
	}
	why := "command has an intent with no result and its DoneWhen is not met"
	if dead != nil {
		why = dead.Reason
	}
	return c.uncertain(in, why)
}

// rerun applies the open command effect again. After a dead detached launch
// that is a fresh effect_intent on mode (see relaunch); otherwise the same
// intent.
func (o commandOps[S]) rerun(c *cycle, in *instance, dead *EffectRerun, mode string) error {
	if dead == nil {
		return o.launch(c, in, in.Cmd.Seq, in.Cmd.Effect, true)
	}
	return o.relaunch(c, in, dead, mode)
}

// relaunch journals a fresh effect_intent (new seq, so new result, proc and log
// paths) on detach mode mode, naming the launch dead it re-runs, and launches
// it. A mode below the dead launch's is a step-down, journaled next as a
// degraded record of kind detach. A crash between the intent and that record
// loses the record (the step-down itself holds, from the intent's mode): an
// accepted gap, the price of never journaling one step-down twice.
func (o commandOps[S]) relaunch(c *cycle, in *instance, dead *EffectRerun, mode string) error {
	from := c.modeOf(in.Cmd)
	eff := c.relocateCommandEffect(in.Cmd.Seq, in.Cmd.Effect)
	intent := EffectIntentData{Command: true, Effect: eff, Rerun: dead, Mode: mode}
	if mode != DetachNone {
		intent.Max = o.c.k.detached
	}
	if err := c.emit(in, JournalEffectIntent, 0, intent); err != nil {
		return err
	}
	seq := c.recs[len(c.recs)-1].Seq
	if mode != from {
		d := DegradedData{Unit: c.st.qualified(in), Kind: DegradedDetach, Cause: lastLine(dead.Reason), Action: "stepped down " + from + " -> " + mode}
		if err := c.emit(in, JournalDegraded, 0, d); err != nil {
			return err
		}
	}
	return o.launch(c, in, seq, eff, true)
}

// lastLine is the last non-empty line of s: the latest of a log tail's lines.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// completeByObservation routes a command whose postcondition holds as a run
// with exit 0 and journals completed_by_observation.
func (o commandOps[S]) completeByObservation(c *cycle, in *instance, n *node) error {
	raw, plan, ok, err := o.routeResult(c, in, n, RunResult{})
	if err != nil || !ok {
		return err
	}
	return c.emit(in, JournalCompletedByObservation, Accepted, CompletedByObservationData{Output: raw, Plan: plan})
}

// finish takes the journaled result of the command's effect: a failed apply is a
// failed attempt (Unavailable, else Refused); otherwise the RunResult is
// routed.
func (o commandOps[S]) finish(c *cycle, in *instance, n *node, res *EffectResultData) error {
	if !res.OK {
		class := Refused
		switch {
		case res.Outcome == "unavailable":
			class = Unavailable
		case retryOf(n) == nil:
			class = Blocked // no Retry: nothing may spend an attempt, so the step halts
		}
		return c.fail(in, n, class, []string{res.Error}, res.Error)
	}
	var rr RunResult
	if err := json.Unmarshal(res.Result, &rr); err != nil {
		return c.fail(in, n, Refused, []string{"command result is not a RunResult: " + err.Error()}, "")
	}
	raw, plan, ok, err := o.routeResult(c, in, n, rr)
	if err != nil || !ok {
		return err
	}
	return c.emit(in, JournalAccepted, Accepted, AcceptedData{Output: raw, Computed: true, Plan: plan})
}

// routeResult chooses the edge for rr and plans it. ok is false when a failure
// was journaled instead.
func (o commandOps[S]) routeResult(c *cycle, in *instance, n *node, rr RunResult) (raw json.RawMessage, plan EdgePlan, ok bool, err error) {
	raw, err = json.Marshal(rr)
	if err != nil {
		return nil, plan, false, c.fail(in, n, Refused, []string{"command result is not JSON: " + err.Error()}, "")
	}
	ctx, err := c.ctx(in)
	if err != nil {
		return nil, plan, false, c.failErr(in, n, err)
	}
	var fn func(Ctx) edgeUse
	if o.c.route != nil {
		fn = func(x Ctx) edgeUse { return o.c.route(rr, x).use() }
	}
	u, ok, err := c.route(in, n, fn, any(rr), ctx)
	if err != nil || !ok {
		return nil, plan, false, err
	}
	if plan, err = c.planOf(in, u, raw); err != nil {
		return nil, plan, false, c.halt(in, err.Error())
	}
	return raw, plan, true, nil
}
