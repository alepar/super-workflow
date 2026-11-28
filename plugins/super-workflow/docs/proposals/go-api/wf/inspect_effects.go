package wf

// EffectKind identifies an adapter operation without formatting or binding args.
type EffectKind struct{ Adapter, Op string }

// EffectKinds lists the distinct external operations a state can invoke, in
// declaration order. Commands include shell.run; edge uses include their
// attached effects. Counters are engine operations and are not adapter calls.
// Like other executable inspection methods, it requires a view from Inspect.
func (s StateView) EffectKinds() []EffectKind {
	return s.effectKinds(true)
}

func (s StateView) effectKinds(command bool) []EffectKind {
	var out []EffectKind
	seen := map[EffectKind]bool{}
	add := func(kind EffectKind) {
		if !seen[kind] {
			seen[kind] = true
			out = append(out, kind)
		}
	}
	if command && s.Kind == "command" {
		add(EffectKind{Adapter: "shell", Op: "run"})
	}
	for _, use := range s.uses {
		for _, effect := range use.effects {
			if external, ok := effect.(ExternalEffect); ok {
				add(EffectKind{Adapter: external.Adapter, Op: external.Op})
			}
		}
	}
	if s.node != nil {
		for _, edge := range s.node.edges {
			if edge.when == nil || s.node.routable(edge) {
				continue
			}
			for _, effect := range edge.effects {
				if external, ok := effect.(ExternalEffect); ok {
					add(EffectKind{Adapter: external.Adapter, Op: external.Op})
				}
			}
		}
	}

	return out
}

// DefaultEdgeInapplicable explains a structural impossibility for a definition
// default. Explicit state routes remain author-owned coverage obligations.
// This is inspection metadata only: it changes neither routing nor hashing.
func (s StateView) DefaultEdgeInapplicable(index int) string {
	if s.node == nil || index < 0 || index >= len(s.node.edges) {
		return ""
	}
	edge := s.node.edges[index]
	use, ok := s.node.defaults[edge.class]
	if !ok || use.e != edge {
		return ""
	}
	hasEdges := len(s.effectKinds(false)) > 0
	switch edge.class {
	case Uncertain:
		if hasEdges {
			return ""
		}
		if k, ok := s.node.kind.(*commandKind); ok {
			if !k.idempotent {
				return ""
			}
			return "idempotent command retries its open intent; no edge effect can become uncertain"
		}
		return "no command or edge effect can leave an unresolved intent"
	case Stale:
		if k, ok := s.node.kind.(*agentKind); ok && (len(k.observes) > 0 || len(k.inputs) > 0) {
			return ""
		}
		return "no agent observation snapshot can change between issue and submission"
	case Unavailable:
		if hasEdges {
			return ""
		}
		switch k := s.node.kind.(type) {
		case *gateKind:
			return "gate has no adapter operation that can be unavailable"
		case *mapKind:
			return "map source failures take join.partial; child failures are settlement data; no external edge effects"
		case *agentKind:
			// Any check can return ErrUnavailable, even without declaring
			// observations. This also excludes dynamic ObservesFrom checks.
			p := k.prompt
			if len(k.checkNames) == 0 && len(k.observes) == 0 && len(k.inputs) == 0 &&
				promptInputsArePure(p.inputs) && p.path == "" && p.item == nil && p.sel == nil &&
				len(p.candidates) == 0 && s.node.doneWhen == nil {
				return "inline agent has only pure prompt inputs and no checks, observations, postcondition or external edge effects"
			}
		}
	case Blocked:
		if k, ok := s.node.kind.(*gateKind); ok && k.reservation == Reserved && !hasEdges && s.totalStaticGateAnswers(k) {
			return "reserved gate has total static unguarded answers and no external effects"
		}
	}
	return ""
}

// Gate answers still run the case router even when policy execution is
// disabled. Only a total table of unguarded static routes cannot be blocked.
func (s StateView) totalStaticGateAnswers(k *gateKind) bool {
	table := s.node.cases
	if table == nil || k.answers == nil {
		return false
	}
	covered := map[string]bool{}
	for _, row := range table.cases {
		if row.hasRoute || row.e.e == nil || row.e.e.when != nil {
			return false
		}
		covered[row.value] = true
	}
	for _, answer := range k.answers.values {
		if !covered[answer] {
			return false
		}
	}
	return true
}

// ObservationKinds lists statically declared observation operations, including
// checks and DoneWhen, without executing closures or parsing display text.
// ObservesFrom declarations depend on submissions and are not enumerated.
func (s StateView) ObservationKinds() []EffectKind {
	if s.node == nil {
		return nil
	}
	var out []EffectKind
	seen := map[EffectKind]bool{}
	add := func(ds ...ObservationDesc) {
		for _, d := range ds {
			k := EffectKind{Adapter: d.Adapter, Op: d.Op}
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	switch k := s.node.kind.(type) {
	case *agentKind:
		add(k.observes...)
		add(k.inputs...)
	case *computeKind:
		add(k.observes...)
	case *subKind:
		add(k.observes...)
	case *waitKind:
		add(k.obs)
	case *mapKind:
		if k.src.obs != nil {
			add(*k.src.obs)
		}
	}
	if s.node.doneWhen != nil {
		add(s.node.doneWhen.obs)
	}
	return out
}

// promptInputsArePure mirrors the journal-only cases in scope.inputValues.
// Aliases retain the underlying descriptor kind. Unknown kinds and artifacts
// stay applicable; bound templates may resolve filesystem-backed builtins.
func promptInputsArePure(inputs []Input) bool {
	for _, input := range inputs {
		d := input.inputRef()
		switch d.kind {
		case "param", "out", "child-input":
		case "bound":
			if bindPattern.MatchString(d.tmpl) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
