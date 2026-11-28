package wf

import "time"

// OutcomeClass is the closed outcome vocabulary of a step attempt.
//
// Owns: the vocabulary for per-class fallback routes and for the declared
// journal and executor types.
// Does not: grow per definition; definition-specific stop reasons are terminal
// marker types, not new classes.
// Extend by: nothing; it is deliberately closed.
type OutcomeClass int

// The outcome classes.
const (
	Accepted OutcomeClass = iota + 1
	Rejected
	// Refused: a guard or precondition refused the step. It never stands for the
	// executor's run budget; a Map child stopped on the budget is reported by
	// Instance.BudgetStopped.
	Refused
	Unavailable
	Uncertain
	Stale
	Blocked
)

// String returns the lower-case class name.
func (c OutcomeClass) String() string {
	switch c {
	case Accepted:
		return "accepted"
	case Rejected:
		return "rejected"
	case Refused:
		return "refused"
	case Unavailable:
		return "unavailable"
	case Uncertain:
		return "uncertain"
	case Stale:
		return "stale"
	case Blocked:
		return "blocked"
	}
	return "unknown"
}

type retryData struct {
	max         Budget
	noProgress  int
	fingerprint string
	fp          func(Ctx) string
	exhausted   edgeUse
	stalled     *edgeUse
	byClass     map[OutcomeClass]edgeUse
	backoff     map[OutcomeClass][]time.Duration
	noRecovery  bool // agent states only: exhaustion takes the edge with no recovery offer
}

// RetryPolicy[S] (used by Agent.Spec, Command.Retry and Compute.Retry) bounds attempts of state S and names its fallback edges, all of
// which must be Edge[S].
//
// Owns: a Budget-valued attempt cap, the exhausted and stalled fallbacks,
// per-OutcomeClass fallback edges and the progress-fingerprint closure
// (default: failing check ids plus output digest). Render shows a stall only
// when NoProgressAfter is set below the attempt budget.
// Does not: exist without a fallback; there is no way to build one without an
// exhausted edge.
// Extend by: a new fallback is a builder method taking an Edge[S].
type RetryPolicy[S any] struct{ d *retryData }

// Retry sets the attempt budget and the edge taken when attempts are exhausted.
// NoProgressAfter stays unset (0) until the caller sets it.
func Retry[S any](max Budget, exhausted Edge[S]) RetryPolicy[S] {
	return RetryPolicy[S]{&retryData{max: max, exhausted: exhausted.use()}}
}

// NoProgressAfter stops early when the progress fingerprint is unchanged for n
// consecutive attempts.
func (r RetryPolicy[S]) NoProgressAfter(n int) RetryPolicy[S] {
	d := *r.d
	d.noProgress = n
	return RetryPolicy[S]{&d}
}

// OnStalled sets the edge taken on a stall (defaults to the exhausted edge).
func (r RetryPolicy[S]) OnStalled(e Edge[S]) RetryPolicy[S] {
	d := *r.d
	u := e.use()
	d.stalled = &u
	return RetryPolicy[S]{&d}
}

// OnClass sets the fallback edge for an outcome class (unavailable, blocked, ...).
// The edge is taken immediately, on the first attempt of that class. Class
// fallbacks do not consume the Retry attempt budget: that budget counts only
// attempts of other classes. For retry-then-pause see OnClassBackoff.
func (r RetryPolicy[S]) OnClass(c OutcomeClass, e Edge[S]) RetryPolicy[S] {
	d := *r.d
	d.byClass = make(map[OutcomeClass]edgeUse, len(r.d.byClass)+1)
	for k, v := range r.d.byClass {
		d.byClass[k] = v
	}
	d.byClass[c] = e.use()
	if e.e != nil {
		e.e.class = c
	}
	return RetryPolicy[S]{&d}
}

// OnClassBackoff retries an attempt of class c after each delay in turn; when
// the schedule is spent, the next attempt of class c takes then (the pause
// route). Class retries do not consume the Retry attempt budget. OnClass is
// the zero-delay case: the first attempt of class c takes the edge.
func (r RetryPolicy[S]) OnClassBackoff(c OutcomeClass, then Edge[S], delays ...time.Duration) RetryPolicy[S] {
	p := r.OnClass(c, then)
	d := *p.d
	d.backoff = make(map[OutcomeClass][]time.Duration, len(r.d.backoff)+1)
	for k, v := range r.d.backoff {
		d.backoff[k] = v
	}
	d.backoff[c] = append([]time.Duration(nil), delays...)
	return RetryPolicy[S]{&d}
}

// NoRecovery opts an agent state out of the recovery offer (loud-failures
// B2): by default, when an agent state's budget, stall or lost cap is spent,
// the coordinator is offered the unit once per state visit (restart for one
// more attempt, or give up and take the exhausted or stalled edge), at most
// RecoveryRestarts restarts per unit in a run. With NoRecovery the edge is
// taken at once. It also opts the state out of the offer of a failure that
// has no route (a Blocked failure with no Blocked route): that failure then
// halts the instance for a person. It shows in the state view, so it is part
// of the hash.
func (r RetryPolicy[S]) NoRecovery() RetryPolicy[S] {
	d := *r.d
	d.noRecovery = true
	return RetryPolicy[S]{&d}
}

// RecoveryRestarts bounds the restarts a coordinator may answer the recovery
// offers of one unit with in a run, across state visits: a route-back cycle
// answered restart every time still ends.
const RecoveryRestarts = 2

// Fingerprint replaces the progress fingerprint; the name is rendered, the
// closure is never run by lint or render.
func (r RetryPolicy[S]) Fingerprint(name string, fn func(c Ctx) string) RetryPolicy[S] {
	d := *r.d
	d.fingerprint, d.fp = name, fn
	return RetryPolicy[S]{&d}
}

// DefaultUnavailableBackoff is the backoff an Unavailable outcome takes on a
// state with no Unavailable route: five tries, then the state halts.
var DefaultUnavailableBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
