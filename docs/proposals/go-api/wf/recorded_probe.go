package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ErrContextUnknown means a recording cannot establish the context in which
// its checks ran. It must not be calibrated against another run's context.
var ErrContextUnknown = errors.New("context unknown")

type recordedCalibrationOps interface {
	probeRecorded(json.RawMessage, *CheckContexts) (map[string]string, error)
}

// CheckRecordedOutput evaluates an output using only its harvested context.
// It never reads a journal, a World, or artifact files. Each recorded read must
// be consumed, and a new read absent from the recording fails calibration.
func (s StateView) CheckRecordedOutput(output json.RawMessage, context *CheckContexts) (map[string]string, error) {
	k, ok := s.node.kind.(*agentKind)
	if !ok {
		return nil, fmt.Errorf("%s is not an agent", s.Name)
	}
	return k.ops.(recordedCalibrationOps).probeRecorded(output, context)
}

func (o agentOps[S, O]) probeRecorded(raw json.RawMessage, context *CheckContexts) (map[string]string, error) {
	if context != nil && context.Version > 1 {
		return nil, fmt.Errorf("%w: %w", ErrContextUnknown, NewerFormat("check context", context.Version, 1))
	}
	if context == nil || context.Version != 1 {
		return nil, fmt.Errorf("%w: absent or unsupported check context", ErrContextUnknown)
	}
	if len(context.Checks) != len(o.a.checks) {
		return nil, fmt.Errorf("%w: check list changed", ErrContextUnknown)
	}
	for i, ck := range o.a.checks {
		recorded := context.Checks[i]
		if recorded.Name != ck.Name {
			return nil, fmt.Errorf("%w: check %d identity changed", ErrContextUnknown, i)
		}
		if len(recorded.Incomplete) > 0 {
			return nil, fmt.Errorf("%w: check %s: %s", ErrContextUnknown, ck.Name, strings.Join(recorded.Incomplete, "; "))
		}
	}
	result := map[string]string{}
	out, _, err := checkedAgentOutput[O](raw, o.a.n.def)
	if err != nil {
		result["$decode"] = err.Error()
		return result, nil
	}
	for i, ck := range o.a.checks {
		declared := append(descsOfObsReaders(ck.observes), ck.fromDescs(out)...)
		ctx, err := newRecordedProbeCtx(o.a.n.def, context.Checks[i].Reads, declared)
		if err != nil {
			return nil, fmt.Errorf("check %s: %w", ck.Name, err)
		}
		cerr := ck.fn(out, ctx)
		if ctx.err != nil {
			return nil, fmt.Errorf("check %s: %w", ck.Name, ctx.err)
		}
		for _, key := range ctx.order {
			if !ctx.used[key] {
				return nil, fmt.Errorf("override of %s not read by check %s", key, ck.Name)
			}
		}
		if cerr != nil {
			result[ck.Name] = cerr.Error()
		}
	}
	return result, nil
}

type recordedProbeCtx struct {
	def      *Def
	reads    map[string]RecordedRead
	order    []string
	used     map[string]bool
	declared map[string]bool
	err      error
}

func replayReadKey(r RecordedRead) (string, error) {
	switch r.Kind {
	case "output", "computed", "child", "childOutcome", "joined", "gateInt":
		return r.Kind + "/" + r.Def + "/" + r.State, nil
	case "param", "count", "pathTemplate":
		return r.Kind + "/" + r.Name, nil
	case "observed":
		if r.Desc == nil {
			return "", fmt.Errorf("observation descriptor missing")
		}
		return r.Kind + "/" + canonDesc(*r.Desc), nil
	case "input", "attempt", "options", "blockedCause", "edge", "runDir":
		return r.Kind, nil
	default:
		return "", fmt.Errorf("%w: unsupported read %s", ErrContextUnknown, r.Kind)
	}
}

func newRecordedProbeCtx(d *Def, reads []RecordedRead, declared []ObservationDesc) (*recordedProbeCtx, error) {
	x := &recordedProbeCtx{def: d, reads: map[string]RecordedRead{}, used: map[string]bool{}, declared: map[string]bool{}}
	for _, desc := range declared {
		x.declared[canonDesc(desc)] = true
	}
	for _, r := range reads {
		key, err := replayReadKey(r)
		if err != nil {
			return nil, err
		}
		if _, ok := x.reads[key]; ok {
			return nil, fmt.Errorf("duplicate recorded read %s", key)
		}
		if r.ErrorKind != "" && r.ErrorKind != "unavailable" && r.ErrorKind != "undeclared" {
			return nil, fmt.Errorf("%w: unsupported error kind %s", ErrContextUnknown, r.ErrorKind)
		}
		if r.Error == "" && !json.Valid(r.Value) {
			return nil, fmt.Errorf("invalid or unresolved recorded value %s", key)
		}
		if err := validateRecordedValue(d, r); err != nil {
			return nil, err
		}
		// A live read may spell a declared observation with already-bound
		// arguments. Capture preserves the raw declaration that authorized it.
		if r.Kind == "observed" && r.Declaration != nil {
			if !x.declared[canonDesc(*r.Declaration)] {
				return nil, fmt.Errorf("recorded observation declaration changed")
			}
			x.declared[canonDesc(*r.Desc)] = true
		}
		x.reads[key] = r
		x.order = append(x.order, key)
	}
	return x, nil
}

func validateRecordedValue(d *Def, r RecordedRead) error {
	var typ reflect.Type
	switch r.Kind {
	case "output", "computed", "child", "childOutcome", "joined", "gateInt":
		if r.Def != d.name {
			return fmt.Errorf("recorded state %s belongs to another definition", r.State)
		}
		var target *node
		for _, n := range d.nodes {
			if n.name == r.State {
				target = n
				break
			}
		}
		if target == nil {
			return fmt.Errorf("recorded state %s not in definition %s", r.State, d.name)
		}
		switch k := target.kind.(type) {
		case *gateKind:
			if r.Kind == "gateInt" {
				typ = reflect.TypeOf((*int)(nil))
			}
		case *agentKind:
			if r.Kind == "output" {
				typ = k.outT
			}
		case *computeKind:
			if r.Kind == "computed" {
				typ = k.outT
			}
		case *subKind:
			if r.Kind == "child" {
				typ = k.child.result
			}
			if r.Kind == "childOutcome" {
				typ = reflect.TypeOf(Outcome(""))
			}
		case *mapKind:
			if r.Kind == "joined" {
				return nil
			}
		}
		if typ == nil {
			return fmt.Errorf("invalid recorded state kind %s/%s", r.Kind, r.State)
		}
	case "param", "blockedCause", "runDir", "pathTemplate":
		typ = reflect.TypeOf("")
	case "count", "attempt":
		typ = reflect.TypeOf(0)
	case "input":
		typ = d.input
	case "edge":
		typ = reflect.TypeOf(EdgeResult{})
	}
	if typ != nil && r.Error == "" {
		if err := json.Unmarshal(r.Value, reflect.New(typ).Interface()); err != nil {
			return fmt.Errorf("invalid recorded %s value: %w", r.Kind, err)
		}
	}
	return nil
}

func (x *recordedProbeCtx) take(r RecordedRead) (any, error) {
	key, err := replayReadKey(r)
	if err != nil {
		x.err = err
		return nil, err
	}
	v, ok := x.reads[key]
	if !ok {
		err = fmt.Errorf("context read %s not recorded", key)
		x.err = err
		return nil, err
	}
	x.used[key] = true
	if v.Error != "" {
		switch v.ErrorKind {
		case "unavailable":
			return nil, recordedProbeError{v.Error, ErrUnavailable}
		case "undeclared":
			return nil, recordedProbeError{v.Error, ErrUndeclaredObservation}
		default:
			return nil, errors.New(v.Error)
		}
	}
	return append(json.RawMessage(nil), v.Value...), nil
}

type recordedProbeError struct {
	message string
	cause   error
}

func (e recordedProbeError) Error() string { return e.message }
func (e recordedProbeError) Unwrap() error { return e.cause }
func (x *recordedProbeCtx) Param(p Param) string {
	v, _ := x.take(RecordedRead{Kind: "param", Name: p.name})
	return decodeValue[string](v)
}
func (x *recordedProbeCtx) Count(c Counter) int {
	v, _ := x.take(RecordedRead{Kind: "count", Name: c.name})
	return decodeValue[int](v)
}
func (x *recordedProbeCtx) Attempt() int {
	v, _ := x.take(RecordedRead{Kind: "attempt"})
	return decodeValue[int](v)
}
func (x *recordedProbeCtx) Eval(Budget) int {
	x.err = fmt.Errorf("%w: Eval read", ErrContextUnknown)
	return 0
}
func (x *recordedProbeCtx) value(k ctxKey) (any, error) {
	r := RecordedRead{Kind: k.kind}
	switch k.kind {
	case "pathTemplate":
		r.Name = k.ref.(string)
	case "output", "computed", "child", "childOutcome", "joined", "gateInt":
		n := k.ref.(*node)
		r.Def, r.State = n.def.name, n.name
	case "observed":
		d := k.ref.(ObservationDesc)
		r.Desc = &d
		if !x.declared[canonDesc(d)] {
			x.err = fmt.Errorf("%w: %s.%s", ErrUndeclaredObservation, d.Adapter, d.Op)
			return nil, x.err
		}
	}
	return x.take(r)
}
