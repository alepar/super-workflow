package wf

import (
	"encoding/json"
	"strings"
	"time"
)

// Binding is what limits a run's progress.
type Binding string

// The bindings.
const (
	BindingNone  Binding = ""
	BindingGraph Binding = "graph-bound"      // most work waits on dependencies
	BindingHuman Binding = "waiting on human" // an open human gate and nothing running
)

// Progress counts settled and known subagent units.
type Progress struct {
	Done, Total       int
	TotalIsLowerBound bool // a source can still grow, or agent states are not yet entered
	WaitingOnDeps     int
}

// classStats is the per-class estimate model of a run, folded from the
// journal: EWMAs of attempt duration (attempt_started to the attempt's
// accepted, refused or late_vote) and of parallelism (the running units of the
// class at each attempt_started).
//
// Owns: Progress, ETA (never below the remaining critical path), Binding, the
// default stall threshold (3x the duration EWMA, else StallDefault) and each
// unit's remaining or estimated time.
// Hides: the smoothing constant and how the ETA is formed.
// Does not: read the wall clock (callers pass now; the Engine samples it) or
// the journal outside foldStats.
// Extend by: a new estimate is a method here; callers never compute an ETA.
type classStats struct {
	cfg     statsConfig
	classes map[string]*classStat
}

type classStat struct {
	dur, par float64 // EWMAs, nanoseconds and units
	samples  int     // duration samples; par is set from the first attempt_started
}

type statsConfig struct {
	StallDefault time.Duration // 20m
	Alpha        float64       // 0.3
}

// withDefaults fills the zero fields of c.
func (c statsConfig) withDefaults() statsConfig {
	if c.StallDefault <= 0 {
		c.StallDefault = 20 * time.Minute
	}
	if c.Alpha <= 0 || c.Alpha > 1 {
		c.Alpha = 0.3
	}
	return c
}

// unitView is what the estimates read of one unit; Status builds it.
type unitView struct {
	Name, Class string
	State       UnitState
	Since       time.Time // pending: issue; handed: hand-out; started+: start
	Ready       bool      // pending and ready to hand out
	Held        bool      // pending, held back by Parallel, a write fence or an entry lock: not a dependency wait
}

// running reports whether the unit holds a started agent.
func (u unitView) running() bool { return u.State.claimed() }

func ewma(old, x, alpha float64, first bool) float64 {
	if first {
		return x
	}
	return alpha*x + (1-alpha)*old
}

// foldStats folds the estimates from recs. st is the folded state the records
// produced: its name index resolves an attempt's unit name to a class (the
// qualified state name, as Instruction.Class).
func foldStats(recs []JournalRecord, st *runState, cfg statsConfig) *classStats {
	s := &classStats{cfg: cfg.withDefaults(), classes: map[string]*classStat{}}
	type open struct {
		name, class string
		at          time.Time
	}
	attempts := map[string]open{} // by instance; a unit has one open attempt
	running := map[string]int{}   // open attempts by class
	stat := func(class string) *classStat {
		cs := s.classes[class]
		if cs == nil {
			cs = &classStat{}
			s.classes[class] = cs
		}
		return cs
	}
	closeAttempt := func(inst string, r JournalRecord, sample bool) {
		o, ok := attempts[inst]
		if !ok {
			return
		}
		delete(attempts, inst)
		running[o.class]--
		if sample && !o.at.IsZero() && !r.At.IsZero() && !r.At.Before(o.at) {
			cs := stat(o.class)
			cs.dur = ewma(cs.dur, float64(r.At.Sub(o.at)), s.cfg.Alpha, cs.samples == 0)
			cs.samples++
		}
	}
	for _, r := range recs {
		switch r.Kind {
		case JournalAttemptStarted:
			var d struct{ Name string }
			if json.Unmarshal(r.Data, &d) != nil {
				continue
			}
			if o, ok := attempts[r.Instance]; ok && o.name == d.Name {
				continue // a same-agent re-claim keeps its attempt
			}
			ref, ok := st.names[d.Name]
			if !ok {
				continue
			}
			class := st.classOf(ref)
			closeAttempt(r.Instance, r, false)
			attempts[r.Instance] = open{name: d.Name, class: class, at: r.At}
			running[class]++
			cs := stat(class)
			cs.par = ewma(cs.par, float64(running[class]), s.cfg.Alpha, cs.par == 0)
		case JournalAccepted, JournalRefused, JournalRouteOffCandidates, JournalLateVote:
			closeAttempt(r.Instance, r, true)
		case JournalAttemptLost, JournalWithdrawn:
			closeAttempt(r.Instance, r, false) // no duration to learn from
		}
	}
	return s
}

// classOf is the class of a named unit: the qualified state name.
func (st *runState) classOf(ref unitRef) string {
	return strings.Join(append(st.defPath(st.insts[ref.Instance]), ref.State), "/")
}

// Duration is the duration EWMA of class; false when it has no history.
func (s *classStats) Duration(class string) (time.Duration, bool) {
	cs := s.classes[class]
	if cs == nil || cs.samples == 0 {
		return 0, false
	}
	return time.Duration(cs.dur), true
}

// StallThreshold is the default stall threshold of class: 3x its duration
// EWMA, else StallDefault.
func (s *classStats) StallThreshold(class string) time.Duration {
	if d, ok := s.Duration(class); ok {
		return 3 * d
	}
	return s.cfg.StallDefault
}

// estimate is the duration estimate of class, falling back to the mean over
// the classes that have one; false when none has.
func (s *classStats) estimate(class string) (time.Duration, bool) {
	if d, ok := s.Duration(class); ok {
		return d, true
	}
	var sum float64
	n := 0
	for _, cs := range s.classes {
		if cs.samples > 0 {
			sum += cs.dur
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return time.Duration(sum / float64(n)), true
}

// remaining is Remaining with the mean fallback of estimate.
func (s *classStats) remaining(u unitView, now time.Time, est func(string) (time.Duration, bool)) (time.Duration, bool) {
	if u.State.terminal() {
		return 0, true
	}
	d, ok := est(u.Class)
	if !ok {
		return 0, false
	}
	if u.running() && !u.Since.IsZero() {
		return d - now.Sub(u.Since), true
	}
	return d, true
}

// Remaining is the time a unit has left: a running unit's estimate less its
// elapsed time (negative when it is over), a pending or handed unit's
// estimate, zero for a settled one. False when its class has no history.
func (s *classStats) Remaining(u unitView, now time.Time) (time.Duration, bool) {
	return s.remaining(u, now, s.Duration)
}

// Progress counts the settled and known units; growing (a source can still
// grow) or future > 0 (agent states not yet entered) makes the total a lower
// bound.
func (s *classStats) Progress(units []unitView, growing bool, future int) Progress {
	p := Progress{Total: len(units), TotalIsLowerBound: growing || future > 0}
	for _, u := range units {
		switch {
		case u.State.terminal():
			p.Done++
		case u.State == UnitPending && !u.Ready && !u.Held:
			p.WaitingOnDeps++
		}
	}
	return p
}

// counts reports the ready pending and running units.
func counts(units []unitView) (ready, running int) {
	for _, u := range units {
		switch {
		case u.running():
			running++
		case u.State == UnitPending && u.Ready:
			ready++
		}
	}
	return ready, running
}

// Binding names what limits the run: an open human gate with nothing running,
// else the graph (more units wait on dependencies than are ready). The engine
// sets no cap of its own, so ready work never binds it.
func (s *classStats) Binding(units []unitView, humanGates int) Binding {
	ready, running := counts(units)
	switch {
	case humanGates > 0 && running == 0:
		return BindingHuman
	case s.Progress(units, false, 0).WaitingOnDeps > ready:
		return BindingGraph
	}
	return BindingNone
}

// ETA is the time left: the larger of the remaining work spread over the
// observed parallelism and the remaining critical path. future[i] is the agent
// state classes ahead of units[i]'s instance (futureSteps, futureOf.classes).
// False when no class has history.
func (s *classStats) ETA(units []unitView, future [][]string, now time.Time) (time.Duration, bool) {
	var work, critical time.Duration
	for i := 0; i < max(len(units), len(future)); i++ {
		var path time.Duration
		if i < len(future) {
			for _, class := range future[i] {
				d, _ := s.estimate(class)
				path += d
			}
		}
		var rem time.Duration
		if i < len(units) {
			rem, _ = s.remaining(units[i], now, s.estimate)
			rem = max(rem, 0)
		}
		work += rem + path
		critical = max(critical, rem+path)
	}
	par := 0.0
	have := false
	for _, cs := range s.classes {
		par += cs.par
		have = have || cs.samples > 0
	}
	if !have {
		return 0, false
	}
	return max(time.Duration(float64(work)/max(par, 1)), critical), true
}
