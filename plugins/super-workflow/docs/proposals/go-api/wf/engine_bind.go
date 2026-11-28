package wf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// ErrUnresolved marks a {name} placeholder with no value in scope; the step is
// classed Blocked.
var ErrUnresolved = errors.New("unresolved placeholder")

var (
	bindPattern   = regexp.MustCompile(`\{([A-Za-z][A-Za-z0-9_.-]*)\}`)
	promptPattern = regexp.MustCompile(`\{\{\s*([A-Za-z][A-Za-z0-9_.-]*)\s*\}\}`)
)

// scope resolves a placeholder name for one instance. The engine's own names
// come first and nothing shadows them: "assets", {artifact.<name>} and
// "scratch" (see scratchDir). Then run params, then "records" (see
// recordsDir), which no document field shadows, then the top-level fields of the instance's input and its ancestors' inputs, then the
// scalar fields of accepted outputs, computed outputs and child results in
// this instance and its ancestors (latest first), then the builtins "run",
// "workspace" (the absolute workspace root), "harness" (the run's resolved
// harness, unbound on a journal that recorded none), "resolvedModels" (the
// journaled tier -> model map as JSON, empty when it resolves no model),
// "blockedCause" (see blockedCause) and "predecessor" (see predecessorName).
// The name "workdir" is reserved for the directory a run's steps resolve
// against once a state has bound it: agents, commands and workdir-relative
// reads use it, and unbound it is the workspace root.
type scope struct {
	c  *cycle
	in *instance
}

func (c *cycle) scope(in *instance) scope { return scope{c: c, in: in} }

// workdirName is the reserved placeholder that names the directory the run's
// steps resolve against after a state binds it.
const workdirName = "workdir"

// workdirBound is the bound {workdir} for in: ok is false before a state has
// bound it, and in any definition that binds nothing.
// workdirRefused is why in's bound workdir cannot be used in this fork (it
// names a source root: checkForkValue), or nil.
func (c *cycle) workdirRefused(in *instance) error {
	w, ok := c.workdirBound(in)
	if !ok {
		return nil
	}
	return c.checkForkValue(w, "workdir")
}

func (c *cycle) workdirBound(in *instance) (string, bool) {
	v, ok := c.scope(in).lookup(workdirName)
	return v, ok && v != ""
}

// workdir is the directory in's agent works in and artifacts resolve against:
// the workspace root joined with the bound {workdir} (an absolute value is used
// as is), or the workspace root when unbound.
func (c *cycle) workdir(in *instance) string {
	w, ok := c.workdirBound(in)
	switch {
	case !ok:
		return c.workspace()
	case filepath.IsAbs(w):
		return w
	}
	return filepath.Join(c.workspace(), w)
}

// chain lists the instance and its ancestors, nearest first.
func (s scope) chain() []*instance {
	var out []*instance
	for i := s.in; i != nil; i = s.c.st.insts[i.Parent] {
		out = append(out, i)
		if i.ID == "" {
			break
		}
	}
	return out
}

func (s scope) lookup(name string) (string, bool) {
	if name == "skillsRoot" && s.c.e != nil && s.c.e.opts.Assets != "" {
		return effectiveAssets(s.in.def, s.c.e.opts.Assets), true
	}
	if name == "assets" {
		if s.c.e != nil && s.c.e.opts.Assets != "" {
			abs, err := filepath.Abs(s.c.e.opts.Assets)
			return abs, err == nil
		}
		if s.c.st.started != nil {
			chain := s.chain()
			names := make([]string, len(chain))
			for i, in := range chain {
				names[len(chain)-1-i] = in.def.name
			}
			if root := s.c.st.started.AssetRoots[strings.Join(names, "/")]; root != "" {
				return root, true
			}
			if len(s.c.st.started.AssetRoots) == 0 && s.c.st.started.Assets != "" {
				return s.c.st.started.Assets, true
			}
		}
		for _, in := range s.chain() {
			if in.def != nil && in.def.assets != "" {
				abs, err := filepath.Abs(in.def.assets)
				return abs, err == nil
			}
		}
		return "", false
	}
	if an, ok := strings.CutPrefix(name, artifactToken); ok && s.in != nil {
		// {artifact.<name>} (ArtifactPath) is the engine's, never a param's or
		// a document field's.
		a := artifactByName(s.in.def, an)
		if a == nil {
			return "", false
		}
		p, err := s.c.artifactPath(s.in, a)
		return p, err == nil
	}
	if name == scratchName {
		// {scratch} is the engine's, never a param's or a document field's.
		if s.in == nil {
			return "", false
		}
		p, err := s.c.scratchDir(s.in)
		return p, err == nil
	}
	if _, ok := s.c.st.started.Params[name]; ok {
		v, err := s.c.paramValue(name)
		return v, err == nil
	}
	if name == recordsName {
		// {records} is never a document field's: a field that copied the
		// root holds the root of the run that recorded it, which in a fork
		// is its source's. A run param still binds it (the caller chose it,
		// and a fork relocates params).
		p, err := s.c.recordsDir()
		return p, err == nil
	}
	if v, ok := s.lookupDocument(name); ok {
		if resolved, err := s.c.resolvePathValues(v); err == nil {
			return scalarDocValue(resolved)
		}
		return "", false
	}
	switch name {
	case "run":
		return string(s.c.run), true
	case "workspace":
		abs, err := filepath.Abs(s.c.workspace())
		return abs, err == nil
	case "harness":
		h := s.c.st.started.Harness
		return h, h != ""
	case "resolvedModels":
		// No resolved models binds empty, never "{}": a definition that
		// passes it as an argument reads absent, not an empty tier table.
		m := s.c.renderModels(s.in).Models
		if len(m) == 0 {
			return "", true
		}
		b, err := json.Marshal(m)
		return string(b), err == nil
	case blockedCauseName:
		return s.in.blockedCause(), true
	case predecessorName:
		return string(s.c.st.started.Predecessor), true
	}
	return "", false
}

// predecessorName is the builtin placeholder for the ended run this one
// succeeds: its run id, and "" for a run that succeeds none. It always binds.
const predecessorName = "predecessor"

// blockedCauseName is the builtin placeholder for the cause of the Blocked
// failure that routed the instance.
const blockedCauseName = "blockedCause"

// blockedCause is the {blockedCause} binding: the reasons of the Blocked
// failure whose OnClass(Blocked) route entered the current state (on that
// edge's own effects, the failure that chose it), and "" when the state was
// entered any other way. It always binds, so one template serves a state that
// is also entered by ordinary routes.
func (in *instance) blockedCause() string {
	if p := in.Pending; p != nil && p.BlockedCause != "" {
		return p.BlockedCause
	}
	return in.BlockedCause
}

// recordsName is the builtin placeholder for the run's records directory.
const recordsName = "records"

// recordsDir is the {records} binding: the absolute <RunDir(run)>/records,
// created on first use. It owns only that path, the run's records root, which
// lives beside the journal, outside any worktree, and as long as the run. It
// does not own what goes in it: client record names, specs and product paths
// are not the engine's, and a client's run records are its own
// records-relative constants composed with {records}. Without a RunDir there
// is no records root, and binding {records} is an error, never a relative path.
func (c *cycle) recordsDir() (string, error) {
	dir := c.resultDir()
	if dir == "" {
		return "", errors.New("{records} needs the engine's RunDir option, which is unset")
	}
	abs, err := filepath.Abs(filepath.Join(dir, "records"))
	if err != nil {
		return "", err
	}
	return abs, os.MkdirAll(abs, 0o755)
}

// unresolved is the ErrUnresolved for placeholder m (with its braces) in
// where, carrying the cause when a builtin knows why it failed to bind.
func (s scope) unresolved(m, where string) error {
	name := strings.Trim(m, "{} ")
	if name == scratchName && s.in != nil {
		if s.c.resultDir() == "" {
			return fmt.Errorf("%w: %s in %s: {scratch} needs the engine's RunDir option, which is unset", ErrUnresolved, m, where)
		}
		if _, err := s.c.scratchDir(s.in); err != nil {
			return fmt.Errorf("%s in %s: %w", m, where, err)
		}
	}
	if name == recordsName {
		if s.c.resultDir() == "" {
			return fmt.Errorf("%w: %s in %s: {records} needs the engine's RunDir option, which is unset", ErrUnresolved, m, where)
		}
		if _, err := s.c.recordsDir(); err != nil {
			// The records directory cannot be made: an environment failure,
			// not a definition error (A3), so not ErrUnresolved.
			return fmt.Errorf("%s in %s: %w", m, where, err)
		}
	}
	return fmt.Errorf("%w: %s in %s", ErrUnresolved, m, where)
}

// lookupDoc finds a (dotted) field path in a JSON object and renders a scalar.
func lookupDocValue(raw json.RawMessage, path string) (any, bool) {
	v, ok := decodeDoc(raw)
	if !ok {
		return "", false
	}
	return walkDoc(v, path)
}

// decodeDoc is raw decoded as a generic JSON value, numbers as written.
func decodeDoc(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	docDecoded()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	return v, true
}

// docDecoded, when set by a test, counts documents decoded.
var docDecoded = func() {}

// docKey names a document's bytes by their identity: a cycle's documents
// are journal data, never changed while it runs.
type docKey struct {
	at *byte
	n  int
}

type docEntry struct {
	v  any
	ok bool
}

// docValue is lookupDocValue with each document decoded once per cycle: a
// cycle that issues a wide fan-out binds hundreds of placeholders against
// the same accepted outputs, and decoding them for every lookup held the
// run lock for seconds. The decoded value is only read.
func (c *cycle) docValue(raw json.RawMessage, path string) (any, bool) {
	if c == nil {
		return lookupDocValue(raw, path)
	}
	if len(raw) == 0 {
		return "", false
	}
	key := docKey{unsafe.SliceData(raw), len(raw)}
	// A compute may resolve placeholders from several goroutines at once.
	c.docsMu.Lock()
	e, cached := c.docs[key]
	if !cached {
		e.v, e.ok = decodeDoc(raw)
		if c.docs == nil {
			c.docs = map[docKey]docEntry{}
		}
		c.docs[key] = e
	}
	c.docsMu.Unlock()
	if !e.ok {
		return "", false
	}
	return walkDoc(e.v, path)
}

// walkDoc follows a dotted path into a decoded document, a key matched
// without case when no key matches exactly.
func walkDoc(v any, path string) (any, bool) {
	for _, seg := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return "", false
		}
		if v, ok = m[seg]; !ok {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			found := false
			for _, k := range keys {
				if strings.EqualFold(k, seg) {
					v, found = m[k], true
					break
				}
			}
			if !found {
				return "", false
			}
		}
	}
	return v, true
}

func lookupDoc(raw json.RawMessage, path string) (string, bool) {
	v, ok := lookupDocValue(raw, path)
	if !ok {
		return "", false
	}
	return scalarDocValue(v)
}

func scalarDocValue(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return fmt.Sprint(x), true
	}
	return "", false
}

// lookupDocument follows the same input/output precedence for scalars and typed paths.
func (s scope) lookupDocument(name string) (any, bool) {
	value, _, _, ok := s.lookupDocumentSource(name)
	return value, ok
}

// lookupDocumentSource also returns the instance and state that supplied
// the selected document. A blank state means it was an instance input.
func (s scope) lookupDocumentSource(name string) (any, *instance, string, bool) {
	usable := func(raw json.RawMessage) (any, bool) {
		v, ok := s.c.docValue(raw, name)
		if !ok {
			return nil, false
		}
		if _, ok, _ := markedRunPath(v); ok {
			return v, true
		}
		if _, ok, _ := markedOwnedPath(v); ok {
			return v, true
		}
		_, ok = scalarDocValue(v)
		return v, ok
	}
	chain := s.chain()
	for _, in := range chain {
		if v, ok := usable(in.Input); ok {
			return v, in, "", true
		}
	}
	for _, in := range chain {
		for i := len(in.Docs) - 1; i >= 0; i-- {
			if v, ok := usable(in.Docs[i].Raw); ok {
				return v, in, in.Docs[i].Name, true
			}
		}
	}
	return nil, nil, "", false
}

// bindString replaces every {name} in s; an unresolved one is ErrUnresolved.
func (s scope) bindString(str string) (string, error) {
	if value, marked, err := s.bindPathTemplate(str); marked || err != nil {
		if err != nil {
			return "", err
		}
		resolved, err := s.c.resolvePathValues(value)
		if err != nil {
			return "", err
		}
		return resolved.(string), nil
	}
	return s.bindPlaceholders(str)
}

func (s scope) bindPlaceholders(str string) (string, error) {
	var missing string
	out := bindPattern.ReplaceAllStringFunc(str, func(m string) string {
		v, ok := s.lookup(m[1 : len(m)-1])
		if !ok && missing == "" {
			missing = m
		}
		return v
	})
	if missing != "" {
		return "", s.unresolved(missing, strconv.Quote(str))
	}
	return out, nil
}

// bindValue binds the strings inside a generic JSON value.
func (s scope) bindValue(v any) (any, error) { return s.bindValueMode(v, false, false, true) }

func (s scope) bindValueMode(v any, preserve, literal, legacyTemplates bool) (any, error) {
	if _, marked, err := markedOwnedPath(v); marked || err != nil {
		if err != nil {
			return nil, err
		}
		if preserve {
			return v, nil
		}
		return s.c.resolvePathValues(v)
	}
	if _, marked, err := markedPathParam(v); marked || err != nil {
		if err != nil {
			return nil, err
		}
		if preserve {
			return v, nil
		}
		return s.c.resolvePathValues(v)
	}
	if _, ok, err := markedRunPath(v); ok || err != nil {
		if err != nil {
			return nil, err
		}
		if preserve {
			return v, nil
		}
		return s.c.resolvePathValues(v)
	}
	switch x := v.(type) {
	case string:
		if literal {
			return x, nil
		}
		if legacyTemplates {
			if value, marked, err := s.bindPathTemplate(x); marked || err != nil {
				if err != nil {
					return nil, err
				}
				if preserve {
					return value, nil
				}
				return s.c.resolvePathValues(value)
			}
		}
		// A new {scratch} token is the engine's directory, never a legacy
		// scratch parameter's or a document field's typed value; a marker
		// already recorded keeps its meaning.
		if preserve && x != "" && bindPattern.FindString(x) == x && x != "{"+scratchName+"}" {
			name := x[1 : len(x)-1]
			_, param := s.c.st.started.Params[name]
			if _, typed := s.c.st.started.ParamPaths[name]; param && typed {
				return map[string]any{pathParamMarker: name}, nil
			}
			if !param && name != "assets" && name != "skillsRoot" && !strings.HasPrefix(name, artifactToken) {
				if raw, ok := s.lookupDocument(name); ok {
					if _, marked, err := markedRunPath(raw); marked || err != nil {
						return raw, err
					}
					if _, marked, err := markedOwnedPath(raw); marked || err != nil {
						return raw, err
					}
				}
			}
		}
		return s.bindPlaceholders(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			b, err := s.bindValueMode(e, preserve, literal, legacyTemplates)
			if err != nil {
				return nil, err
			}
			out[k] = b
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			b, err := s.bindValueMode(e, preserve, literal, legacyTemplates)
			if err != nil {
				return nil, err
			}
			out[i] = b
		}
		return out, nil
	}
	return v, nil
}

// genericArgs turns args into generic JSON values (what a runtime decodes).
func genericArgs(args map[string]any) (map[string]any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

// bindArgs binds a args map; the result holds generic JSON values.
func (s scope) bindArgs(args map[string]any) (map[string]any, error) {
	g, err := genericArgs(args)
	if err != nil || g == nil {
		return nil, err
	}
	b, err := s.bindValue(g)
	if err != nil {
		return nil, err
	}
	return b.(map[string]any), nil
}

// bindArgv preserves whole-argument typed paths in the command's durable
// intent. Ordinary strings retain their existing binding behavior; launch
// resolves explicit path markers against the current run directory.
func (s scope) bindArgv(argv []string) ([]any, error) {
	out := make([]any, len(argv))
	for i, a := range argv {
		b, err := s.bindValueMode(a, true, false, true)
		if err != nil {
			return nil, err
		}
		// ArtifactPath is a typed authoring token too, but the general binder
		// resolves it for observations and external effects. Only command
		// intents preserve run-owned artifact arguments for later launch.
		if strings.HasPrefix(a, "{"+artifactToken) && bindPattern.FindString(a) == a {
			if path, ok := b.(string); ok && filepath.IsAbs(path) && s.c.resultDir() != "" {
				root, err := filepath.Abs(s.c.resultDir())
				if err != nil {
					return nil, err
				}
				if relative, err := filepath.Rel(root, path); err == nil {
					p := RunPath(filepath.ToSlash(relative))
					if p.validate() == nil {
						b = p
					}
				}
			}
		}
		out[i] = b
	}
	return out, nil
}

// bindDesc binds an observation descriptor: the resolved one is journaled in
// observed, and a closure's Observed matches against it.
func (s scope) bindDesc(d ObservationDesc) (ObservationDesc, error) {
	args, err := s.bindArgs(d.Args)
	if err != nil {
		return d, err
	}
	if args != nil {
		if err := s.c.checkForkValue(args, "observation "+d.Adapter+"."+d.Op); err != nil {
			return d, err
		}
	}
	d.Args = args
	return d, nil
}

// bindEffect binds an external effect's args and Key.
func (s scope) bindEffect(e EffectData) (EffectData, error) {
	if e.Kind != "external" {
		return e, nil
	}
	generic, err := genericArgs(e.Args)
	if err != nil {
		return e, err
	}
	literal := false
	var argTemplates []EffectArgTemplate
	var keyTemplate []TextSpan
	switch e.BindingVersion {
	case 0: // legacy plans treated all ordinary strings as source
	case effectBindingVersion:
		literal, argTemplates, keyTemplate = e.Literal, e.ArgTemplates, e.KeyTemplate
	default:
		return e, fmt.Errorf("unknown effect binding version %d", e.BindingVersion)
	}
	for _, template := range argTemplates {
		if !setEffectArg(generic, template.Path, nil) {
			return e, fmt.Errorf("template argument path %q is missing", template.Path)
		}
	}
	var args map[string]any
	if generic != nil {
		bound, bindErr := s.bindValueMode(generic, true, literal, e.BindingVersion == 0)
		if bindErr != nil {
			return e, bindErr
		}
		args = bound.(map[string]any)
	}
	for _, template := range argTemplates {
		bound, bindErr := s.bindEffectArg(template)
		if bindErr != nil {
			return e, bindErr
		}
		if !setEffectArg(args, template.Path, bound) {
			return e, fmt.Errorf("bound template argument path %q is missing", template.Path)
		}
	}
	e.Args = args
	var parts []string
	switch {
	case len(e.KeyPartTemplates) > 1 && keyTemplate != nil:
		for _, part := range e.KeyPartTemplates {
			bound, bindErr := s.bindText(part)
			if bindErr != nil {
				return e, bindErr
			}
			parts = append(parts, bound)
		}
	case len(e.KeyParts) > 1:
		for _, part := range e.KeyParts {
			bound := part
			switch {
			case literal:
			case e.BindingVersion == 0:
				bound, err = s.bindString(part)
			default:
				bound, err = s.bindPlaceholders(part)
			}
			if err != nil {
				return e, err
			}
			parts = append(parts, bound)
		}
	case keyTemplate != nil:
		e.Key, err = s.bindText(keyTemplate)
	case !literal && e.Key != "" && e.BindingVersion == 0:
		e.Key, err = s.bindString(e.Key)
	case !literal && e.Key != "":
		e.Key, err = s.bindPlaceholders(e.Key)
	}
	if parts != nil {
		e.Key, e.LegacyKey = escapedKey(parts)
	}
	e.BindingVersion, e.Literal, e.ArgTemplates, e.KeyTemplate, e.KeyParts, e.KeyPartTemplates = 0, false, nil, nil, nil, nil
	return e, err
}

// escapedKey is the key of bound parts (joinKeyParts) and, when it differs,
// the unescaped join a version before part escaping bound them to.
func escapedKey(parts []string) (key, legacy string) {
	key = joinKeyParts(parts)
	if legacy = legacyKeyJoin(parts); legacy == key {
		legacy = ""
	}
	return key, legacy
}

// canonDesc is the canonical JSON of a resolved descriptor.
func canonDesc(d ObservationDesc) string {
	b, err := json.Marshal(d)
	if err != nil {
		panic("wf: marshalling a descriptor: " + err.Error())
	}
	return string(b)
}

// normDesc binds d and normalizes it to the form the runtime sees.
func (s scope) normDesc(d ObservationDesc) (ObservationDesc, error) { return s.bindDesc(d) }

// promptInput is one declared prompt input resolved to a name and a value.
type promptInput struct {
	name  string
	value string
	out   bool
	bound bool // a Bound input: its literal [NAME] token in the prompt is replaced too
	shell bool // a ShellBound or ShellWords input: presented as bound, never paged
}

// inputValues resolves the declared inputs of a prompt, in declaration order.
func (s scope) inputValues(inputs []Input) ([]promptInput, error) {
	var out []promptInput
	for _, in := range inputs {
		d := in.inputRef()
		pi := promptInput{name: d.name}
		switch d.kind {
		case "param":
			if _, ok := s.c.st.started.Params[d.name]; !ok {
				return nil, fmt.Errorf("%w: parameter %s not supplied", ErrUnresolved, d.name)
			}
			v, err := s.c.paramValue(d.name)
			if err != nil {
				return nil, err
			}
			if _, typed := s.c.st.started.ParamPaths[d.name]; typed {
				if err := s.c.checkForkValue(v, "prompt input "+d.name); err != nil {
					return nil, err
				}
			}
			pi.value = v
		case "artifact":
			ref, ok := s.in.Artifacts[d.name]
			if ok {
				pi.value = ref.Path
			} else if d.art != nil {
				v, err := s.c.artifactPath(s.in, d.art)
				if err != nil {
					return nil, err
				}
				pi.value = v
			}
			if err := s.c.checkForkValue(pi.value, "prompt input "+d.name); err != nil {
				return nil, err
			}
		case "out":
			raw, ok := s.in.Outputs[d.name]
			if !ok {
				raw, ok = s.in.Computed[d.name]
			}
			// A state that has not produced output yet binds empty: the first
			// round of a loop (critique's first Draft, a Strong tier reached
			// without the Cheap tier ever being accepted) has nothing to show.
			value, err := s.promptDocument(raw, "prompt input "+pi.name)
			if err != nil {
				return nil, err
			}
			pi.value, pi.out = value, true
		case "bound":
			// A path marked in the prose keeps its path semantics: in a fork
			// its bound target is checked before it is presented. Unmarked
			// text is citation and passes.
			for _, seg := range markedSegments(d.tmpl) {
				p, err := s.bindString(seg)
				if err != nil {
					continue // the whole template's binding reports it
				}
				if err := s.c.checkForkValue(p, "prompt input "+pi.name); err != nil {
					return nil, err
				}
			}
			// Bound is rendered prose, so path tokens embedded by shared path
			// helpers render as text rather than claiming the whole sentence.
			v, err := s.bindString(strings.ReplaceAll(d.tmpl, pathTemplatePrefix, ""))
			if err != nil {
				return nil, err
			}
			if d.shell != "" {
				// A shell word is an argument the agent runs, never citation.
				if err := s.c.checkForkValue(v, "prompt input "+pi.name); err != nil {
					return nil, err
				}
				if v, err = quoteBound(d.shell, v); err != nil {
					return nil, fmt.Errorf("prompt input %s: %w", pi.name, err)
				}
			}
			pi.value, pi.bound, pi.shell = v, true, d.shell != ""
		case "observed":
			v, err := s.c.observedInput(s.in, d)
			if err != nil {
				return nil, err
			}
			pi.value = v
		case "returned":
			value, err := s.returnedDocument(s.in.node())
			if err != nil {
				return nil, err
			}
			pi.value, pi.out = value, true
		case "child-input":
			if len(s.in.Input) == 0 {
				return nil, fmt.Errorf("%w: the instance has no input", ErrUnresolved)
			}
			value, err := s.promptDocument(s.in.Input, "prompt input "+pi.name)
			if err != nil {
				return nil, err
			}
			pi.value = value
		default:
			return nil, fmt.Errorf("%w: input kind %s", ErrUnresolved, d.kind)
		}
		if d.alias != "" {
			pi.name = d.alias
		}
		out = append(out, pi)
	}
	return out, nil
}

// promptDocument resolves typed paths for delivery, preserving the stored
// output. In a fork, each typed path is checked at its resolved target
// (field names the input): it is a path the agent is given to act on. The
// untyped text around it is citation, what the agent reads, and unchecked.
func (s scope) promptDocument(raw json.RawMessage, field string) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	if typed := typedPaths(value); typed != nil {
		if err := s.c.checkForkValue(typed, field); err != nil {
			return "", err
		}
	}
	resolved, err := s.c.resolvePathValues(value)
	if err != nil {
		return "", err
	}
	if reflect.DeepEqual(value, resolved) {
		return string(raw), nil
	}
	b, err := json.Marshal(resolved)
	return string(b), err
}

// bindPrompt resolves {{name}} in a prompt: a declared input by name, then the
// binding scope, then the next Out input not yet bound, in declaration order.
// An unbound {{records}} is an error, never filled from an Out input.
// It also reports which inputs a token consumed: their values are in the text,
// so the Inputs block leaves them out. Each value goes in through
// substitute, so model text cannot add prompt structure; a Bound input's
// value is the definition's prose.
func (s scope) bindPrompt(text string, ins []promptInput) (string, []bool, error) {
	used := make([]bool, len(ins))
	var missing string
	// bindToken is the value of the token naming name, and whether it is a
	// Bound input's prose; ok is false for a token that stays as written.
	// modeOf is how substitute places an input's value: a Bound value as
	// the definition's prose, a shell word as it is.
	modeOf := func(pi promptInput) substitution {
		switch {
		case pi.shell:
			return asWord
		case pi.bound:
			return asProse
		}
		return asValue
	}
	bindToken := func(name string) (value string, mode substitution, ok bool) {
		if name == scratchName {
			// The engine's directory, never a prompt input named scratch.
			v, ok := s.lookup(name)
			return v, asValue, ok
		}
		for i, pi := range ins {
			if pi.name == name {
				used[i] = true
				return pi.value, modeOf(pi), true
			}
		}
		for i, pi := range ins {
			if strings.EqualFold(pi.name, name) {
				used[i] = true
				return pi.value, modeOf(pi), true
			}
		}
		if v, ok := s.lookup(name); ok {
			return v, asValue, true
		}
		if name == recordsName || name == "assets" {
			// A builtin that fails to bind is an error, never an Out input.
			return "", asValue, false
		}
		for i, pi := range ins {
			if pi.out && !used[i] {
				used[i] = true
				return pi.value, modeOf(pi), true
			}
		}
		return "", asValue, false
	}
	out := bracketBound(text, ins, used, func(b *promptText, source string) {
		at := 0
		for _, m := range promptPattern.FindAllStringSubmatchIndex(source, -1) {
			b.WriteString(source[at:m[0]])
			if v, mode, ok := bindToken(source[m[2]:m[3]]); ok {
				substitute(b, v, mode)
			} else {
				b.WriteString(source[m[0]:m[1]])
				if missing == "" {
					missing = source[m[0]:m[1]]
				}
			}
			at = m[1]
		}
		b.WriteString(source[at:])
	})
	if missing != "" {
		return "", nil, s.unresolved(missing, "prompt")
	}
	return out, used, nil
}

// blockStart matches the part of a prompt line before a token where a
// Markdown block can still start: only indentation, block quote markers and
// list item markers.
var blockStart = regexp.MustCompile(`^(?:[ \t]*(?:>|[-*+]|[0-9]{1,9}[.)])(?:[ \t]|$))*[ \t]*$`)

// sectionLine matches a line Markdown can read as a heading (ATX, or a
// setext underline, which also covers a thematic break) or as a code fence,
// also inside block quote and list item containers ("> ## Artifacts",
// "- ## Inputs", "1. > ```"), whose markers come before the block.
var sectionLine = regexp.MustCompile("(?m)^(?:[ \t]*(?:>|[-*+]|[0-9]{1,9}[.)])[ \t]+)* {0,3}(#{1,6}([ \t]|\r?$)|(=+|-+)[ \t]*\r?$|```|~~~)")

// promptText is a prompt being bound. widened is the template code fence
// substitute lengthened to hold a value, while the template text after the
// value, which holds that fence's closer, is still being written.
type promptText struct {
	strings.Builder
	widened *widenedFence
}

// widenedFence is a template code fence whose opener now has n of c, up
// from orig. Its closer is the first line after offset after (the end of
// the last value in the fence) that closes a fence of orig c's: the
// template wrote it for the opener it wrote.
type widenedFence struct {
	c              byte
	orig, n, after int
}

// closeWidened lengthens the widened fence's closer to its opener's n once
// the template text holding it is written. Before the bind ends (final
// false) only a complete line counts: text after a token may still extend
// the last line.
func (p *promptText) closeWidened(final bool) {
	w := p.widened
	if w == nil {
		return
	}
	s := p.String()
	at := w.after
	if at > 0 && s[at-1] != '\n' {
		// The rest of the value's last line is fence content.
		i := strings.IndexByte(s[at:], '\n')
		if i < 0 {
			return
		}
		at += i + 1
	}
	for at < len(s) {
		end := strings.IndexByte(s[at:], '\n')
		if end < 0 && !final {
			return
		}
		if end < 0 {
			end = len(s)
		} else {
			end += at
		}
		if indent, c, run, rest, ok := fenceLine(s[at:end]); ok && c == w.c && run >= w.orig && strings.Trim(rest, " \t") == "" {
			if run < w.n {
				start := at + indent
				p.Reset()
				p.WriteString(s[:start] + strings.Repeat(string(c), w.n) + s[start+run:])
			}
			p.widened = nil
			return
		}
		at = end + 1
	}
}

// fenceLine reads line as a code fence line (CommonMark): at most three
// spaces of indentation, then a run of three or more of one fence character
// (a backtick or a tilde), then rest. Container markers before it are not
// read: a template's own code block starts at the margin.
func fenceLine(line string) (indent int, c byte, run int, rest string, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	for indent < len(line) && indent < 4 && line[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent == len(line) || line[indent] != '`' && line[indent] != '~' {
		return 0, 0, 0, "", false
	}
	c = line[indent]
	for run = 0; indent+run < len(line) && line[indent+run] == c; run++ {
	}
	if run < 3 {
		return 0, 0, 0, "", false
	}
	return indent, c, run, line[indent+run:], true
}

// openFence reports the code fence the complete lines of s leave open: the
// offset of its opener's run, the run's length and its character. An
// opener of backticks has no backtick after the run (CommonMark: the info
// string cannot hold one); a closer is a run of the opener's character at
// least as long as the opener's, with nothing but spaces or tabs after it.
func openFence(s string) (at, run int, c byte, open bool) {
	s = s[:strings.LastIndexByte(s, '\n')+1]
	for i := 0; i < len(s); {
		j := i + strings.IndexByte(s[i:], '\n')
		indent, fc, n, rest, ok := fenceLine(s[i:j])
		switch {
		case !ok:
		case !open && (fc == '~' || !strings.Contains(rest, "`")):
			at, run, c, open = i+indent, n, fc, true
		case open && fc == c && n >= run && strings.Trim(rest, " \t") == "":
			open = false
		}
		i = j + 1
	}
	return at, run, c, open
}

// longestRun is the length of the longest run of c in v.
func longestRun(v string, c byte) int {
	run, longest := 0, 0
	for i := 0; i < len(v); i++ {
		if v[i] == c {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// substitute appends v, the value of a prompt token, to b, the prompt so
// far. A value is often an earlier state's model output, so it must not add
// prompt structure (a heading, a list, a second Artifacts section). A single
// line stays as it is mid-sentence, where inline text cannot start a block,
// and so does a plain line (plainLine) anywhere. A Bound input (prose) is
// the definition's own text, which may lay out a table or a list it built
// (through sink, escaping each value at construction), so it stays as it is
// too unless a line in it could start a section or a code fence: a bound
// value can still carry a model's words. Any other value goes on lines of
// its own as inputBody fences it, which carries it verbatim; the text after
// the token continues on the line after the fence.
//
// A token inside a code fence the template opened is already in a code
// block, where only a closing fence line is structure. The value goes in as
// it is when it holds no run of the fence's character as long as the
// opener's, so no line of it can close the block. Otherwise the template's
// fence grows one longer than the value's longest run, its opener here and
// its closer when the template writes it (closeWidened), so the block still
// holds the value and ends where the template ends it.
func substitute(b *promptText, v string, mode substitution) {
	b.closeWidened(false)
	if mode == asWord {
		// One shell word: one line with no backtick (quoteBound), so it
		// cannot end a fence or a code span. Fencing it would split the
		// command it completes ("--" reads as a setext underline).
		b.WriteString(v)
		return
	}
	prose := mode == asProse
	s := b.String()
	if at, run, c, open := openFence(s); open {
		if n := longestRun(v, c); n >= run {
			b.Reset()
			b.WriteString(s[:at] + strings.Repeat(string(c), n+1) + s[at+run:])
			if b.widened == nil {
				b.widened = &widenedFence{c: c, orig: run}
			}
			b.widened.n = n + 1
		}
		b.WriteString(v)
		if b.widened != nil {
			b.widened.after = b.Len()
		}
		return
	}
	line := s[strings.LastIndexAny(s, "\r\n")+1:]
	if !strings.ContainsAny(v, "\r\n") && (v == "" || plainLine(v) || !blockStart.MatchString(line)) ||
		prose && !sectionLine.MatchString(strings.ReplaceAll(v, "\r", "\n")) {
		b.WriteString(v)
		return
	}
	if line != "" {
		b.WriteString("\n")
	}
	b.WriteString(fenceValue(v))
	b.WriteString("\n")
}

// substitution is how substitute places a value.
type substitution int

const (
	asValue substitution = iota // model text: fenced where it could add structure
	asProse                     // a Bound input's prose: fenced only where it would read as a section
	asWord                      // a shell word: written as it is
)

// bracketPattern matches one [..] token, allowing one nested [..] (the triage
// prompt's plan section names [plan file name] inside its own token).
var bracketPattern = regexp.MustCompile(`\[(?:[^\[\]]|\[[^\[\]]*\])*\]`)

// bracketBound replaces each [..] token that a Bound input names with its
// value, in one pass: a value that itself holds a bracket token is not
// expanded. A token is named by an input whose name equals its text or starts
// it and is followed by a non-alphanumeric character, so Bound("ARTIFACT", ..)
// fills [ARTIFACT] and [ARTIFACT — per the caller's mode]. Names are tried
// longest first, never in map order; a token no input names stays as it is.
// used[i] is set for each input in ins that filled a token. bindUnbound
// appends the spans of the original source that no Bound input replaced;
// a Bound value goes in through substitute.
func bracketBound(text string, ins []promptInput, used []bool, bindUnbound func(*promptText, string)) string {
	var bound []int
	for i, pi := range ins {
		if pi.bound {
			bound = append(bound, i)
		}
	}
	var out promptText
	if len(bound) == 0 {
		bindUnbound(&out, text)
		out.closeWidened(true)
		return out.String()
	}
	sort.SliceStable(bound, func(i, j int) bool { return len(ins[bound[i]].name) > len(ins[bound[j]].name) })
	at := 0
	for _, m := range bracketPattern.FindAllStringIndex(text, -1) {
		inner := text[m[0]+1 : m[1]-1]
		for _, i := range bound {
			pi := ins[i]
			rest, ok := strings.CutPrefix(inner, pi.name)
			if !ok {
				continue
			}
			if r, _ := utf8.DecodeRuneInString(rest); rest == "" || !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				bindUnbound(&out, text[at:m[0]])
				mode := asProse
				if pi.shell {
					mode = asWord
				}
				substitute(&out, pi.value, mode)
				at = m[1]
				used[i] = true
				break
			}
		}
	}
	bindUnbound(&out, text[at:])
	out.closeWidened(true)
	return out.String()
}

// inputsBlock renders every declared input no token consumed, in declaration
// order: a consumed input's value is already in the prompt, and a second copy
// only doubles its size (a judge's or the deduper's findings JSON).
func inputsBlock(ins []promptInput, used []bool) string {
	var b strings.Builder
	for i, pi := range ins {
		if i < len(used) && used[i] {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("## Inputs\n")
		}
		fmt.Fprintf(&b, "\n### %s\n\n%s\n", pi.name, inputBody(pi.value))
	}
	return b.String()
}

// inputBody is an input value as the body of its ### section. The value is
// often an earlier state's model output, so one that Markdown could read as
// block structure (a heading, a fence, a list, a second section) goes inside
// a fenced code block whose fence is longer than any backtick run in it:
// nothing in the value can close the fence, and the value is carried
// verbatim. A single line that starts with a letter, a digit (not an ordered
// list marker) or a path or JSON opener cannot start a block, and stays as
// it is.
func inputBody(v string) string {
	if v == "" || plainLine(v) {
		return v
	}
	return fenceValue(v)
}

// fenceValue is v as a fenced code block whose fence is longer than any
// backtick run in v, so nothing in v can close it.
func fenceValue(v string) string {
	fence := strings.Repeat("`", max(3, longestRun(v, '`')+1))
	// The closing fence goes on its own line after the value, so the block's
	// content is the value and one final line break, which a reader drops.
	return fence + "\n" + v + "\n" + fence
}

// plainLine reports whether v is one line Markdown reads as paragraph text.
func plainLine(v string) bool {
	if strings.ContainsAny(v, "\r\n") {
		return false
	}
	c := v[0]
	switch {
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '/' || c == '{' || c == '"':
		return true
	case c >= '0' && c <= '9':
		rest := strings.TrimLeft(v, "0123456789")
		return rest == "" || rest[0] != '.' && rest[0] != ')'
	}
	return false
}
