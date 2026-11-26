package wf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// ErrUndeclaredObservation marks an Observed read of an observation the
// running Check or Compute did not declare; the step is classed Blocked.
var ErrUndeclaredObservation = errors.New("observation not declared by the running check or compute")

// runCtx is the Ctx the Engine hands to user closures.
//
// Owns: every value a closure reads, served only from the folded journal: run
// params, options, counters, attempt number, accepted and computed outputs,
// the instance input, child results, artifact refs, the blocked cause and the
// observations
// journaled for the closure's declared set. Values cross as raw JSON and are
// decoded by the typed accessor, so replay never needs the type.
// Does not: read the world (adapters are called by the Engine, which journals
// the result first), allow writes, or serve an Observation the closure did not
// declare: that sets blocked and returns ErrUndeclaredObservation.
// Extend by: a new readable value is a new ctxKey kind resolved in value.
type runCtx struct {
	c        *cycle
	in       *instance
	declared map[string]bool // canonical descriptors the closure declared
	blocked  bool
	why      string
}

// blockedErr is the closure's undeclared read as an ErrUndeclaredObservation:
// a definition bug (A3), which failErr halts on.
func (x *runCtx) blockedErr() error { return fmt.Errorf("%w: %s", ErrUndeclaredObservation, x.why) }

func (c *cycle) ctx(in *instance, declared ...ObservationDesc) (*runCtx, error) {
	x := &runCtx{c: c, in: in, declared: map[string]bool{}}
	for _, d := range declared {
		b, err := c.scope(in).bindDesc(d)
		if err != nil {
			return nil, err
		}
		x.declared[canonDesc(b)] = true
	}
	return x, nil
}

// EdgeEffect is the outcome of one external effect on the edge that entered a
// state: Outcome is applied, done, settled, failed or unavailable.
type EdgeEffect struct{ Adapter, Op, Outcome, Error string }

// EdgeResult is what the external effects of the edge into a state did. Skipped
// counts the effects that never ran because an earlier one failed.
type EdgeResult struct {
	Effects []EdgeEffect `json:",omitempty"`
	Skipped int          `json:",omitempty"`
}

// Failed reports that an effect on the edge did not succeed (failed,
// unavailable, uncertain or refused), or that
// one was skipped because an earlier one failed.
func (r EdgeResult) Failed() bool {
	if r.Skipped > 0 {
		return true
	}
	for _, e := range r.Effects {
		if effectFailed(e.Outcome) {
			return true
		}
	}
	return false
}

// firstError is the first failed effect's error, for messages.
func (r EdgeResult) firstError() string {
	for _, e := range r.Effects {
		if effectFailed(e.Outcome) {
			return fmt.Sprintf("%s.%s %s: %s", e.Adapter, e.Op, e.Outcome, e.Error)
		}
	}
	if r.Skipped > 0 {
		return fmt.Sprintf("%d edge effect(s) skipped after an earlier failure", r.Skipped)
	}
	return ""
}

// EdgeOutcome is the outcome of the external effects on the edge that entered
// the current state. Only an Optional edge enters a state past a failed
// effect; after any other edge every effect applied.
func EdgeOutcome(c Ctx) EdgeResult { return read[EdgeResult](c, "edge", nil) }

// BlockedCause is the cause of the Blocked failure (`wf failed --class
// blocked`, a not-applied effect, an engine Blocked class) whose
// OnClass(Blocked) route entered the current state, as the {blockedCause}
// placeholder binds it; "" when the state was entered any other way.
func BlockedCause(c Ctx) string { return read[string](c, "blockedCause", nil) }

func (x *runCtx) Param(p Param) string {
	value, err := x.c.paramValue(p.name)
	if err != nil {
		x.blocked, x.why = true, err.Error()
		return ""
	}
	return value
}
func (x *runCtx) Count(c Counter) int { return x.in.Counters[c.name] }
func (x *runCtx) Eval(b Budget) int   { return x.in.eval(b) }
func (x *runCtx) Attempt() int        { return x.in.Attempt }
func (x *runCtx) value(k ctxKey) (any, error) {
	in := x.in
	switch k.kind {
	case "gateInt":
		var value *int
		if n, ok := in.AnswerInts[k.ref.(*node).name]; ok {
			value = &n
		}
		raw, err := json.Marshal(value)
		return json.RawMessage(raw), err
	case "pathTemplate":
		return x.c.scope(in).bindString(k.ref.(string))
	case "runDir":
		return x.c.resultDir(), nil
	case "options":
		return x.c.st.started.Options, nil
	case "output":
		return x.pathDocument(in.Outputs[k.ref.(*node).name])
	case "computed":
		return x.pathDocument(in.Computed[k.ref.(*node).name])
	case "input":
		return x.pathDocument(in.Input)
	case "child":
		return x.pathDocument(in.Children[k.ref.(*node).name])
	case "childOutcome":
		b, err := json.Marshal(in.ChildOutcomes[k.ref.(*node).name])
		return json.RawMessage(b), err
	case "joined":
		return x.pathDocument(in.Computed[k.ref.(*node).name]) // the Map's join, accepted as computed
	case "ref", "artifact":
		a := k.ref.(*artifactData)
		ref, ok := in.Artifacts[a.name]
		if !ok {
			p, err := x.c.artifactPath(in, a)
			if err != nil {
				return nil, err
			}
			ref = ArtifactRef{Path: p}
		}
		if k.kind == "ref" {
			b, err := json.Marshal(ref)
			return json.RawMessage(b), err
		}
		// In a fork, an artifact at a source path is never opened.
		p := ref.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(x.c.workdir(in), p)
		}
		if err := x.c.checkForkFile(p, "artifact "+a.name); err != nil {
			x.blocked, x.why = true, err.Error()
			return nil, err
		}
		return readArtifact(x.c.workdir(in), ref)
	case "blockedCause":
		b, err := json.Marshal(in.blockedCause())
		return json.RawMessage(b), err
	case "edge":
		b, err := json.Marshal(in.LastEdge)
		return json.RawMessage(b), err
	case "observed":
		d, err := x.c.scope(in).bindDesc(k.ref.(ObservationDesc))
		if err != nil {
			return nil, err
		}
		key := canonDesc(d)
		if !x.declared[key] {
			x.blocked, x.why = true, fmt.Sprintf("%s.%s is not declared by the running check or compute", d.Adapter, d.Op)
			return nil, fmt.Errorf("%w: %s.%s", ErrUndeclaredObservation, d.Adapter, d.Op)
		}
		o, ok := in.Observed[key]
		switch {
		case !ok:
			return nil, fmt.Errorf("observation %s.%s was not read", d.Adapter, d.Op)
		case o.Unavailable:
			return nil, fmt.Errorf("%w: %s", ErrUnavailable, o.Error)
		case o.Error != "":
			return nil, errors.New(o.Error)
		}
		return o.Value, nil
	}
	return nil, fmt.Errorf("unknown context read %q", k.kind)
}

func (x *runCtx) pathDocument(raw json.RawMessage) (any, error) {
	resolved, err := x.c.decodeOwnedPathValues(raw)
	if err != nil {
		x.blocked, x.why = true, err.Error()
	}
	return resolved, err
}

// readArtifact serves an artifact's content as JSON: the file itself when it is
// valid JSON, else a JSON string. A hash recorded at submission must match.
func readArtifact(workspace string, ref ArtifactRef) (json.RawMessage, error) {
	p := ref.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if ref.Hash != "" && hashBytes(b) != ref.Hash {
		return nil, fmt.Errorf("artifact %s changed since it was submitted", ref.Path)
	}
	if json.Valid(b) {
		return b, nil
	}
	return json.Marshal(string(b))
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// callYield runs a terminal's Yield closure, a func(Ctx) R held as any.
func callYield(fn any, ctx Ctx) any {
	return reflect.ValueOf(fn).Call([]reflect.Value{reflect.ValueOf(&ctx).Elem()})[0].Interface()
}
