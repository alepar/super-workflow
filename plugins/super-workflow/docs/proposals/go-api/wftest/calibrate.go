package wftest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// CalibrationConfig supplies known-good outputs and the same world as the
// randomized driver. Rewrites are author-declared semantic equivalences, such
// as row reordering or equivalent IDs; these are not safe for arbitrary types.
type CalibrationConfig struct {
	budget *checkRunBudget
	// Corpus preserves recording provenance and prompt identity alongside Valid fixtures.
	Corpus []wf.CorpusEntry
	// Progress is the designated forward-progress subset used by Check's
	// liveness phase. Calibrate validates the full Valid corpus.
	Progress        map[string][]json.RawMessage
	Valid           map[string][]json.RawMessage
	Artifacts       map[string]map[string]string
	Seeds, MaxSteps int
	Rewrites        map[string]func(json.RawMessage) []json.RawMessage
}

type CalibrationReport struct {
	// Problems retains the combined strict verdict; unknown contexts are
	// advisory because no check evaluation can be established for them.
	RealProblems, CoverageProblems []string
	ContextUnknown                 []string
	Stale                          []wf.CorpusEntry
	Problems                       []string
	NotChecked                     []string
	// TypeRejected mutations never reached the named checks. They also remain
	// in NotChecked, because decoding is not evidence of check coverage.
	TypeRejected    []string
	States, Samples int
	Overrides       []string
}

func (r *CalibrationReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "calibration: %d states, %d samples\n", r.States, r.Samples)
	fmt.Fprintf(&b, "unknown calibration contexts: %d\n", len(r.ContextUnknown))
	fmt.Fprintf(&b, "%d stale recordings (calibration requires recorded context)\n", len(r.Stale))
	for _, e := range r.Stale {
		fmt.Fprintf(&b, "stale %s from %s seq %d\n", e.State, e.Run, e.Seq)
	}
	for _, p := range r.Problems {
		fmt.Fprintln(&b, p)
	}
	for _, p := range r.Overrides {
		fmt.Fprintln(&b, "overrides: "+p)
	}
	for _, p := range r.TypeRejected {
		fmt.Fprintln(&b, "type rejected: "+p)
	}
	for _, p := range r.NotChecked {
		fmt.Fprintln(&b, "not checked: "+p)
	}
	return b.String()
}

// Calibrate reaches states through the real engine, then probes their checks
// in the reached context. Missing corpus entries and states never reached are
// fixture gaps, never a successful calibration. Declared examples exercise
// their own check; corpus entries exercise decoding and every check.
func Calibrate(factory func() *Sim, cfg CalibrationConfig) *CalibrationReport {
	return calibrate(factory, cfg)
}

// withRecordedCorpus keeps the caller's fixture slices unchanged and retains
// recording provenance for calibration while sharing values with simulation.
func withRecordedCorpus(cfg CalibrationConfig) CalibrationConfig {
	valid := make(map[string][]json.RawMessage, len(cfg.Valid))
	for key, values := range cfg.Valid {
		valid[key] = append([]json.RawMessage(nil), values...)
	}
	for _, entry := range cfg.Corpus {
		valid[entry.State] = append(valid[entry.State], entry.Output)
	}
	cfg.Valid = valid
	return cfg
}

func calibrate(factory func() *Sim, cfg CalibrationConfig) *CalibrationReport {
	report := &CalibrationReport{}
	views := map[string]wf.StateView{}
	d := factory().def
	var walk func(wf.DefView)
	walk = func(v wf.DefView) {
		for _, s := range v.States {
			if s.Kind == "agent" {
				views[strings.TrimPrefix(s.Path, d.Name()+"/")] = s
			}
		}
		for _, child := range v.Children {
			walk(child)
		}
	}
	walk(wf.Inspect(d))
	for _, entry := range cfg.Corpus {
		view, ok := views[entry.State]
		if !ok {
			for _, candidate := range views {
				if candidate.Path == entry.State {
					view, ok = candidate, true
					break
				}
			}
		}
		if !ok {
			report.addReal(entry.State + ": recording names unknown state")
			continue
		}
		calibrateRecording(view, entry, cfg, report)
	}
	keys := make([]string, 0, len(views))
	for key := range views {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if cfg.Rewrites[key] == nil && cfg.Rewrites[views[key].Name] == nil {
			report.NotChecked = append(report.NotChecked, key+": no rewrites declared")
		}
		for name, examples := range views[key].CheckExamples() {
			accept, refuse := false, false
			for _, ex := range examples {
				if ex.Refusal == "" {
					accept = true
				} else {
					refuse = true
				}
			}
			if !accept || !refuse {
				report.addReal(fmt.Sprintf("%s check %s: examples must include acceptance and refusal", key, name))
			}
		}
	}
	seen := map[string]bool{}
	probe := func(e *wf.Engine, run wf.RunID, in wf.Instruction) {
		key := strings.TrimPrefix(in.Class, d.Name()+"/")
		if _, ok := views[key]; !ok {
			key = strings.TrimPrefix(in.State, d.Name()+"/")
		}
		if seen[key] {
			return
		}
		seen[key] = true
		report.States++
		view, ok := views[key]
		if !ok {
			report.addReal("unknown state " + key)
			return
		}
		evaluate := func(raw json.RawMessage, check, expect, label string, mutation bool, overrides ...wf.ExampleRead) {
			report.Samples++
			defer func() {
				if p := recover(); p != nil {
					report.addReal(fmt.Sprintf("%s %s: check panic: %v", key, label, p))
				}
			}()
			for _, override := range overrides {
				report.Overrides = append(report.Overrides, fmt.Sprintf("%s %s %s: %s", key, check, label, override))
			}
			refused, err := e.CheckOutput(run, in.Name, raw, check, overrides...)
			if err != nil {
				report.addReal(fmt.Sprintf("%s %s: fixture gap: %v", key, label, err))
				return
			}
			if mutation {
				if reason, decodeOnly := refused["$decode"]; decodeOnly && len(refused) == 1 {
					report.TypeRejected = append(report.TypeRejected, key+" "+label+": "+reason)
					report.NotChecked = append(report.NotChecked, key+" "+label)
				} else if len(refused) == 0 {
					report.NotChecked = append(report.NotChecked, key+" "+label)
				}
				return
			}
			if expect != "" {
				if refused[check] != expect {
					report.addReal(fmt.Sprintf("%s check %s %s: expected refusal %q, got %v", key, check, label, expect, refused))
				}
			} else if len(refused) > 0 {
				report.addReal(fmt.Sprintf("%s %s: known-good result refused: %v", key, label, refused))
			}
		}
		for _, entry := range cfg.Corpus {
			if entry.State != key && entry.State != in.State && entry.State != view.Name {
				continue
			}
			then, now := entry.PromptHash, in.PromptHash
			if entry.PromptShapeHash != "" {
				then, now = entry.PromptShapeHash, in.PromptShapeHash
			}
			if then != "" && now != "" && then != now {
				report.Stale = append(report.Stale, entry)
			}
		}
		examples := view.CheckExamples()
		names := make([]string, 0, len(examples))
		for name := range examples {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for i, ex := range examples[name] {
				evaluate(ex.Output, name, ex.Refusal, fmt.Sprintf("example %d", i), false, ex.Observations...)
			}
		}
		valid := cfg.Valid[key]
		if len(valid) == 0 {
			valid = cfg.Valid[in.State]
		}
		if len(valid) == 0 {
			valid = cfg.Valid[view.Name]
		}
		hasRecording := false
		for _, entry := range cfg.Corpus {
			if entry.State == key || entry.State == in.State || entry.State == view.Path {
				hasRecording = true
				break
			}
		}
		if len(valid) == 0 && !hasRecording {
			report.addCoverage(key + ": fixture gap: no known-good corpus entries")
		}
		for i, raw := range valid {
			label := fmt.Sprintf("corpus %d", i)
			evaluate(raw, "", "", label, false)
			var value any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if decoder.Decode(&value) != nil {
				continue
			}
			// These parser variants do not exercise semantic invariance after typed
			// decoding. Semantic coverage requires author-declared Rewrites.
			parserVariants := []json.RawMessage{orderedJSON(value, true)}
			var pretty bytes.Buffer
			if json.Indent(&pretty, raw, "", "  ") == nil {
				parserVariants = append(parserVariants, pretty.Bytes())
			}
			for j, variant := range parserVariants {
				evaluate(variant, "", "", fmt.Sprintf("%s parser variant %d", label, j), false)
			}
			var rewrites []json.RawMessage
			f := cfg.Rewrites[key]
			if f == nil {
				f = cfg.Rewrites[view.Name]
			}
			if f != nil {
				rewrites = append(rewrites, f(append(json.RawMessage{}, raw...))...)
			}
			for j, rewrite := range rewrites {
				evaluate(rewrite, "", "", fmt.Sprintf("%s rewrite %d", label, j), false)
			}
			for j, mutation := range fieldMutations(value) {
				evaluate(mutation, "", "", fmt.Sprintf("%s mutation %d: %s", label, j, mutation), true)
			}
		}
	}
	seeds := cfg.Seeds
	if seeds == 0 {
		seeds = 16
	}
	simulation := withRecordedCorpus(cfg)
	for seed := 0; seed < seeds; seed++ {
		result := cfg.run(factory, "calibration discovery", FuzzConfig{Seed: int64(seed), MaxSteps: cfg.MaxSteps, Valid: simulation.Valid, Artifacts: cfg.Artifacts, FailPercent: -1, SchemaPercent: -1, probe: probe})
		if result == nil {
			break
		}
		if result.Finding != nil && result.Finding.Rule == "definition-panic" {
			report.addReal(result.Finding.String())
		}
		if len(seen) == len(views) {
			break
		}
	}
	for _, key := range keys {
		if !seen[key] {
			if cfg.budget != nil && cfg.budget.seen["calibration discovery"] {
				cfg.budget.take("calibration state " + key)
			} else {
				report.addCoverage(key + ": fixture gap: calibration never reached this state")
			}
		}
	}
	sort.Strings(report.RealProblems)
	sort.Strings(report.CoverageProblems)
	sort.Strings(report.ContextUnknown)
	sort.Strings(report.Problems)
	sort.Strings(report.NotChecked)
	sort.Strings(report.TypeRejected)
	return report
}

func orderedJSON(value any, reverse bool) json.RawMessage {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if reverse {
			sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		}
		var b bytes.Buffer
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			raw, _ := json.Marshal(k)
			b.Write(raw)
			b.WriteByte(':')
			b.Write(orderedJSON(v[k], reverse))
		}
		b.WriteByte('}')
		return b.Bytes()
	case []any:
		var b bytes.Buffer
		b.WriteByte('[')
		for i, x := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(orderedJSON(x, reverse))
		}
		b.WriteByte(']')
		return b.Bytes()
	default:
		raw, _ := json.Marshal(v)
		return raw
	}
}

// Each mutation changes a single field or removes a single row, including
// nested fields. Accepted mutations are diagnostics, not presumed defects.
func fieldMutations(value any) []json.RawMessage {
	var out []json.RawMessage
	emit := func(v any) { out = append(out, orderedJSON(v, false)) }
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			clone := func() map[string]any {
				m := map[string]any{}
				for key, x := range v {
					m[key] = x
				}
				return m
			}
			m := clone()
			delete(m, k)
			emit(m)
			for _, replacement := range []any{nil, "", "__outside_enum__"} {
				m = clone()
				m[k] = replacement
				emit(m)
			}
			for _, child := range fieldMutations(v[k]) {
				m = clone()
				m[k] = json.RawMessage(child)
				emit(m)
			}
		}
	case []any:
		for i := range v {
			row := append([]any{}, v[:i]...)
			row = append(row, v[i+1:]...)
			emit(row)
			for _, child := range fieldMutations(v[i]) {
				row = append([]any{}, v...)
				row[i] = json.RawMessage(child)
				emit(row)
			}
		}
	}
	return out
}

func (r *CalibrationReport) addReal(problems ...string) {
	r.RealProblems = append(r.RealProblems, problems...)
	r.Problems = append(r.Problems, problems...)
}
func (r *CalibrationReport) addCoverage(problems ...string) {
	r.CoverageProblems = append(r.CoverageProblems, problems...)
	r.Problems = append(r.Problems, problems...)
}
