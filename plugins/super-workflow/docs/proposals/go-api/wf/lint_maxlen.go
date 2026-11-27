package wf

import (
	"fmt"
	"reflect"
)

// ruleMaxlenTag is RuleMaxlenTag: a maxlen tag, anywhere in a type the engine
// checks tags on (an agent's or a compute's output, the definition's input, a
// yield's result; maxlen itself is enforced only on an agent's output), whose
// value is not a positive whole number or whose field is not a string, a
// *string or a list of strings with no JSON encoding of their own. It is refused here, before any run, so the check and the shown
// schema never meet a bad tag.
func ruleMaxlenTag(c *lintCtx) []Problem {
	var out []Problem
	type checked struct {
		n    *node // nil: the definition's input
		what string
		t    reflect.Type
	}
	types := []checked{{nil, "input", c.d.input}}
	for _, n := range c.d.nodes {
		switch k := n.kind.(type) {
		case *agentKind:
			types = append(types, checked{n, "output", k.outT})
		case *computeKind:
			types = append(types, checked{n, "computed", k.outT})
		case *terminalKind:
			types = append(types, checked{n, "result", k.result})
		}
	}
	for _, ty := range types {
		t, n, what := ty.t, ty.n, ty.what
		if t == nil {
			continue
		}
		problem := func(format string, args ...any) {
			if n == nil {
				out = append(out, Problem{Rule: RuleMaxlenTag, Path: c.d.name, Msg: fmt.Sprintf(format, args...)})
				return
			}
			out = append(out, n.problem(RuleMaxlenTag, format, args...))
		}
		active := map[reflect.Type]bool{}
		var walk func(reflect.Type, string)
		walk = func(t reflect.Type, path string) {
			for t != nil && t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			if t == nil || active[t] || ownEncoding(t) {
				return
			}
			active[t] = true
			defer delete(active, t)
			switch t.Kind() {
			case reflect.Slice, reflect.Array, reflect.Map:
				walk(t.Elem(), path+"[]")
			case reflect.Struct:
				for _, f := range jsonFields(t) {
					field := path + "." + f.name
					if _, tagged, err := maxlenOf(f.field.Tag); err != nil {
						problem("%s field %s: %v", what, field, err)
					} else if tagged && !maxlenApplies(f.field.Type) {
						problem("%s field %s has a maxlen tag but is %s: it applies to a string, a *string or a list of strings without their own JSON encoding", what, field, f.field.Type)
					}
					walk(f.field.Type, field)
				}
			}
		}
		walk(t, "$")
	}
	return out
}

// maxlenApplies reports whether a maxlen tag can cap a field of type t.
func maxlenApplies(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if ownEncoding(t) {
		return false // the schema shows such a field as {}, with no cap to see
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	return t.Kind() == reflect.String && !ownEncoding(t)
}
