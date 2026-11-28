package wf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// CheckContexts records only reads actually made by an accepted submission's checks.
// Nil means unknown legacy context; version 1 with no checks is known empty.
type CheckContexts struct {
	Version int
	Checks  []RecordedCheck
}
type RecordedCheck struct {
	Name       string
	Reads      []RecordedRead
	Incomplete []string `json:",omitempty"`
}

// RecordedRead.Name identifies params/counters, or the canonical bound descriptor
// for an observation. Desc retains the unbound descriptor used by the check.
// RecordedPathView supplies roots for the historical view a check read. It is
// metadata for calibration, not a request to access those files during harvest.
type RecordedPathView struct {
	Workspace string `json:",omitempty"`
	RunDir    string `json:",omitempty"`
}

type RecordedRead struct {
	PathView    *RecordedPathView `json:",omitempty"`
	Declaration *ObservationDesc  `json:",omitempty"`
	Kind        string
	Def         string           `json:",omitempty"`
	State       string           `json:",omitempty"`
	Name        string           `json:",omitempty"`
	Desc        *ObservationDesc `json:",omitempty"`
	Instance    string           `json:",omitempty"`
	Seq         int64            `json:",omitempty"`
	Value       json.RawMessage  `json:",omitempty"`
	Error       string           `json:",omitempty"`
	ErrorKind   string           `json:",omitempty"`
}

type recordingCtx struct {
	declared []ObservationDesc
	live     *runCtx
	record   *RecordedCheck
}

func (x *recordingCtx) incomplete(reason string) {
	for _, s := range x.record.Incomplete {
		if s == reason {
			return
		}
	}
	x.record.Incomplete = append(x.record.Incomplete, reason)
}
func (x *recordingCtx) add(r RecordedRead) {
	key := func(r RecordedRead) string {
		r.Value = nil
		r.Seq = 0
		r.Error = ""
		r.ErrorKind = ""
		if r.Kind == "observed" {
			r.Name = ""
		}
		b, _ := json.Marshal(r)
		return string(b)
	}
	for _, old := range x.record.Reads {
		if key(old) == key(r) {
			a, _ := json.Marshal(old)
			b, _ := json.Marshal(r)
			if !bytes.Equal(a, b) {
				x.incomplete("conflicting " + r.Kind + " read")
			}
			return
		}
	}
	x.record.Reads = append(x.record.Reads, r)
}
func (x *recordingCtx) inline(r RecordedRead, v any) {
	b, err := json.Marshal(v)
	if err != nil || len(b) > 4096 {
		x.incomplete("unrecordable " + r.Kind + " read")
		return
	}
	r.Value = b
	x.add(r)
}
func (x *recordingCtx) Param(p Param) string {
	v := x.live.Param(p)
	x.inline(RecordedRead{Kind: "param", Name: p.name}, v)
	return v
}
func (x *recordingCtx) Count(c Counter) int {
	v := x.live.Count(c)
	x.inline(RecordedRead{Kind: "count", Name: c.name}, v)
	return v
}
func (x *recordingCtx) Attempt() int {
	v := x.live.Attempt()
	x.inline(RecordedRead{Kind: "attempt"}, v)
	return v
}
func (x *recordingCtx) Eval(b Budget) int {
	x.incomplete("unsupported eval read")
	return x.live.Eval(b)
}
func (x *recordingCtx) value(k ctxKey) (any, error) {
	v, err := x.live.value(k)
	r := RecordedRead{Kind: k.kind, Instance: x.live.in.ID, Def: x.live.in.def.name}
	switch k.kind {
	case "pathTemplate":
		r.Name = k.ref.(string)
		if err != nil {
			x.incomplete("failed pathTemplate read")
			return v, err
		}
		x.inline(r, v)
		return v, err
	case "input", "runDir":
		x.inline(r, v)
		return v, err
	case "output", "computed", "joined", "child", "childOutcome", "gateInt":
		n := k.ref.(*node)
		r.State = n.name
		r.Def = n.def.name
	case "observed":
		d := k.ref.(ObservationDesc)
		r.Desc = &d
		if bound, bindErr := x.live.c.scope(x.live.in).bindDesc(d); bindErr == nil {
			r.Name = canonDesc(bound)
			for _, declared := range x.declared {
				boundDeclaration, bindErr := x.live.c.scope(x.live.in).bindDesc(declared)
				if bindErr == nil && canonDesc(boundDeclaration) == r.Name {
					declaration := declared
					r.Declaration = &declaration
					break
				}
			}
		}
		r.State = x.live.in.Cur
		if r.Declaration == nil {
			x.incomplete("missing observation declaration")
		}
	default:
		x.incomplete("unsupported " + k.kind + " read")
		return v, err
	}
	if err != nil {
		r.Error = err.Error()
		if errors.Is(err, ErrUnavailable) {
			r.ErrorKind = "unavailable"
		} else {
			x.incomplete("unsupported error from " + k.kind)
			return v, err
		}
	}
	var raw json.RawMessage
	if v != nil {
		raw, _ = json.Marshal(v)
	}
	folded, isRaw := v.(json.RawMessage)
	if err == nil && (v == nil || (isRaw && len(folded) == 0)) {
		x.incomplete("absent producer for " + k.kind + " read")
		return v, err
	}
	for i := len(x.live.c.recs) - 1; i >= 0; i-- {
		j := x.live.c.recs[i]
		if j.Run != x.live.c.run || j.Instance != r.Instance || (r.Kind != "observed" && j.State != r.State) {
			continue
		}
		if r.Kind == "observed" {
			var od ObservedData
			if j.Kind != JournalObserved || json.Unmarshal(j.Data, &od) != nil {
				continue
			}
			bound, e := x.live.c.scope(x.live.in).bindDesc(*r.Desc)
			if e != nil || canonDesc(bound) != canonDesc(od.Desc) {
				continue
			}
			r.State = j.State
		}
		value, er, kind, ok := recordedProducer(j, r.Kind, x.live.c.resultDir())
		if !ok {
			continue
		}
		if r.Error != "" {
			if kind != r.ErrorKind || er == "" {
				continue
			}
		} else {
			r.PathView = nil
			view := value
			if r.Kind == "output" || r.Kind == "computed" || r.Kind == "child" || r.Kind == "joined" {
				var decodeErr error
				view, decodeErr = x.live.c.decodeOwnedPathValues(value)
				if decodeErr != nil {
					continue
				}
				roots := &RecordedPathView{Workspace: x.live.c.workspace(), RunDir: x.live.c.resultDir()}
				if roots.Workspace != "" {
					roots.Workspace, decodeErr = filepath.Abs(roots.Workspace)
					if decodeErr != nil {
						continue
					}
				}
				if roots.RunDir != "" {
					roots.RunDir, decodeErr = filepath.Abs(roots.RunDir)
					if decodeErr != nil {
						continue
					}
				}
				_, marked, decodeErr := recordedOwnedPathView(value, roots)
				if decodeErr != nil {
					continue
				}
				if marked {
					r.PathView = roots
				}
			}
			if !bytes.Equal(view, raw) {
				continue
			}
		}
		r.Seq = j.Seq
		x.add(r)
		return v, err
	}
	// Before the first answer, null is a known absence, not a missing producer.
	// Even a malformed answer record prevents this fallback.
	if r.Kind == "gateInt" && err == nil && bytes.Equal(raw, []byte("null")) {
		answered := false
		for _, j := range x.live.c.recs {
			if j.Run == x.live.c.run && j.Instance == r.Instance && j.State == r.State && j.Kind == JournalGateAnswered {
				answered = true
				break
			}
		}
		if !answered {
			r.Value = raw
			x.add(r)
			return v, err
		}
	}
	x.incomplete("missing producer for " + k.kind + " read")
	return v, err
}

// recordedProducer is the value j produced for a read of kind; an observed
// record's blob is loaded from runDir (a blob that cannot be read is no
// producer).
func recordedProducer(j JournalRecord, kind, runDir string) (json.RawMessage, string, string, bool) {
	switch kind {
	case "gateInt":
		if j.Kind == JournalGateAnswered {
			var d GateAnsweredData
			if json.Unmarshal(j.Data, &d) == nil {
				value, _ := json.Marshal(d.IntValue)
				return value, "", "", true
			}
		}
	case "output", "computed", "joined":
		if j.Kind == JournalAccepted {
			var d AcceptedData
			if json.Unmarshal(j.Data, &d) != nil || d.Computed != (kind != "output") {
				return nil, "", "", false
			}
			return d.Output, "", "", true
		}
		if j.Kind == JournalCompletedByObservation && kind == "output" {
			var d CompletedByObservationData
			if json.Unmarshal(j.Data, &d) == nil {
				return d.Output, "", "", true
			}
		}
	case "child", "childOutcome":
		if j.Kind == JournalChildSettled {
			var d ChildSettledData
			if json.Unmarshal(j.Data, &d) == nil && d.State == j.State {
				if kind == "childOutcome" {
					v, _ := json.Marshal(d.childOutcome())
					return v, "", "", true
				}
				return d.Result, "", "", true
			}
		}
	case "observed":
		if j.Kind == JournalObserved {
			var d ObservedData
			if json.Unmarshal(j.Data, &d) == nil {
				k := ""
				if d.Unavailable {
					k = "unavailable"
				}
				v, err := observedValue(runDir, d)
				if err != nil {
					return nil, "", "", false
				}
				return v, d.Error, k, true
			}
		}
	}
	return nil, "", "", false
}

// resolveCheckContexts materializes references only from earlier producers in
// the same run and instance. Invalid provenance is a corrupt corpus, not a zero.
// An observed producer's blob is read from runDir.
func resolveCheckContexts(context *CheckContexts, accept JournalRecord, records []JournalRecord, runDir string) (*CheckContexts, error) {
	if context == nil {
		return nil, nil
	}
	if context.Version > 1 {
		return nil, NewerFormat("check context", context.Version, 1)
	}
	if context.Version != 1 {
		return nil, fmt.Errorf("unsupported check context version %d", context.Version)
	}
	b, err := json.Marshal(context)
	if err != nil {
		return nil, fmt.Errorf("invalid check context: %w", err)
	}
	var out CheckContexts
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("invalid check context: %w", err)
	}
	defs := map[string]string{}
	for _, j := range records {
		if j.Run != accept.Run || j.Seq >= accept.Seq {
			continue
		}
		switch j.Kind {
		case JournalRunStarted:
			var d RunStartedData
			if json.Unmarshal(j.Data, &d) == nil {
				defs[""] = d.Def
			}
		case JournalChildSpawned:
			var d ChildSpawnedData
			if json.Unmarshal(j.Data, &d) == nil {
				defs[d.ID] = d.Def
			}
		}
	}
	for ci := range out.Checks {
		for ri := range out.Checks[ci].Reads {
			r := &out.Checks[ci].Reads[ri]
			if r.Seq == 0 {
				if r.PathView != nil {
					return nil, fmt.Errorf("unproven inline path view")
				}
				if len(r.Value) > 4096 || !json.Valid(r.Value) || r.Error != "" || r.ErrorKind != "" {
					return nil, fmt.Errorf("invalid inline %s context read", r.Kind)
				}
				switch r.Kind {
				case "gateInt":
					if !bytes.Equal(r.Value, []byte("null")) || r.Def == "" || defs[r.Instance] != r.Def || r.Instance != accept.Instance || r.State == "" {
						return nil, fmt.Errorf("invalid absent gateInt context read")
					}
					for _, j := range records {
						if j.Run == accept.Run && j.Seq < accept.Seq && j.Instance == r.Instance && j.State == r.State && j.Kind == JournalGateAnswered {
							return nil, fmt.Errorf("unproven absent gateInt context read: earlier answer %d", j.Seq)
						}
					}
				case "param", "runDir", "pathTemplate":
					if r.Kind == "pathTemplate" && !strings.HasPrefix(r.Name, pathTemplatePrefix) {
						return nil, fmt.Errorf("invalid inline pathTemplate identity")
					}
					var v string
					if (r.Kind == "param" && r.Name == "") || bytes.Equal(r.Value, []byte("null")) || json.Unmarshal(r.Value, &v) != nil {
						return nil, fmt.Errorf("invalid inline %s context read", r.Kind)
					}
				case "count", "attempt":
					var v int
					if bytes.Equal(r.Value, []byte("null")) || json.Unmarshal(r.Value, &v) != nil || (r.Kind == "count" && r.Name == "") {
						return nil, fmt.Errorf("invalid inline %s context read", r.Kind)
					}
				case "input":
				default:
					return nil, fmt.Errorf("unproven inline %s context read", r.Kind)
				}
				continue
			}
			if r.Seq < 0 || r.Seq >= accept.Seq || r.Def == "" || defs[r.Instance] != r.Def || r.Instance != accept.Instance {
				return nil, fmt.Errorf("invalid check context reference %d", r.Seq)
			}
			var producer *JournalRecord
			for i := range records {
				j := &records[i]
				if j.Run == accept.Run && j.Seq == r.Seq {
					if producer != nil {
						return nil, fmt.Errorf("duplicate context producer %d", r.Seq)
					}
					producer = j
				}
			}
			if producer == nil || producer.Instance != r.Instance || producer.State != r.State {
				return nil, fmt.Errorf("missing or mismatched context producer %d", r.Seq)
			}
			value, er, kind, ok := recordedProducer(*producer, r.Kind, runDir)
			if !ok || (r.ErrorKind != "" && r.ErrorKind != "unavailable") || kind != r.ErrorKind || (er != "") != (r.Error != "") {
				return nil, fmt.Errorf("invalid context producer kind/error %d", r.Seq)
			}
			if r.Kind == "observed" {
				var d ObservedData
				_ = json.Unmarshal(producer.Data, &d)
				if r.Desc == nil || r.Name == "" || r.Name != canonDesc(d.Desc) || r.Desc.Adapter != d.Desc.Adapter || r.Desc.Op != d.Desc.Op || r.Desc.Result != d.Desc.Result {
					return nil, fmt.Errorf("mismatched observation producer %d", r.Seq)
				}
			}
			if r.PathView != nil {
				resolved, marked, err := recordedOwnedPathView(value, r.PathView)
				if err != nil || !marked {
					return nil, fmt.Errorf("invalid context path view %d: %v", r.Seq, err)
				}
				value = resolved
			}
			if len(r.Value) > 0 && !bytes.Equal(r.Value, value) {
				return nil, fmt.Errorf("conflicting context value %d", r.Seq)
			}
			if r.Error == "" {
				if !json.Valid(value) {
					return nil, fmt.Errorf("invalid context producer value %d", r.Seq)
				}
				r.Value = append(json.RawMessage(nil), value...)
			}
		}
	}
	return &out, nil
}

// recordedOwnedPathView validates and resolves explicit markers lexically. A
// historical source may have been deleted or replaced; harvesting never probes
// its filesystem and does not apply live delivery's symlink checks.
func recordedOwnedPathView(raw json.RawMessage, roots *RecordedPathView) (json.RawMessage, bool, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false, err
	}
	markedAny := false
	resolved, err := transformPathJSON(value, func(value any) (any, bool, error) {
		p, marked, err := markedOwnedPath(value)
		if !marked || err != nil {
			return nil, marked, err
		}
		markedAny = true
		if p.Base == externalPathBase {
			return p.Path + p.Fragment, true, nil
		}
		if roots == nil {
			return nil, true, fmt.Errorf("missing historical path roots")
		}
		root := roots.Workspace
		if p.Base == RunRelative {
			root = roots.RunDir
		}
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return nil, true, fmt.Errorf("invalid historical %s root", p.Base)
		}
		return filepath.Join(root, filepath.FromSlash(p.Path)) + p.Fragment, true, nil
	})
	if err != nil {
		return nil, markedAny, err
	}
	if !markedAny {
		return raw, false, nil
	}
	result, err := json.Marshal(resolved)
	return result, true, err
}
