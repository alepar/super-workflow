package wf

import (
	"fmt"
	"maps"
	"reflect"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"
)

// The exec rule (reference-workflow rule S3a): no agent output is ever
// executed. A command's argv comes from the definition or from parameters a
// person supplied; a value from anywhere else is allowed in argv only when
// its type is validated, so it cannot be read as a program, a script or an
// option.
//
// A {name} placeholder in a shell or command argv binds, in the engine's
// order, a run param, a builtin, or else a field of that name (matched
// without regard to case) of the instance's or an ancestor's input, of any
// accepted or computed output, or of a child's result: whichever was stored
// last. So every such field can bind it, not only the one the definition
// means. The placeholder is clean only when every one of them has a
// validated type: an integer, a bool, a registered enum, or a string with an
// anchored pattern:"^...$" tag, which the engine checks when an agent's
// result is decoded and when a compute returns its value. Otherwise it is
// tainted, whatever its source: a compute or a child input may carry an
// agent's text. Free text goes on stdin or in a file. Typed adapters (bd,
// git) build their own argv and are not checked.
//
// It is not in rules: LintExec reports it.
const RuleExecTainted RuleID = "exec-tainted"

// RuleExecUnbound: a {name} in a command's or a read's argv that no param,
// builtin or stored field can ever bind. The engine binds every such {name} when the
// state runs, so the run halts there on an unresolved placeholder. It is
// usually script source passed inline (JavaScript destructuring, a ${var}
// expansion) that the binder reads as a placeholder; put the script in a file.
// LintExec reports it.
const RuleExecUnbound RuleID = "exec-unbound"

// execBuiltins are the names lookup binds without a param or a stored field.
var execBuiltins = map[string]bool{
	"run": true, "workspace": true, "harness": true, "resolvedModels": true,
	"assets": true, "skillsRoot": true, workdirName: true, recordsName: true,
	scratchName: true, blockedCauseName: true, predecessorName: true,
}

// LintExec is the exec rule over d and its children. params names the run
// params the served entry supplies beyond those d and its children declare
// with Param (a Registration's ParamSpecs): each binds like a declared one.
func (d *Def) LintExec(params ...string) []Problem {
	entry := map[string]bool{}
	for _, p := range params {
		entry[p] = true
	}
	exec := func(c *lintCtx) []Problem { return ruleExecArgv(c, entry) }
	return lintDef(d, "", map[*Def]bool{}, []rule{exec}, false)
}

// execBinder is one stored value type that can bind a placeholder.
type execBinder struct {
	where string // "<def>/<state> output", "<def> input", "<def> child <child> result"
	t     reflect.Type
}

// execBinders is every type whose fields a placeholder of c.d can bind.
func execBinders(c *lintCtx) []execBinder {
	var out []execBinder
	for _, d := range append(slices.Clone(c.ancestors), c.d) {
		if d.input != nil {
			out = append(out, execBinder{d.name + " input", d.input})
		}
		for _, n := range d.nodes {
			switch k := n.kind.(type) {
			case *agentKind:
				out = append(out, execBinder{d.name + "/" + n.name + " output", k.outT})
			case *computeKind:
				out = append(out, execBinder{d.name + "/" + n.name + " computed", k.outT})
			}
		}
		for _, n := range d.nodes {
			switch n.kind.(type) {
			case *commandKind:
				// A command's result is stored: its stdout and stderr are text.
				out = append(out, execBinder{d.name + "/" + n.name + " command result", reflect.TypeFor[RunResult]()})
			case *mapKind:
				out = append(out, execBinder{d.name + "/" + n.name + " map join", reflect.TypeFor[struct {
					SourceError string `json:"sourceError"`
				}]()})
			}
		}
		for _, ch := range d.children {
			if ch != nil && ch.result != nil {
				out = append(out, execBinder{d.name + " child " + ch.name + " result", ch.result})
			}
		}
	}
	return out
}

// How a stored type binds a placeholder path, as lookupDoc walks it.
const (
	bindsNot     = iota // nothing at that path binds (no such field, a list, an object)
	bindsClean          // a validated scalar
	bindsTainted        // a scalar of no validated type, or a value whose shape is not known
)

// binding is how a stored value of type t binds the dotted path. lookupDoc
// descends objects by key (a struct's field, matched exactly or without
// regard to case, or any key of a map) and binds a string, a number or a
// bool at the end. A map binds any key, an interface or a type with its own
// encoding may hold anything: those are tainted unless typed.
func binding(t reflect.Type, path []string, tag reflect.StructTag, enums []*enumData) int {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if ownEncoding(t) || t.Kind() == reflect.Interface {
		return bindsTainted
	}
	if len(path) == 0 {
		switch t.Kind() {
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
			return bindsClean
		case reflect.String:
			if enumFor(t, enums) != nil || validPattern(tag) {
				return bindsClean
			}
			return bindsTainted
		}
		return bindsNot
	}
	switch t.Kind() {
	case reflect.Struct:
		fs := jsonFields(t)
		i := slices.IndexFunc(fs, func(x jsonField) bool { return x.name == path[0] })
		if i < 0 {
			i = slices.IndexFunc(fs, func(x jsonField) bool { return strings.EqualFold(x.name, path[0]) })
		}
		if i < 0 {
			return bindsNot
		}
		return binding(fs[i].field.Type, path[1:], fs[i].field.Tag, enums)
	case reflect.Map:
		return binding(t.Elem(), path[1:], "", enums)
	}
	return bindsNot
}

// validPattern reports whether a pattern tag validates a string for argv: no
// string the whole pattern matches can begin with '-', so the value is never
// read as an option. Decided on the pattern's syntax, not by samples.
func validPattern(tag reflect.StructTag) bool {
	p, ok := tag.Lookup("pattern")
	if !ok {
		return false
	}
	// The value is checked against the whole pattern (^(?:p)$), so that is
	// the expression whose first character is decided.
	re, err := syntax.Parse(`^(?:`+p+`)$`, syntax.Perl)
	if err != nil {
		return false
	}
	dash, _ := mayStartWith(re.Simplify(), '-')
	return !dash
}

// mayStartWith reports whether a string re matches can begin with c, and
// whether re can match the empty string (so what follows it may begin it).
func mayStartWith(re *syntax.Regexp, c rune) (may, nullable bool) {
	switch re.Op {
	case syntax.OpNoMatch:
		return false, false
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return false, true
	case syntax.OpLiteral:
		if len(re.Rune) == 0 {
			return false, true
		}
		r := re.Rune[0]
		return r == c || (re.Flags&syntax.FoldCase != 0 && unicode.SimpleFold(r) == c), false
	case syntax.OpCharClass:
		for i := 0; i+1 < len(re.Rune); i += 2 {
			if re.Rune[i] <= c && c <= re.Rune[i+1] {
				return true, false
			}
		}
		return false, false
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return true, false
	case syntax.OpCapture:
		return mayStartWith(re.Sub[0], c)
	case syntax.OpStar, syntax.OpQuest:
		m, _ := mayStartWith(re.Sub[0], c)
		return m, true
	case syntax.OpPlus:
		return mayStartWith(re.Sub[0], c)
	case syntax.OpRepeat:
		m, n := mayStartWith(re.Sub[0], c)
		return m, n || re.Min == 0
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			m, n := mayStartWith(sub, c)
			if m {
				return true, false
			}
			if !n {
				return false, false
			}
		}
		return false, true
	case syntax.OpAlternate:
		var may, null bool
		for _, sub := range re.Sub {
			m, n := mayStartWith(sub, c)
			may, null = may || m, null || n
		}
		return may, null
	}
	return true, true // an op this does not know: assume the worst
}

// argvOf is the argv of a shell effect, nil for any other effect.
func argvOf(ef Effect) []string {
	x, ok := ef.(ExternalEffect)
	if !ok || x.Adapter != "shell" || x.Op != "run" {
		return nil
	}
	switch a := x.Args["argv"].(type) {
	case []string:
		return a
	case []any:
		var out []string
		for _, v := range a {
			s, _ := v.(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}

// nodeReads is every observation a state declares: its own reads, a check's
// and the staleness token's, a wait's condition, a Map's item source and a
// DoneWhen.
func nodeReads(n *node) []ObservationDesc {
	var out []ObservationDesc
	switch k := n.kind.(type) {
	case *agentKind:
		out = append(append(out, k.observes...), k.inputs...)
	case *computeKind:
		out = append(out, k.observes...)
	case *subKind:
		out = append(out, k.observes...)
	case *waitKind:
		out = append(out, k.obs)
	case *mapKind:
		if k.src.obs != nil {
			out = append(out, *k.src.obs)
		}
	}
	if n.doneWhen != nil {
		out = append(out, n.doneWhen.obs)
	}
	return out
}

// argvPosition names where argv[i] is used: the program, a shell script, or
// an argument.
func argvPosition(argv []string, i int) string {
	switch {
	case i == 0:
		return "program"
	case argv[i-1] == "-c":
		return "script"
	}
	return "argument"
}

func ruleExecArgv(c *lintCtx, entry map[string]bool) []Problem {
	binders := execBinders(c)
	var enums []*enumData
	for _, d := range append(slices.Clone(c.ancestors), c.d) {
		enums = append(enums, d.enums...)
	}
	params := maps.Clone(entry)
	for _, d := range append(slices.Clone(c.ancestors), c.d) {
		for _, p := range d.params {
			params[p.name] = true
		}
	}
	var out []Problem
	seen := map[string]bool{}
	// bound: argv is bound as declared when the state runs (a command, a
	// read), so a placeholder nothing binds is certain to halt it. An edge's
	// declared effects may stand for effects a route attaches at run time with
	// literal args, so only their taint is checked.
	check := func(path, where string, argv []string, bound bool) {
		for i, a := range argv {
			for _, m := range bindPattern.FindAllStringSubmatch(a, -1) {
				name := m[1]
				// A param binds first; a builtin binds only when no stored
				// value has a field of its name (lookup reads those first), so
				// it is checked like any other name.
				if params[name] || strings.HasPrefix(name, artifactToken) {
					continue
				}
				var tainted []string
				binds := execBuiltins[name]
				for _, b := range binders {
					switch binding(b.t, strings.Split(name, "."), "", enums) {
					case bindsTainted:
						tainted = append(tainted, b.where)
						binds = true
					case bindsClean:
						binds = true
					}
				}
				var msg string
				switch {
				case !binds && bound:
					msg = fmt.Sprintf("%s · {%s} (%s) binds no param, builtin or stored field: the run halts here; pass script text in a file, not inline", where, name, argvPosition(argv, i))
				case len(tainted) > 0:
					msg = fmt.Sprintf("%s · {%s} (%s) can bind an unvalidated value: %s", where, name, argvPosition(argv, i), strings.Join(tainted, ", "))
				default:
					continue
				}
				key := path + "|" + where + "|" + name
				if seen[key] {
					continue
				}
				seen[key] = true
				rule := RuleExecTainted
				if !binds {
					rule = RuleExecUnbound
				}
				out = append(out, Problem{Rule: rule, Path: path, Msg: msg})
			}
		}
	}
	for i, n := range c.d.nodes {
		v := c.states[i]
		if k, ok := n.kind.(*commandKind); ok {
			check(v.Path, "command", k.argv, true)
		}
		// A read runs its argv the same way an effect does.
		for _, d := range nodeReads(n) {
			if d.Adapter == "shell" && d.Op == "run" {
				check(v.Path, "read", argvOf(ExternalEffect{Adapter: "shell", Op: "run", Args: d.Args}), true)
			}
		}
		for _, u := range v.uses {
			for _, ef := range u.effects {
				if argv := argvOf(ef); argv != nil {
					check(v.Path, fmt.Sprintf("edge %q effect", useLabel(u)), argv, false)
				}
			}
		}
	}
	return out
}
