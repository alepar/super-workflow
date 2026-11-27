package wf

import (
	"errors"
	"reflect"
)

// computeOps is the typed half of a Compute: it reads the declared
// observations (journaled), runs the closure over the journaled values and
// routes on its output.
type computeOps[S, O any] struct{ c *Compute[S, O] }

func (o computeOps[S, O]) execute(c *cycle, in *instance, n *node) error {
	k := o.c.k
	// DoneWhen on a compute only journals its probe: the closure is
	// deterministic, so skipping it would route on a zero output.
	if _, ok, err := c.doneWhenMet(in, n); err != nil || !ok {
		return err
	}
	descs, err := c.bindDescs(in, k.observes)
	if err != nil {
		return c.failErr(in, n, err)
	}
	for _, d := range descs {
		if _, err := c.readObs(in, d, "compute", true); err != nil {
			return err
		}
	}
	ctx, err := c.ctx(in, k.observes...)
	if err != nil {
		return c.failErr(in, n, err)
	}
	fn, _ := k.fn.(func(Ctx) (O, error))
	if fn == nil {
		return c.fail(in, n, Blocked, []string{"compute has no function"}, "")
	}
	out, ferr := fn(ctx)
	switch {
	case ctx.blocked:
		return c.failErr(in, n, ctx.blockedErr())
	case ferr != nil && errors.Is(ferr, ErrUnavailable):
		return c.fail(in, n, Unavailable, []string{ferr.Error()}, "")
	case ferr != nil:
		return c.fail(in, n, Refused, []string{ferr.Error()}, ferr.Error())
	}
	if err := validatePatterns(reflect.ValueOf(out), "computed"); err != nil {
		return c.fail(in, n, Refused, []string{err.Error()}, err.Error())
	}
	raw, err := c.marshalPathValue(in, out)
	if err != nil {
		return c.fail(in, n, Refused, []string{"computed value is not JSON: " + err.Error()}, "")
	}
	// The route closure reads what the compute observed (journaled just above), so
	// a free-form route can choose its effects from the same tracker state.
	rctx, err := c.ctx(in, k.observes...)
	if err != nil {
		return c.failErr(in, n, err)
	}
	var fnRoute func(Ctx) edgeUse
	if o.c.route != nil {
		fnRoute = func(x Ctx) edgeUse { return o.c.route(out, x).use() }
	}
	u, ok, err := c.route(in, n, fnRoute, any(out), rctx)
	if err != nil || !ok {
		return err
	}
	plan, err := c.planOf(in, u, raw)
	if err != nil {
		return c.halt(in, err.Error())
	}
	return c.emit(in, JournalAccepted, Accepted, AcceptedData{Output: raw, Computed: true, Plan: plan})
}
