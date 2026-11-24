package wf

import (
	"slices"
	"testing"
)

type (
	foMode  string
	foInner struct{ X bool }
	foOpts  struct {
		foInner
		Autonomous bool
		Mode       foMode
		Ptr        *foInner
		hidden     bool
	}
)

// optCtx is a Ctx serving only the run's options.
type optCtx struct{ opts any }

func (optCtx) Param(Param) string { return "" }
func (optCtx) Count(Counter) int  { return 0 }
func (optCtx) Eval(Budget) int    { return 0 }
func (optCtx) Attempt() int       { return 0 }
func (c optCtx) value(k ctxKey) (any, error) {
	if k.kind == "options" {
		return c.opts, nil
	}
	return nil, nil
}

func TestFieldIsKeepsCanonicalString(t *testing.T) {
	opts := Options[foOpts](New("fo"))
	auto := Field(opts, func(o *foOpts) *bool { return &o.Autonomous })
	mode := Field(opts, func(o *foOpts) *foMode { return &o.Mode })
	for want, p := range map[string]Pred{
		"Autonomous=true":               auto.Is(true),
		"Mode=fast":                     mode.Is("fast"),
		"not Autonomous=true":           Not(auto.Is(true)),
		"Autonomous=true and Mode=fast": auto.Is(true).And(mode.Is("fast")),
	} {
		if got := p.String(); got != want {
			t.Errorf("String = %q, want %q", got, want)
		}
	}
}

func TestFieldOfReadsCtx(t *testing.T) {
	d := New("fo")
	opts := Options[foOpts](d)
	c := optCtx{opts: foOpts{Autonomous: true, Mode: "fast"}}
	if !Field(opts, func(o *foOpts) *bool { return &o.Autonomous }).Of(c) {
		t.Error("Of(Autonomous) = false")
	}
	if got := Field(opts, func(o *foOpts) *foMode { return &o.Mode }).Of(c); got != "fast" {
		t.Errorf("Of(Mode) = %q", got)
	}
	if len(d.problems) != 0 {
		t.Errorf("problems: %v", d.problems)
	}
}

func TestFieldMisuseIsABuildProblem(t *testing.T) {
	for name, sel := range map[string]func(*foOpts) *bool{
		"non-field pointer": func(*foOpts) *bool { return new(bool) },
		"nil":               func(*foOpts) *bool { return nil },
		"nil selector":      nil,
		"embedded field":    func(o *foOpts) *bool { return &o.X },
		"unexported field":  func(o *foOpts) *bool { return &o.hidden },
		"selector panics":   func(o *foOpts) *bool { return &o.Ptr.X },
	} {
		t.Run(name, func(t *testing.T) {
			d := New("fo")
			f := Field(Options[foOpts](d), sel)
			if !slices.ContainsFunc(d.Lint(), func(p Problem) bool { return p.Rule == RuleBuild }) {
				t.Fatalf("no build problem: %v", d.Lint())
			}
			if f.Of(optCtx{opts: foOpts{Autonomous: true}}) {
				t.Error("a broken handle read a value")
			}
		})
	}
}

func TestFieldOnNonStructOptions(t *testing.T) {
	d := New("fo")
	Field(Options[int](d), func(o *int) *int { return o })
	if !slices.ContainsFunc(d.Lint(), func(p Problem) bool { return p.Rule == RuleBuild }) {
		t.Fatalf("no build problem: %v", d.Lint())
	}
}
