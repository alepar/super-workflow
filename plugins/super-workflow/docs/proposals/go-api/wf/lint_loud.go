package wf

import (
	"reflect"
	"strings"
)

// ruleExplicitPromptOutputs refuses the two silent Out bindings the prototype
// hit. Named inputs win; remaining outputs must be bound explicitly with As.
// promptTexts reads only declared candidates; selectors and closures never run.
// Unbound parameterized paths cannot be inspected before the run supplies them.
func promptTexts(p Prompt) []string {
	if p.path == "" && p.item == nil {
		return []string{p.text}
	}
	refs := p.candidates
	if p.item == nil {
		refs = []string{p.path}
	}
	var out []string
	for _, name := range refs {
		ref, err := ParsePromptRef(name)
		if err != nil {
			continue
		}
		text, err := ref.Resolve(nil, ".")
		if err == nil {
			out = append(out, text)
		}
	}
	return out
}

func ruleExplicitPromptOutputs(c *lintCtx) []Problem {
	var out []Problem
	scope := append(append([]*Def{}, c.ancestors...), c.d)
	for _, n := range c.d.nodes {
		k, ok := n.kind.(*agentKind)
		if !ok {
			continue
		}
		named := map[string]bool{}
		hasOut := false
		for _, in := range k.prompt.inputs {
			d := in.inputRef()
			name := d.name
			if d.alias != "" {
				name = d.alias
			}
			named[strings.ToLower(name)] = true
			hasOut = hasOut || d.kind == "out"
		}
		if !hasOut {
			continue
		}
		for _, text := range promptTexts(k.prompt) {
			for _, m := range promptPattern.FindAllStringSubmatch(text, -1) {
				name := m[1]
				if named[strings.ToLower(name)] {
					continue
				}
				param := false
				for _, d := range scope {
					for _, p := range d.params {
						param = param || p.name == name
					}
				}
				if param {
					out = append(out, n.problem(RuleInputUnresolved, "prompt {{%s}} matches a run parameter while Out is declared: bind the intended input explicitly with wf.As", name))
					continue
				}
				if scopeBinds(scope, n, name) {
					continue
				}
				out = append(out, n.problem(RuleInputUnresolved, "prompt {{%s}} has no guaranteed scope binding and may use the positional Out fallback: name the output with wf.As", name))
			}
		}
	}
	return out
}

type outputTyper interface{ outputType() reflect.Type }

func scopeBinds(defs []*Def, consumer *node, name string) bool {
	switch name {
	case "run", "workspace", "harness", "resolvedModels", recordsName, scratchName, blockedCauseName, predecessorName:
		return true
	}
	d := consumer.def
	if typeBinds(d.input, name) {
		return true
	}
	for _, n := range d.nodes {
		if !outputBefore(n, consumer) {
			continue
		}
		var ops kindOps
		switch k := n.kind.(type) {
		case *agentKind:
			ops = k.ops
		case *commandKind:
			ops = k.ops
		case *computeKind:
			ops = k.ops
		case *subKind:
			ops = k.ops
		case *mapKind:
			ops = k.ops
		}
		if typed, ok := ops.(outputTyper); ok && typeBinds(typed.outputType(), name) {
			return true
		}
	}
	if len(defs) < 2 {
		return false
	}
	parent := defs[len(defs)-2]
	entries := 0
	for _, entry := range parent.nodes {
		var child *Def
		switch k := entry.kind.(type) {
		case *subKind:
			child = k.child
		case *mapKind:
			child = k.child
		}
		if child != d {
			continue
		}
		entries++
		if !scopeBinds(defs[:len(defs)-1], entry, name) {
			return false
		}
	}
	return entries > 0
}

// typeBinds proves a scalar JSON field exists. Optional fields, nullable
// embeddings and dynamic maps cannot promise a binding; use an explicit input.
func typeBinds(t reflect.Type, name string) bool {
	if t == nil {
		return false
	}
	for _, part := range strings.Split(name, ".") {
		if t.Kind() != reflect.Struct || ownEncoding(t) {
			return false
		}
		fields := jsonFields(t)
		var found *jsonField
		for i := range fields {
			if fields[i].name == part {
				found = &fields[i]
				break
			}
		}
		if found == nil {
			for i := range fields {
				if strings.EqualFold(fields[i].name, part) {
					found = &fields[i]
					break
				}
			}
		}
		if found == nil || found.optional {
			return false
		}
		fieldType := t
		for _, index := range found.index {
			if fieldType.Kind() == reflect.Pointer {
				return false
			}
			fieldType = fieldType.Field(index).Type
		}
		t = fieldType
	}
	if ownEncoding(t) {
		return false
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func ruleFreeRouteEnum(c *lintCtx) []Problem {
	var out []Problem
	for _, n := range c.d.nodes {
		k, ok := n.kind.(*agentKind)
		if !ok || n.route == nil || n.cases != nil || len(k.checkNames) > 0 {
			continue
		}
		typed := k.ops.(interface{ outputType() reflect.Type })
		if hasEnum(typed.outputType(), c.d.enums, map[reflect.Type]bool{}) {
			out = append(out, n.problem(RuleBuild, "free Route output contains a registered enum but has no check: declare a Checkf or use RouteCases"))
		}
	}
	return out
}

func outputBefore(producer, consumer *node) bool {
	if producer == consumer || producer.def != consumer.def || !reach(producer)[consumer] {
		return false
	}
	reachesConsumer := func(u edgeUse) bool {
		return u.e != nil && u.e.to != nil && (u.e.to == consumer || reach(u.e.to)[consumer])
	}
	if r := retryOf(producer); r != nil {
		if reachesConsumer(r.exhausted) {
			return false
		}
		if r.stalled != nil && reachesConsumer(*r.stalled) {
			return false
		}
		for _, u := range r.byClass {
			if reachesConsumer(u) {
				return false
			}
		}
	}
	if producer.maxVisits != nil && reachesConsumer(producer.maxVisits.over) {
		return false
	}
	for _, e := range producer.edges {
		if e.when != nil && reachesConsumer(edgeUse{e: e}) {
			return false
		}
	}
	seen := map[*node]bool{}
	todo := []*node{consumer.def.nodes[0]}
	for len(todo) > 0 {
		n := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if n == producer || seen[n] {
			continue
		}
		if n == consumer {
			return false
		}
		seen[n] = true
		todo = append(todo, succs(n)...)
	}
	return true
}
