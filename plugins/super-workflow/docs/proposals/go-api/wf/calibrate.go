package wf

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// CheckExample documents a check's accepting or refusing contract. Refusal is
// the expected check error (without its name); empty means acceptance.
type CheckExample struct {
	Output       json.RawMessage
	Refusal      string
	Observations []ExampleRead
}

// Accepts declares a value this check must accept.
func (k Check[O]) Accepts(out O, observations ...ExampleRead) Check[O] {
	raw, err := json.Marshal(out)
	if err != nil {
		panic(fmt.Sprintf("check %s example: %v", k.Name, err))
	}
	k.examples = append(append([]CheckExample{}, k.examples...), CheckExample{Output: raw, Observations: append([]ExampleRead{}, observations...)})
	return k
}

// Refuses declares a value this check must refuse with the given error text.
func (k Check[O]) Refuses(out O, reason string, observations ...ExampleRead) Check[O] {
	if reason == "" {
		panic("Refuses requires a refusal reason")
	}
	k = k.Accepts(out, observations...)
	k.examples[len(k.examples)-1].Refusal = reason
	return k
}

type calibrationOps interface {
	examples() map[string][]CheckExample
	probe(*cycle, *instance, json.RawMessage, string, []ExampleRead) (map[string]string, error)
}

func (o agentOps[S, O]) examples() map[string][]CheckExample {
	result := map[string][]CheckExample{}
	for _, check := range o.a.checks {
		result[check.Name] = append([]CheckExample{}, check.examples...)
	}
	return result
}

// CheckExamples returns the contracts of this agent state's named checks.
func (s StateView) CheckExamples() map[string][]CheckExample {
	if k, ok := s.node.kind.(*agentKind); ok {
		return k.ops.(calibrationOps).examples()
	}
	return nil
}

// CheckOutput probes only decoding, enum validation and the selected named
// check (all checks when check is empty). It uses the live unit's context and
// declared adapter reads, but never routes, runs effects or writes the journal.
// Refusals are keyed by check name; "$decode" identifies decoding errors.
func (e *Engine) CheckOutput(run RunID, name string, output json.RawMessage, check string, observations ...ExampleRead) (refusals map[string]string, err error) {
	err = e.withDryRun(run, func(c *cycle) error {
		in, _, er := c.liveUnit("check-output", name)
		if er != nil {
			return er
		}
		k, ok := in.node().kind.(*agentKind)
		if !ok {
			return fmt.Errorf("%s is not an agent", name)
		}
		refusals, er = k.ops.(calibrationOps).probe(c, in, output, check, observations)
		return er
	})
	return
}

func (o agentOps[S, O]) probe(c *cycle, in *instance, raw json.RawMessage, only string, observations []ExampleRead) (map[string]string, error) {
	result := map[string]string{}
	if len(observations) > 0 && only == "" {
		return nil, fmt.Errorf("example overrides require a named check")
	}
	out, _, err := checkedAgentOutput[O](raw, in.def)
	if err != nil {
		result["$decode"] = err.Error()
		return result, nil
	}
	found := only == ""
	for _, check := range o.a.checks {
		if only != "" && check.Name != only {
			continue
		}
		found = true
		declared := descsOfObsReaders(check.observes)
		declared = append(declared, check.fromDescs(out)...)
		bound, err := c.bindDescs(in, declared)
		if err != nil {
			return nil, err
		}
		overrides := map[string]json.RawMessage{}
		contextOverrides := map[ctxKey]json.RawMessage{}
		var contextKeys []ctxKey
		for _, example := range observations {
			if example.node != nil {
				if example.node.def.name != in.def.name {
					return nil, fmt.Errorf("example state %s not in definition %s", example.node.name, in.def.name)
				}
				var target *node
				// Calibration factories rebuild equivalent definitions for each seed.
				for _, n := range in.def.nodes {
					if n.name == example.node.name {
						target = n
						break
					}
				}
				if target == nil {
					return nil, fmt.Errorf("example state %s not in definition %s", example.node.name, in.def.name)
				}
				var actual reflect.Type
				switch k := target.kind.(type) {
				case *agentKind:
					if example.kind == "output" {
						actual = k.outT
					}
				case *computeKind:
					if example.kind == "computed" {
						actual = k.outT
					}
				}
				if actual == nil || actual != example.valueType {
					return nil, fmt.Errorf("example state %s has incompatible type", example.node.name)
				}
				if err := json.Unmarshal(example.Value, reflect.New(actual).Interface()); err != nil {
					return nil, fmt.Errorf("example value for %s: %w", example.node.name, err)
				}
				key := ctxKey{kind: example.kind, ref: target}
				if _, exists := contextOverrides[key]; exists {
					return nil, fmt.Errorf("duplicate example %s of %s", example.kind, example.node.name)
				}
				contextOverrides[key] = example.Value
				contextKeys = append(contextKeys, key)
				continue
			}
			desc, err := c.scope(in).bindDesc(example.Desc)
			if err != nil {
				return nil, err
			}
			key := canonDesc(desc)
			declared := false
			for _, d := range bound {
				if canonDesc(d) == key {
					declared = true
					break
				}
			}
			if !declared {
				return nil, fmt.Errorf("example overrides undeclared observation %s.%s", desc.Adapter, desc.Op)
			}
			if _, exists := overrides[key]; exists {
				return nil, fmt.Errorf("duplicate example observation %s.%s", desc.Adapter, desc.Op)
			}
			overrides[key] = example.Value
		}
		for _, desc := range bound {
			if raw, ok := overrides[canonDesc(desc)]; ok {
				if in.Observed == nil {
					in.Observed = map[string]ObservedData{}
				}
				in.Observed[canonDesc(desc)] = ObservedData{Desc: desc, Value: raw}
				continue
			}
			if _, err = c.readObs(in, desc, "check", true); err != nil {
				return nil, err
			}
		}
		ctx, err := c.ctx(in, declared...)
		if err != nil {
			return nil, err
		}
		exampleCtx := &calibrationCtx{Ctx: ctx, overrides: contextOverrides, read: map[ctxKey]bool{}}
		if err = check.fn(out, exampleCtx); err != nil {
			result[check.Name] = err.Error()
		}
		if ctx.blocked {
			return nil, ctx.blockedErr()
		}
		for _, key := range contextKeys {
			if !exampleCtx.read[key] {
				return nil, fmt.Errorf("override of %s not read by check %s", key.ref.(*node).name, check.Name)
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("unknown check %q", only)
	}
	return result, nil
}

func descsOfObsReaders(readers []Reader) []ObservationDesc {
	var out []ObservationDesc
	for _, r := range readers {
		out = append(out, r.Desc())
	}
	return out
}

// ExampleRead overrides one declared observation or actually read prior result
// for this example only.
// The descriptor and value are retained in the contract and report.
type ExampleRead struct {
	Desc      ObservationDesc
	Value     json.RawMessage
	kind      string
	node      *node
	valueType reflect.Type
}

func ExampleObserved[T any](observation Observation[T], value T) ExampleRead {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("example observation: %v", err))
	}
	return ExampleRead{Desc: observation.Desc(), Value: raw}
}

// ExampleOutputOf supplies an agent result only when the example's check reads
// that exact state through OutputOf. It never changes the run's recorded output.
func ExampleOutputOf[S, O any](state *Agent[S, O], value O) ExampleRead {
	return exampleResult("output", state.n, value)
}

// ExampleComputedOf supplies a compute result only when the example's check
// reads that exact state through ComputedOf.
func ExampleComputedOf[S, O any](state *Compute[S, O], value O) ExampleRead {
	return exampleResult("computed", state.n, value)
}

func exampleResult[O any](kind string, node *node, value O) ExampleRead {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("example %s: %v", kind, err))
	}
	return ExampleRead{Value: raw, kind: kind, node: node, valueType: reflect.TypeFor[O]()}
}

// String describes the override for calibration reports.
func (r ExampleRead) String() string {
	if r.node != nil {
		name := "OutputOf"
		if r.kind == "computed" {
			name = "ComputedOf"
		}
		return fmt.Sprintf("%s(%s) = %s", name, r.node.name, r.Value)
	}
	return fmt.Sprintf("%s.%s %v = %s", r.Desc.Adapter, r.Desc.Op, r.Desc.Args, r.Value)
}

type calibrationCtx struct {
	Ctx
	overrides map[ctxKey]json.RawMessage
	read      map[ctxKey]bool
}

func (c *calibrationCtx) value(key ctxKey) (any, error) {
	if key.kind == "output" || key.kind == "computed" {
		if value, ok := c.overrides[key]; ok {
			c.read[key] = true
			return append(json.RawMessage(nil), value...), nil
		}
	}
	return c.Ctx.value(key)
}
