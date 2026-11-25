package wf

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// snapshotRelocatable checks values that survive into the fork's operational
// state. It never rewrites domain data. Rendered instructions, observation
// receipts, a command's accepted result and pinned harness evidence are
// history, not path declarations.
func snapshotRelocatable(records []JournalRecord, roots PathRoots) error {
	return CheckJournalRelocation(records, roots)
}

// CheckJournalRelocation performs the same operational-path preflight as
// snapshot forking, without taking a snapshot or modifying journal data.
// Errors identify the journal record and field that cannot be relocated.
func CheckJournalRelocation(records []JournalRecord, roots PathRoots) error {
	origins := []PathRoots{roots}
	if len(records) > 0 && records[0].Roots != nil {
		origins = append(origins, *records[0].Roots)
	}
	var started RunStartedData
	for _, rec := range records {
		if rec.Kind == JournalRunStarted {
			if err := json.Unmarshal(rec.Data, &started); err != nil {
				return fmt.Errorf("journal %d %s: %w", rec.Seq, rec.Kind, err)
			}
			for _, name := range slices.Sorted(maps.Keys(started.ParamPaths)) {
				base := started.ParamPaths[name]
				value, ok := started.Params[name]
				if !ok {
					return fmt.Errorf("%w: journal %d %s Params.%s has no value", ErrCorruptJournal, rec.Seq, rec.Kind, name)
				}
				if err := validatePathParam(name, value, base); err != nil {
					return fmt.Errorf("journal %d %s: %w", rec.Seq, rec.Kind, err)
				}
			}
			break
		}
	}
	// A command state's accepted output is the RunResult its effect returned
	// (commandOps.finish): what the command printed, like an observation's
	// value, never a path the run declared. Only its Output, the engine's
	// own full-output file, is a path: a fork relocates it
	// (relocateCommandOutputs), so it must be that file.
	commands := commandAcceptances(records)
	// An agent's submission and the acceptance deciding it are what the
	// agent wrote: prose that may cite any path, like a command's printed
	// output. So are the values a definition derives from them, a Map's
	// child_spawned Input and child_settled Result and a Compute acceptance.
	// This check covers typed paths in every record and untyped strings in
	// engine-owned records (Params, Options, effect and cleanup args, Plan,
	// an acceptance that is no agent's or command's); of history it reads
	// only typed path fields. The bind-time source-root refusal
	// (checkForkValue) guards every operational use of untyped history.
	history := historyRecords(records)
	// An effect intent with a result or a settlement in the prefix is done:
	// its args are history, like a command's printed output, and the fork
	// never applies it again; only an open intent is applied from its
	// recorded args.
	done := doneIntents(records)
	for _, rec := range records {
		var data map[string]any
		if err := json.Unmarshal(rec.Data, &data); err != nil {
			return fmt.Errorf("journal %d %s: %w", rec.Seq, rec.Kind, err)
		}
		values := map[string]any{"Plan": data["Plan"]}
		switch rec.Kind {
		case JournalRunStarted:
			params, _ := data["Params"].(map[string]any)
			for name, base := range started.ParamPaths {
				if base == WorkspaceRelative || base == RunRelative {
					delete(params, name)
				}
			}
			// A fork rewrites an untyped param from the source roots to its
			// own (relocateUntypedParams): it is checked as rewritten, so
			// only a source path the rewrite cannot map (another root, a
			// spelling that is no plain path) refuses.
			for name, v := range params {
				if _, typed := started.ParamPaths[name]; !typed {
					if str, ok := v.(string); ok {
						params[name] = relocateParamRoots(str, origins, PathRoots{Workspace: "<fork-workspace>", State: "<fork-state>"})
					}
				}
			}
			values["Params"], values["Options"] = params, data["Options"]
		case JournalAccepted, JournalSubmitted:
			if pair, ok := commands[rec.Seq]; ok {
				if out := pair.rr.Output; out != "" && !engineOutputFile(out, pair.intent, origins) {
					values["Output.Output"] = out
				}
				break
			}
			if history[rec.Seq] {
				values["Output"] = typedPaths(data["Output"])
				break
			}
			values["Output"] = data["Output"]
		case JournalChildSpawned:
			values["Input"] = typedPaths(data["Input"])
		case JournalChildSettled:
			values["Result"] = typedPaths(data["Result"])
		case JournalEffectIntent, JournalCleanupDeferred:
			if effect, ok := data["Effect"].(map[string]any); ok {
				if args, ok := effect["Args"].(map[string]any); ok {
					if command, _ := data["Command"].(bool); command {
						// Command cwd is already an explicitly owned journal path.
						delete(args, "dir")
					}
					if rec.Kind == JournalEffectIntent && done[rec.Seq] {
						values["Effect.Args"] = typedPaths(args)
						break
					}
					values["Effect.Args"] = args
				}
			}
		}
		for _, field := range sortedPathFields(values) {
			if err := snapshotValueRelocatableAt(values[field], origins, started.ParamPaths, field); err != nil {
				return fmt.Errorf("%w (journal %d %s)", err, rec.Seq, rec.Kind)
			}
		}
	}
	return nil
}

func snapshotValueRelocatableAt(value any, roots []PathRoots, params map[string]PathBase, field string) error {
	if p, marked, err := markedOwnedPath(value); marked || err != nil {
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		if p.Base == externalPathBase {
			return snapshotValueRelocatableAt(p.Path, roots, params, field)
		}
		return nil
	}
	if name, marked, err := markedPathParam(value); marked || err != nil {
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		if _, ok := params[name]; !ok {
			return fmt.Errorf("%w: %s path param marker %q has no recorded ownership", ErrCorruptJournal, field, name)
		}
		return nil
	}
	if _, marked, err := markedRunPath(value); marked || err != nil {
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		return nil
	}
	switch v := value.(type) {
	case string:
		for _, root := range roots {
			for _, origin := range []struct{ name, path string }{{"state", root.State}, {"workspace", root.Workspace}} {
				if origin.path == "" {
					continue
				}
				if mentionsSnapshotRoot(v, origin.path) {
					return &untypedSourcePathError{fmt.Sprintf("%s records untyped paths under the source %s root; it can't be relocated", field, origin.name)}
				}
			}
		}
	case map[string]any:
		for _, key := range sortedPathFields(v) {
			if err := snapshotValueRelocatableAt(v[key], roots, params, field+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range v {
			if err := snapshotValueRelocatableAt(item, roots, params, fmt.Sprintf("%s[%d]", field, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Paths embedded in a shell argument or another operational string are just as
// unsafe as bare paths. This is a conservative refusal, never a text rewrite.
func mentionsSnapshotRoot(value, root string) bool {
	root = filepath.Clean(root)
	// Containment must use filesystem spelling, so dot components and repeated
	// separators cannot hide a source path. Also inspect shell-delimited words.
	words := append([]string{value}, strings.FieldsFunc(value, func(r rune) bool {
		return strings.ContainsRune(" \t\r\n\"'`;|&()=", r)
	})...)
	for _, word := range words {
		if filepath.IsAbs(word) {
			if _, inside := snapshotRelative(root, filepath.Clean(word)); inside || filepath.Clean(word) == root {
				return true
			}
		}
	}
	for {
		i := strings.Index(value, root)
		if i < 0 {
			return false
		}
		tail := value[i+len(root):]
		if tail == "" || strings.ContainsRune("/\\ \t\r\n\"'`;|&)", rune(tail[0])) {
			return true
		}
		value = tail
	}
}

func sortedPathFields(v map[string]any) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normalizeSnapshotParams updates only the copied run header. A declaration may
// opt a legacy named parameter into relocation; recorded ownership always wins.
func normalizeSnapshotParams(records []JournalRecord, def *Def, workspace, runDir, stateRoot string) error {
	for i := range records {
		if records[i].Kind != JournalRunStarted {
			continue
		}
		var started RunStartedData
		if err := json.Unmarshal(records[i].Data, &started); err != nil {
			return err
		}
		legacy := map[string]string{}
		for name, value := range started.Params {
			if _, typed := started.ParamPaths[name]; !typed {
				legacy[name] = value
			}
		}
		params, ownership, err := normalizePathParams(def, legacy, workspace, runDir, stateRoot)
		if err != nil {
			return err
		}
		for name, base := range started.ParamPaths {
			value, ok := started.Params[name]
			if !ok {
				return fmt.Errorf("%w: path param %q has no value", ErrCorruptJournal, name)
			}
			if err := validatePathParam(name, value, base); err != nil {
				return err
			}
			params[name] = value
			if ownership == nil {
				ownership = map[string]PathBase{}
			}
			ownership[name] = base
		}
		if maps.Equal(params, started.Params) && maps.Equal(ownership, started.ParamPaths) {
			return nil
		}
		// Preserve all other historical fields as JSON values.
		var header map[string]json.RawMessage
		if err := json.Unmarshal(records[i].Data, &header); err != nil {
			return err
		}
		header["Params"], err = json.Marshal(params)
		if err != nil {
			return err
		}
		if len(ownership) != 0 {
			header["ParamPaths"], err = json.Marshal(ownership)
			if err != nil {
				return err
			}
		}
		records[i].Data, err = json.Marshal(header)
		return err
	}
	return fmt.Errorf("%w: missing run header", ErrCorruptJournal)
}

// untypedSourcePathError: a journal record holds a path under a source root
// that is not typed, so a fork cannot move it to its own roots. It is the one
// relocation refusal that concerns a fork alone; a fanout runs its other
// forks.
type untypedSourcePathError struct{ msg string }

func (e *untypedSourcePathError) Error() string { return ErrUnavailable.Error() + ": " + e.msg }
func (e *untypedSourcePathError) Unwrap() error { return ErrUnavailable }

// commandPair is a command state's accepted record's source: the effect
// intent it ran and the RunResult that intent's result returned.
type commandPair struct {
	intent, result int64
	rr             RunResult
}

// commandAcceptances maps each accepted record that is a command state's
// routed result to its pair: a Computed acceptance of the same instance and
// state as an OK effect_result of a command intent, before any transition of
// that instance, whose Output is that very RunResult. A failed result, a
// new intent or a transition ends the pairing, so a later acceptance (a
// migrated or reused state name) is never taken for a command's.
func commandAcceptances(records []JournalRecord) map[int64]commandPair {
	commandIntents := map[int64]bool{}
	open := map[string]commandPair{}
	out := map[int64]commandPair{}
	for _, r := range records {
		key := r.Instance + "\x00" + r.State
		switch r.Kind {
		case JournalEffectIntent:
			var d EffectIntentData
			if json.Unmarshal(r.Data, &d) == nil && d.Command {
				commandIntents[r.Seq] = true
			}
			delete(open, key)
		case JournalEffectResult:
			var d EffectResultData
			if json.Unmarshal(r.Data, &d) != nil || !commandIntents[d.Intent] {
				continue
			}
			var rr RunResult
			if d.OK && json.Unmarshal(d.Result, &rr) == nil {
				open[key] = commandPair{intent: d.Intent, result: r.Seq, rr: rr}
			} else {
				delete(open, key)
			}
		case JournalAccepted:
			pair, ok := open[key]
			delete(open, key)
			var a AcceptedData
			var rr RunResult
			if ok && json.Unmarshal(r.Data, &a) == nil && a.Computed && json.Unmarshal(a.Output, &rr) == nil && rr == pair.rr {
				out[r.Seq] = pair
			}
		case JournalTransition:
			for k := range open {
				if strings.HasPrefix(k, r.Instance+"\x00") {
					delete(open, k)
				}
			}
		}
	}
	return out
}

// historyRecords are the seqs of agent history among submitted and accepted
// records: every submission (only an agent unit submits), the acceptance that
// decides the instance's open submission in that state, and every Computed
// acceptance of a state that ran no command (a Compute state's value,
// derived from history; a command state's is history only as
// commandAcceptances pairs it). An acceptance with no open submission (a
// migrated or reused state name, a hand-written record) is not history.
// doneIntents are the effect intents of records that have a result or a
// settlement in records.
func doneIntents(records []JournalRecord) map[int64]bool {
	out := map[int64]bool{}
	for _, r := range records {
		var d struct{ Intent int64 }
		switch r.Kind {
		case JournalEffectResult, JournalEffectSettled:
			if json.Unmarshal(r.Data, &d) == nil && d.Intent != 0 {
				out[d.Intent] = true
			}
		}
	}
	return out
}

func historyRecords(records []JournalRecord) map[int64]bool {
	open := map[string]bool{}
	commandStates := map[string]bool{}
	out := map[int64]bool{}
	for _, r := range records {
		key := r.Instance + "\x00" + r.State
		switch r.Kind {
		case JournalEffectIntent:
			var d EffectIntentData
			if json.Unmarshal(r.Data, &d) == nil && d.Command {
				commandStates[key] = true
			}
		case JournalSubmitted:
			open[key] = true
			out[r.Seq] = true
		case JournalAccepted:
			var a AcceptedData
			if json.Unmarshal(r.Data, &a) == nil && (a.Computed && !commandStates[key] || open[key]) {
				out[r.Seq] = true
			}
			delete(open, key)
		case JournalTransition:
			for k := range open {
				if strings.HasPrefix(k, r.Instance+"\x00") {
					delete(open, k)
				}
			}
		}
	}
	return out
}

// typedPaths is v with only its typed path values kept (the owned, path
// parameter and run path markers), at their positions; every other value,
// untyped text included, is dropped.
func typedPaths(v any) any {
	if _, marked, _ := markedOwnedPath(v); marked {
		return v
	}
	if _, marked, _ := markedPathParam(v); marked {
		return v
	}
	if _, marked, _ := markedRunPath(v); marked {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if t := typedPaths(e); t != nil {
				out[k] = t
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]any, len(x))
		kept := false
		for i, e := range x {
			if out[i] = typedPaths(e); out[i] != nil {
				kept = true
			}
		}
		if !kept {
			return nil
		}
		return out
	}
	return nil
}

// engineOutputFile reports whether p is the full-output file the engine
// wrote for intent (cutResult): <run dir>/effects/<intent>[.<n>].out under a
// source state root.
func engineOutputFile(p string, intent int64, origins []PathRoots) bool {
	base := filepath.Base(p)
	name := strconv.FormatInt(intent, 10)
	if !filepath.IsAbs(p) || filepath.Base(filepath.Dir(p)) != "effects" || !(base == name+".out" || strings.HasPrefix(base, name+".") && strings.HasSuffix(base, ".out")) {
		return false
	}
	for _, o := range origins {
		if _, inside := snapshotRelative(o.State, p); o.State != "" && inside {
			return true
		}
	}
	return false
}

// relocateCommandOutputs moves each command result's full-output path (its
// RunResult.Output) from the source roots to the fork's, in the result and
// in the acceptance that routed it: it is the engine's own file, so a typed
// run path in all but encoding. It returns the files to carry into the fork,
// source path to fork path: each is written once, when its intent's result
// is, and never changes.
func relocateCommandOutputs(prefix []JournalRecord, from, to PathRoots) (map[string]string, error) {
	pairs := commandAcceptances(prefix)
	move := map[int64]bool{}
	files := map[string]string{}
	for acc, p := range pairs {
		if p.rr.Output != "" && engineOutputFile(p.rr.Output, p.intent, []PathRoots{from}) {
			move[acc], move[p.result] = true, true
			files[p.rr.Output] = relocateString(p.rr.Output, from, to)
		}
	}
	for i := range prefix {
		if !move[prefix[i].Seq] {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(prefix[i].Data, &fields); err != nil {
			return nil, err
		}
		key := "Output"
		if prefix[i].Kind == JournalEffectResult {
			key = "Result"
		}
		var rr RunResult
		if err := json.Unmarshal(fields[key], &rr); err != nil {
			return nil, err
		}
		rr.Output = relocateString(rr.Output, from, to)
		raw, err := json.Marshal(rr)
		if err != nil {
			return nil, err
		}
		fields[key] = raw
		if prefix[i].Data, err = json.Marshal(fields); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// relocateUntypedParams rewrites, in a fork's run_started record, each
// untyped param that names a source root as a path (the root, or a path
// under it, wherever it stands in the string: a shell command's operand)
// to the same path under the fork's root, and returns the params rewritten
// ("Params.<name>", sorted). A typed path param is relocated by its type,
// and a string that only shares a root's spelling ("<root>-other") is left
// as it is.
func relocateUntypedParams(raw json.RawMessage, from []PathRoots, to PathRoots) (json.RawMessage, []string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	var started struct {
		Params     map[string]string
		ParamPaths map[string]PathBase
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		return nil, nil, err
	}
	var names []string
	for _, name := range slices.Sorted(maps.Keys(started.Params)) {
		if _, typed := started.ParamPaths[name]; typed {
			continue
		}
		if v := relocateParamRoots(started.Params[name], from, to); v != started.Params[name] {
			started.Params[name] = v
			names = append(names, "Params."+name)
		}
	}
	if len(names) == 0 {
		return raw, nil, nil
	}
	params, err := json.Marshal(started.Params)
	if err != nil {
		return nil, nil, err
	}
	fields["Params"] = params
	out, err := json.Marshal(fields)
	return out, names, err
}

// relocateParamRoots replaces each occurrence in s of a root of from (the
// run's current roots, then the roots it was recorded under: a run moved
// since names its old ones) that stands as a path, the root itself or a path
// under it (preceded by the string's start or a shell delimiter, followed by
// its end, a separator or a delimiter), by the same root of to. It is one
// pass over every root, the longest first, so a state root inside the
// workspace keeps its own mapping and a rewritten path is never rewritten
// again (a fork's root may lie below a source root).
func relocateParamRoots(s string, from []PathRoots, to PathRoots) string {
	target := map[string]string{}
	for _, f := range from {
		for _, p := range [][2]string{{f.Workspace, to.Workspace}, {f.State, to.State}} {
			root := filepath.Clean(p[0])
			if _, seen := target[root]; !seen && p[0] != "" && root != "." && p[1] != "" {
				target[root] = p[1]
			}
		}
	}
	roots := slices.Collect(maps.Keys(target))
	sort.Slice(roots, func(i, j int) bool {
		return len(roots[i]) > len(roots[j]) || len(roots[i]) == len(roots[j]) && roots[i] < roots[j]
	})
	const delimiters = " \t\r\n\"'`;|&()=<>"
	var b strings.Builder
	last := 0
	for at := 0; at < len(s); {
		matched := false
		for _, root := range roots {
			if !strings.HasPrefix(s[at:], root) {
				continue
			}
			end := at + len(root)
			before := at == 0 || strings.ContainsRune(delimiters, rune(s[at-1]))
			after := end == len(s) || s[end] == '/' || strings.ContainsRune(delimiters, rune(s[end]))
			if before && after {
				b.WriteString(s[last:at] + target[root])
				last, at, matched = end, end, true
				break
			}
		}
		if !matched {
			at++
		}
	}
	b.WriteString(s[last:])
	return b.String()
}
