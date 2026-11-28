package wf

import (
	"reflect"
	"strings"
)

// ruleAgentOutputPath checks declarations, never prompt prose or Go comments.
// Runtime validation catches run roots in otherwise ordinary string results.
func ruleAgentOutputPath(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		k, ok := n.kind.(*agentKind)
		if !ok {
			continue
		}
		bindings := map[string]bool{}
		for _, input := range k.prompt.inputs {
			in := input.inputRef()
			if in.kind != "bound" {
				continue
			}
			artifactBound := false
			for _, token := range bindPattern.FindAllStringSubmatch(in.tmpl, -1) {
				artifactBound = artifactBound || strings.HasPrefix(token[1], artifactToken)
			}
			if !artifactBound {
				continue
			}
			name := in.name
			if in.alias != "" {
				name = in.alias
			}
			bindings[name] = true
		}
		active := map[reflect.Type]bool{}
		var walk func(reflect.Type, string)
		walk = func(t reflect.Type, path string) {
			if t == nil || active[t] {
				return
			}
			pathType := t
			for pathType.Kind() == reflect.Pointer {
				pathType = pathType.Elem()
			}
			if pathType == reflect.TypeFor[RunPath]() {
				out = append(out, n.problem(RuleAgentOutputPath, "output field %s has type RunPath: use the engine-recorded artifact instead of returning its path", path))
				return
			}
			if ownEncoding(t) {
				return
			}
			active[t] = true
			defer delete(active, t)
			switch t.Kind() {
			case reflect.Pointer:
				walk(t.Elem(), path)
			case reflect.Slice, reflect.Array:
				walk(t.Elem(), path+"[]")
			case reflect.Map:
				walk(t.Elem(), path+".*")
			case reflect.Struct:
				for _, f := range jsonFields(t) {
					field := f.name
					if path != "$" {
						field = path + "." + field
					}
					if bindings[field] {
						out = append(out, n.problem(RuleAgentOutputPath, "output field %s matches an ArtifactPath prompt binding: use the engine-recorded artifact instead of returning its path", field))
					}
					walk(f.field.Type, field)
				}
			}
		}
		walk(k.outT, "$")
	}
	return out
}
