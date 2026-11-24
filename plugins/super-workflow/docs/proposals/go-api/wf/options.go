package wf

import (
	"fmt"
	"reflect"
	"strings"
)

type optionsData struct{ typ reflect.Type }

// RunOptions is a typed options struct declared once per Def; its fields are
// registered-Enum-typed values.
//
// Owns: the typed, Enum-valued run options of a definition. It is a typed
// input of the definition, passed to Subs through their input I, read by
// routes through Ctx and by policy rules as data predicates. Lint and render
// run with no concrete options: render shows every option-dependent edge and
// policy rule with its predicate.
// Does not: provide untyped flags; routes and policies never read them. Field
// is the way routes and policies name an option field.
// Extend by: a new option is a field on the options struct.
type RunOptions[T any] struct{ d *Def }

// Options declares the typed options struct T on d.
func Options[T any](d *Def) RunOptions[T] {
	if d.options != nil {
		d.misuse("Options declared twice")
	} else {
		d.options = &optionsData{typ: reflect.TypeFor[T]()}
	}
	return RunOptions[T]{d: d}
}

// OptionsOf reads the run's options from a Ctx.
func OptionsOf[T any](c Ctx, o RunOptions[T]) T { return read[T](c, "options", nil) }

// Pred is a data-only option predicate (never a closure).
type Pred struct {
	field, value string
	all          []Pred
	not          bool
}

// OptionField is a typed handle to one field of a definition's run options:
// wf.Field(opts, func(o *Options) *bool { return &o.Autonomous }).
//
// Owns: recovering the field name once at definition time, building Pred
// values (Is) and reading the value from a Ctx (Of).
// Does not: use unsafe, cache anything globally, or panic on a bad selector (a
// pointer that is not a direct exported field is a RuleBuild problem).
// Extend by: a new option is a struct field plus a Field handle; callers never
// name option fields by string.
type OptionField[T any, V comparable] struct{ name string }

// Field builds the handle for the field sel returns. sel runs once, here, on a
// framework-owned zero T; render, hash and lint never run it.
func Field[T any, V comparable](o RunOptions[T], sel func(*T) *V) OptionField[T, V] {
	name, err := optionFieldName(sel)
	if err != nil {
		o.d.misuse(fmt.Sprintf("wf.Field: %v", err))
	}
	return OptionField[T, V]{name: name}
}

// optionFieldName matches the pointer sel returns against the addresses of T's
// exported fields. The type is compared too: a struct's first field shares its
// address with that field's own first field.
func optionFieldName[T any, V comparable](sel func(*T) *V) (name string, err error) {
	defer func() {
		if r := recover(); r != nil {
			name, err = "", fmt.Errorf("selector panicked: %v", r)
		}
	}()
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		return "", fmt.Errorf("options type %s is not a struct", t)
	}
	if sel == nil {
		return "", fmt.Errorf("selector is nil")
	}
	zero := new(T)
	p := sel(zero)
	if p == nil {
		return "", fmt.Errorf("selector returned nil")
	}
	addr, want := reflect.ValueOf(p).Pointer(), reflect.TypeFor[V]()
	v := reflect.ValueOf(zero).Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() && f.Type == want && v.Field(i).Addr().Pointer() == addr {
			return f.Name, nil
		}
	}
	return "", fmt.Errorf("selector does not return a direct exported field of %s", t)
}

// Is matches when the field equals v.
func (f OptionField[T, V]) Is(v V) Pred { return Pred{field: f.name, value: fmt.Sprint(v)} }

// Of reads the field's value from the run's options; a broken handle reads the
// zero value (its definition does not lint, so it never runs).
func (f OptionField[T, V]) Of(c Ctx) V {
	var zero V
	if f.name == "" {
		return zero
	}
	v, _ := reflect.ValueOf(read[T](c, "options", nil)).FieldByName(f.name).Interface().(V)
	return v
}

// And conjoins two predicates.
func (p Pred) And(q Pred) Pred { return Pred{all: []Pred{p, q}} }

// Not negates a predicate.
func Not(p Pred) Pred { return Pred{all: []Pred{p}, not: true} }

// String renders the predicate canonically, since it feeds Hash: nested And
// operands are flattened (And is associative) and a Not of a composite is
// parenthesised, so Not(A.And(B)) is "not (A and B)" and Not(A).And(B) is
// "not A and B".
func (p Pred) String() string {
	switch {
	case p.not && len(p.all) == 1:
		q := p.all[0]
		if len(q.all) > 0 && !q.not {
			return "not (" + q.String() + ")"
		}
		return "not " + q.String()
	case len(p.all) > 0:
		return strings.Join(p.conjuncts(nil), " and ")
	}
	return p.field + "=" + p.value
}

// conjuncts flattens nested And operands into their rendered leaves.
func (p Pred) conjuncts(out []string) []string {
	for _, q := range p.all {
		if len(q.all) > 0 && !q.not {
			out = q.conjuncts(out)
		} else {
			out = append(out, q.String())
		}
	}
	return out
}

// PolicyRule is one AutonomyPolicy entry: (gate, answer, when, park note).
type PolicyRule struct {
	gate   *node
	answer string
	when   Pred
	park   string
}

// PolicyAnswer answers a policy-answerable gate with the given answer when the
// predicate matches.
func PolicyAnswer[S any, A comparable](g *Gate[S, A], answer A, when Pred) PolicyRule {
	return PolicyRule{gate: g.n, answer: fmt.Sprint(answer), when: when}
}

// Park records the road not taken into the run's parked list.
func (r PolicyRule) Park(note string) PolicyRule {
	r.park = note
	return r
}

// AutonomyPolicy is a declarative, data-only rule set attached to the Def: an
// ordered list of rules (gate, answer, when, park note); the first rule whose
// predicate matches the run's options answers the gate.
//
// Owns: what answers a policy-answerable gate when the run is autonomous. Its
// inputs are only the run options and the gate's identity; it reads no context
// and is never a closure, so render can show the policy beside each gate and
// lint can check it. No matching rule means the gate waits for a human; each
// answer is journaled as auto_answered with the rule used.
// Does not: answer a reserved gate (lint RuleReservedPolicy) or decide
// anything beyond the gate's declared answer set.
// Extend by: a new rule is an entry in the Def's policy.
type AutonomyPolicy struct{ rules []PolicyRule }

// Policy builds an ordered policy; first match wins.
func Policy(rules ...PolicyRule) AutonomyPolicy { return AutonomyPolicy{rules: rules} }

// SetPolicy attaches the policy to the definition.
func (d *Def) SetPolicy(p AutonomyPolicy) *Def {
	d.policy = p
	return d
}
