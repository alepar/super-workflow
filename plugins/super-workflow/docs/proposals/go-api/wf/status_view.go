package wf

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// statusOrder is the order of the per-state counts on the totals line.
var statusOrder = []UnitState{UnitStalled, UnitRunning, UnitStarted, UnitHanded, UnitHeld, UnitPending, UnitWaiting,
	UnitDone, UnitFailed, UnitLost, UnitWithdrawn}

// RenderStatus is the default `wf status` view: one line per subagent unit, in
// the order of rs.Units, and a totals line. It is a pure function of rs (rs.Now
// is the clock sample) and renders from the unit state with one switch; no
// caller parses its output, they read `status --json`.
func RenderStatus(rs RunStatus) string {
	var b strings.Builder
	nameW, stateW := 0, 0
	for _, u := range rs.Units {
		nameW, stateW = max(nameW, len(u.Name)), max(stateW, len(u.State))
	}
	for _, u := range rs.Units {
		parts := []string{statCell(u)}
		if u.PromptCoverage != "" {
			parts = append(parts, u.PromptCoverage)
		}
		// The note and the observation are model text: escaped like a stderr
		// tail, so they stay on the unit's line.
		if u.Note != "" {
			parts = append(parts, escapeControl(u.Note))
		}
		if u.Observed != "" {
			parts = append(parts, "observed: "+escapeControl(u.Observed))
		}
		if u.Session != "" {
			parts = append(parts, "session "+u.Session)
		}
		if u.Context != "" {
			parts = append(parts, "context "+u.Context)
		}
		if u.Harness != "" {
			h := "on " + u.Harness
			if u.Pid != 0 {
				h += fmt.Sprintf(" (pid %d)", u.Pid)
			}
			parts = append(parts, h)
		}
		if u.StartErr != "" {
			parts = append(parts, "did not start: "+escapeControl(u.StartErr))
		}
		fmt.Fprintf(&b, "%-*s  %-*s  %s\n", nameW, u.Name, stateW, u.State, strings.Join(parts, "  "))
	}

	for _, in := range rs.Instances {
		name := in.ID
		if name == "" {
			name = "run"
		}
		if in.Reason != "" {
			fmt.Fprintf(&b, "instance %s %s at %s: %s\n", name, in.Phase, in.State, escapeControl(in.Reason))
		}
		if r := in.Returned; r != nil && in.Phase != phaseDone {
			fmt.Fprintf(&b, "instance %s at %s: returned by %s (%q, round %d of %d), %d reason(s)\n", name, in.State, r.From, r.Label, r.Round, r.Of, len(r.Reasons))
		}
	}
	if turn := rs.CoordinatorTurn; turn != nil {
		fmt.Fprintf(&b, "automatic continuations: %d\n", turn.Continuations)
		if turn.StallSeq != 0 {
			fmt.Fprintf(&b, "coordinator continuation stalled at journal seq %d; an operator can stop and resume the run\n", turn.StallSeq)
		}
	}
	if bpt := rs.Breakpoint; bpt != nil {
		fmt.Fprintf(&b, "%s\n", bpt.summary())
	}
	if rs.BudgetStop != "" {
		fmt.Fprintf(&b, "stopped on its run budget, not ended: %s\nits operator raises the budget with:\n  %s\n", rs.BudgetStop, rs.BudgetResume)
	}
	for _, ch := range rs.Children {
		if ch.Available {
			fmt.Fprintf(&b, "children on %s: available (%s)\n", ch.Harness, escapeControl(ch.Version))
		} else {
			fmt.Fprintf(&b, "children on %s: unavailable: %s; fix: %s\n", ch.Harness, escapeControl(ch.Why), escapeControl(ch.Fix))
		}
	}
	b.WriteString(StatusTotals(rs) + "\n")
	if rs.Phase == RunEnded {
		fmt.Fprintf(&b, "ended: terminal %s, outcome %s\n", rs.Terminal, rs.Outcome)
		if rs.Cause != "" {
			fmt.Fprintf(&b, "cause: %s\n", rs.Cause)
		}
		if rs.Resume != "" {
			fmt.Fprintf(&b, "resume, to continue this run later:\n  %s\n", rs.Resume)
		}
	}
	b.WriteString(harnessLine(rs) + "\n")
	b.WriteString(buildLine(rs))
	if line := modelsLine(rs.Models, segmentHarnessOf(rs)); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString(unitCountsLine(rs.UnitCounts) + "\n")
	if line := segmentsLine(rs.Segments); line != "" {
		b.WriteString(line + "\n")
	}
	if rs.Tokens > 0 {
		fmt.Fprintf(&b, "tokens: %d\n", rs.Tokens)
	}
	for _, r := range rs.Recoveries {
		answers := []string{}
		for _, k := range []string{RecoveryResume, RecoveryRestart, RecoveryGiveUp} {
			if cmd, ok := r.Commands[k]; ok {
				answers = append(answers, cmd)
			}
		}
		fmt.Fprintf(&b, "recovery: %s: %s; answer with one of:\n", r.Unit, r.Cause)
		for _, cmd := range answers {
			fmt.Fprintf(&b, "  %s\n", cmd)
		}
	}
	for _, d := range rs.Degraded {
		fmt.Fprintf(&b, "degraded: %s %s: %q: %q\n", d.Unit, d.Kind, d.Cause, d.Action)
	}
	for _, h := range rs.Held {
		if h.ApprovalAction != "" {
			fmt.Fprintf(&b, "held: %s needs approval. Reported action: %q\n", h.Name, h.ApprovalAction)
		}
	}
	for _, d := range rs.Due {
		who := d.Unit
		if who == "" {
			who = d.Instance
		}
		if who == "" {
			who = "run"
		}
		fmt.Fprintf(&b, "due: %s %s %s\n", d.Kind, who, d.Due.UTC().Format(time.RFC3339))
	}
	for _, cleanup := range rs.PendingCleanups {
		fmt.Fprintf(&b, "pending cleanup %d: %s (workspace still in use or removal needs retry)\n", cleanup.Deferred, cleanup.Effect)
	}
	for _, o := range rs.OpenIntents {
		fmt.Fprintf(&b, "open effect %d %s%s\n", o.Seq, o.Effect, o.detachedNote())
	}
	if h := rs.LockHolder; h != nil {
		fmt.Fprintf(&b, "lock: last held by pid %d since %s", h.Pid, h.Since.UTC().Format(time.RFC3339))
		if !h.Released.IsZero() {
			fmt.Fprintf(&b, ", released at %s", h.Released.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
	}
	if rs.NothingReadyReason != "" {
		fmt.Fprintf(&b, "nothing ready: %s\n", rs.NothingReadyReason)
	}
	for i, f := range rs.Fix {
		what := "fix:"
		if i < len(rs.FixNotes) && rs.FixNotes[i] != "" {
			what = "fix, " + rs.FixNotes[i] + ":"
		}
		fmt.Fprintf(&b, "%s\n  %s\n", what, f)
	}
	if len(rs.Delegations) > 0 {
		ds := make([]string, 0, len(rs.Delegations))
		for g, v := range rs.Delegations {
			ds = append(ds, g+"="+v)
		}
		sort.Strings(ds)
		b.WriteString("delegated: " + strings.Join(ds, ", ") + "\n")
	}
	return b.String()
}

// unitCountsLine is the counts a coordinator acts on: the units ready to hand
// out, handed and not yet claimed, and running.
func unitCountsLine(n UnitCounts) string {
	return fmt.Sprintf("units: %d ready, %d handed, %d running", n.Ready, n.Handed, n.Running)
}

// StatusTotals is the totals line of the status view: the subagent count, the
// count per state, elapsed, ETA and the binding constraint.
func StatusTotals(rs RunStatus) string {
	counts := map[string]int{}
	total := 0
	for _, u := range rs.Units {
		if u.State != "future" {
			counts[u.State]++
			total++
		}
	}
	ge := ""
	if rs.Progress.TotalIsLowerBound {
		ge = "≥ "
	}
	var cs []string
	for _, s := range statusOrder {
		if n := counts[s.String()]; n > 0 {
			cs = append(cs, fmt.Sprintf("%d %s", n, s))
		}
	}
	head := fmt.Sprintf("subagents %s%d", ge, total)
	if len(cs) > 0 {
		head += ": " + strings.Join(cs, ", ")
	}
	if rs.Future > 0 {
		head += fmt.Sprintf(" (+%d future steps not counted)", rs.Future)
	}
	segs := []string{head}
	if !rs.Started.IsZero() {
		segs = append(segs, "elapsed "+fmtHM(rs.Now.Sub(rs.Started)))
	}
	if rs.ETAKnown {
		segs = append(segs, "eta ~"+fmtHM(rs.ETA))
	} else {
		segs = append(segs, "eta --")
	}
	if rs.Binding != BindingNone {
		segs = append(segs, string(rs.Binding))
	}
	return strings.Join(segs, " · ")
}

// statCell is the unit's stats column, per the status table of API section 4.1.
func statCell(u UnitStatus) string {
	switch u.State {
	case UnitRunning.String():
		switch {
		case u.Estimate <= 0:
			return fmtMS(u.Elapsed)
		case u.Remaining < 0:
			return fmtMS(u.Elapsed) + "  over " + fmtMS(-u.Remaining)
		}
		return fmtMS(u.Elapsed) + "  eta ~" + fmtMS(u.Remaining)
	case UnitStalled.String():
		return fmtMS(u.Elapsed) + "  quiet " + fmtMS(u.Quiet)
	case UnitHanded.String():
		return fmtMS(u.Elapsed) + "  window " + fmtWindow(u.Window)
	case UnitStarted.String():
		return fmtMS(u.Elapsed)
	case UnitPending.String(), UnitWaiting.String(), "future":
		if u.Estimate > 0 {
			return "est ~" + fmtMS(u.Estimate)
		}
		return "--"
	default: // done, failed, lost, withdrawn
		if u.Elapsed > 0 {
			return fmtMS(u.Elapsed)
		}
		return "--"
	}
}

// fmtMS formats d as 2m10s.
func fmtMS(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// fmtWindow formats a window: 2m when whole minutes, else 2m30s.
func fmtWindow(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmtMS(d)
}

// fmtHM formats d as H:MM.
func fmtHM(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	return fmt.Sprintf("%d:%02d", m/60, m%60)
}

// detachedNote is what a detached open effect adds to its status line: the
// process group a re-run waits on, the maximum-duration due time and the dead
// launch's reason. Empty for an effect with none of them.
func (o OpenIntent) detachedNote() string {
	var parts []string
	due := ""
	if !o.Due.IsZero() {
		due = "max due " + o.Due.UTC().Format(time.RFC3339)
	}
	switch {
	case o.WaitGroup != 0 && due != "":
		parts = append(parts, fmt.Sprintf("re-run waits for process group %d to end (%s)", o.WaitGroup, due))
	case o.WaitGroup != 0:
		parts = append(parts, fmt.Sprintf("re-run waits for process group %d to end", o.WaitGroup))
	case due != "":
		parts = append(parts, due)
	}
	if o.Reason != "" {
		parts = append(parts, fmt.Sprintf("dead launch %d: %s", o.RerunOf, o.Reason))
	}
	if len(parts) == 0 {
		return ""
	}
	return " detached: " + strings.Join(parts, "; ")
}

// StatusForCoordinator is status's one line for a running run's coordinator
// (H3): what is in flight, and that next, not a poll, is its next move. It
// names the verb, not the whole command: the coordinator has its loop line,
// and a poll should cost a short line.
func StatusForCoordinator(st RunStatus) string {
	u := st.UnitCounts
	if u.Ready+u.Handed == 0 {
		return fmt.Sprintf("running: %d running, nothing for you to do; coordinator: run next; subagent: carry on", u.Running)
	}
	return fmt.Sprintf("running: %d running, %d handed out not started, %d ready; coordinator: run next; subagent: carry on", u.Running, u.Handed, u.Ready)
}

// segmentsLine is the status line of a run's harness segments,
// each bound one with its tier models:
// "harness segments: codex 1-512 (40 units, start), hermes 513- (7 units,
// resume)"; "" for a run with one segment that never changed hands.
func segmentsLine(ss []SegmentView) string {
	if len(ss) < 2 {
		return ""
	}
	parts := make([]string, len(ss))
	for i, s := range ss {
		h := s.Harness
		if h == "" {
			h = "unbound"
		}
		end := ""
		if s.End > 0 {
			end = fmt.Sprint(s.End)
		}
		models := ""
		if s.ModelsFrom != "" {
			models = ", " + tiersOf(s.Models)
			if len(s.Models) == 0 {
				models = ", no tiers"
			}
		}
		parts[i] = fmt.Sprintf("%s %d-%s (%d units, %s%s)", h, s.Start, end, s.Units, s.Cause, models)
	}
	return "harness segments: " + strings.Join(parts, ", ")
}

// RenderLock is wf lock's view: whether the run's lock is held now, as the
// kernel's lock table shows it, then its last writer's record. It never
// certifies the lock free: a table that lists no holder is "no visible
// holder", and one it cannot read is unknown. A record with no release stamp
// says only that none was recorded (its holder may have died, or released
// before stamps were written).
func RenderLock(st LockState) string {
	var b strings.Builder
	switch {
	case st.Held && st.Pid > 0:
		fmt.Fprintf(&b, "lock: held now by pid %d\n", st.Pid)
	case st.Held:
		b.WriteString("lock: held now by a process this host cannot name\n")
	case !st.Known:
		b.WriteString("lock: unknown: the kernel's lock table cannot be read\n")
	default:
		b.WriteString("lock: no visible holder (the kernel's lock table lists none; the engine's verbs still wait for any hold it does not show)\n")
	}
	switch h := st.Holder; {
	case h == nil:
		b.WriteString("no call has written to the run's lock record\n")
	case h.Released.IsZero():
		fmt.Fprintf(&b, "last written by %s; no release was recorded\n", h.describe())
	default:
		fmt.Fprintf(&b, "last written by %s\n", h.describe())
	}
	return b.String()
}

// buildLine is the run's build stamp, and a note when the binary reading the
// status is not the one the stamp names.
func buildLine(rs RunStatus) string {
	if rs.Build == nil {
		return "build: not recorded (the run predates build stamps)\n"
	}
	line := "build: " + rs.Build.Summary() + "\n"
	if rs.BuildDiffers != "" {
		line += "this binary differs: " + rs.BuildDiffers + "\n"
	}
	return line
}
