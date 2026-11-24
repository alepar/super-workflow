package wf_test

import (
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	CpA    struct{}
	CpEnd  struct{}
	CpStop struct{}
	CpWait struct{}
)

// computeDef is one compute whose closure reads a bd.show observation; shape
// receives the compute and the edges it may use and declares the rest.
func computeDef(name string, arg string, shape func(c *wf.Compute[CpA, int], done, exhausted, pause wf.Edge[CpA])) *wf.Def {
	d := wf.New(name).Version(1)
	c := wf.NewCompute[CpA, int](d, func(wf.Ctx) (int, error) { return 0, nil })
	end := wf.NewTerminal[CpEnd](d, wf.Success)
	stop := wf.NewTerminal[CpStop](d, wf.Stopped)
	wait := wf.NewTerminal[CpWait](d, wf.Stopped)
	done := c.To(end, "done")
	shape(c, done, c.To(stop, "failed x3"), c.To(wait, "pause"))
	c.Route(func(int, wf.Ctx) wf.Edge[CpA] { return done }, done)
	return d
}

func showObs(arg string) wf.Reader {
	return wf.Observe[int]("bd", "show", map[string]any{"id": arg})
}

func TestComputeObservesRenderedAndHashed(t *testing.T) {
	mk := func(arg string) *wf.Def {
		return computeDef("obs", arg, func(c *wf.Compute[CpA, int], done, exhausted, pause wf.Edge[CpA]) {
			c.Observes(showObs(arg))
			c.Retry(wf.Retry(wf.Const(3), exhausted).OnClass(wf.Unavailable, pause))
		})
	}
	d := mk("{epic}")
	if out := d.Render(); !strings.Contains(out, "observes: bd.show") {
		t.Fatalf("render lacks the observes line:\n%s", out)
	}
	if d.Hash() == mk("{other}").Hash() {
		t.Fatal("differing observation args hash alike")
	}
	if d.Hash() != mk("{epic}").Hash() {
		t.Fatal("hash not stable for equal observations")
	}
}

func TestComputeRetryRoutes(t *testing.T) {
	d := computeDef("routes", "{epic}", func(c *wf.Compute[CpA, int], done, exhausted, pause wf.Edge[CpA]) {
		c.Observes(showObs("{epic}"))
		c.Retry(wf.Retry(wf.Const(3), exhausted).OnClassBackoff(wf.Unavailable, pause, time.Second))
	})
	var rv *wf.RetryView
	for _, s := range wf.Inspect(d).States {
		if s.Name == "CpA" {
			rv = s.Retry
		}
	}
	if rv == nil {
		t.Fatal("no retry view on CpA")
	}
	if rv.ByClass["unavailable"] != "pause" {
		t.Fatalf("ByClass[unavailable] = %q, want pause", rv.ByClass["unavailable"])
	}
	if got := rv.Backoff["unavailable"]; len(got) != 1 || got[0] != time.Second {
		t.Fatalf("Backoff[unavailable] = %v, want [1s]", got)
	}
	if out := d.Render(); !strings.Contains(out, "[on unavailable") {
		t.Fatalf("render lacks the class annotation:\n%s", out)
	}
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
}

func TestComputeWithoutObservesNeedsNoRoute(t *testing.T) {
	d := computeDef("plain", "", func(*wf.Compute[CpA, int], wf.Edge[CpA], wf.Edge[CpA], wf.Edge[CpA]) {})
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
}
