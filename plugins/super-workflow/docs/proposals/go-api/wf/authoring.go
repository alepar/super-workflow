package wf

// ThenTo declares an unconditional route to target using the given label.
func (a *Agent[S, O]) ThenTo(target State, label string) {
	a.Then(a.To(target, label))
}

// ThenTo declares an unconditional route to target using the given label.
func (c *Compute[S, O]) ThenTo(target State, label string) {
	c.Then(c.To(target, label))
}

// ThenTo declares an unconditional route to target using the given label.
func (c *Command[S]) ThenTo(target State, label string) {
	c.Then(c.To(target, label))
}

// Attempts sets a fixed total attempt budget for n >= 1, including the initial
// attempt. With n == 0 the initial attempt still runs, as with Retry(Const(0), exhausted).
func Attempts[S any](n int, exhausted Edge[S]) RetryPolicy[S] {
	return Retry(Const(n), exhausted)
}

// RouteValue routes on the entire state output using an exhaustive enum table.
func RouteValue[S any, V comparable](state interface{ RouteCases(CaseTable[S, V]) }, values Enum[V], cases ...Case[S, V]) {
	state.RouteCases(Cases(func(value V) V { return value }, values, cases...))
}
