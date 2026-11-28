package wf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

var errResultRunPaths = fmt.Errorf("%w: leave run-owned paths out; the engine already records your artifacts", ErrRejected)
var errResultWorkspacePath = fmt.Errorf("%w: give this path relative to the repository root", ErrRejected)
var errResultAmbiguous = fmt.Errorf("%w: give each field once", ErrRejected)

// resultAmbiguousError refuses a result that gives a field more than once,
// exactly or differing only in case: Go reads either spelling into
// one struct field, and which one wins depends on member order, which an
// engine re-encoding cannot keep. It is errResultAmbiguous under errors.Is.
type resultAmbiguousError struct{ field, first, again string }

func (e *resultAmbiguousError) Unwrap() error { return errResultAmbiguous }

func (e *resultAmbiguousError) Error() string {
	field := e.field
	if field == "" {
		field = "the result"
	} else {
		field = "result field " + field
	}
	spelled := fmt.Sprintf("%q twice", e.first)
	if e.first != e.again {
		spelled = fmt.Sprintf("%q and %q, which differ only in case and decode into one field", e.first, e.again)
	}
	return fmt.Sprintf("%s: %s gives a key more than once (%s): give each field once, with one spelling", ErrRejected, field, spelled)
}

// resultPathError is a refusal of one path in a result: the field
// that holds it, the path, and what to give instead. It is its kind's
// sentinel (errResultRunPaths or errResultWorkspacePath) under errors.Is.
type resultPathError struct {
	kind         error
	field, value string
	instead      string
}

func (e *resultPathError) Unwrap() error { return e.kind }

func (e *resultPathError) Error() string {
	field := e.field
	if field == "" {
		field = "the result"
	}
	if strings.HasPrefix(e.value, "$wf.") {
		return fmt.Sprintf("%s: result field %s holds the engine's path reference form (%s): give a plain value instead, such as the recorded artifact path itself; the engine records citations in that form on its own", ErrRejected, field, e.value)
	}
	if e.kind == errResultWorkspacePath {
		return fmt.Sprintf("%s: result field %s gives the absolute path %s: give this path relative to the repository root instead: %s", ErrRejected, field, e.value, e.instead)
	}
	return fmt.Sprintf("%s: result field %s names %s, a path in this run's state directory that the engine has not recorded as an artifact of this run: leave run-owned paths out; the engine already records your artifacts. To cite evidence, give instead the recorded path of an artifact an earlier step of this run produced, or of a file inside it", ErrRejected, field, e.value)
}

// Already durable submissions retain their original replay semantics.
func (c *cycle) submissionResultPaths(s Submission) error {
	in := c.st.insts[s.Child]
	if in != nil && s.Child == "" {
		in = c.byToken(s.Token, in)
	}
	if in != nil && in.Undecided != nil && in.Undecided.Token == s.Token {
		return nil
	}
	return c.resultRunPaths(s.Output)
}

func (c *cycle) unitResultPaths(name string, raw json.RawMessage) error {
	token, _ := c.st.tokenForName(name)
	return c.submissionResultPaths(Submission{Token: token, Output: raw})
}

// resultRunPaths rejects nonportable result values before they enter the
// journal. Artifact metadata is engine-owned and is not part of the result.
// Citing is not claiming: a value that is an artifact the engine
// allocated for an accepted step of this run, or a path inside one, is
// evidence the result points to and stays (journaled as a run-relative
// reference, citationsMarked); any other path in the state directory is
// refused, and so is the engine's own path-marker form.
func (c *cycle) resultRunPaths(raw json.RawMessage) error {
	if c.e.recordedReplay {
		return nil
	}
	// Keep existing malformed-output classification and retry behavior.
	if !json.Valid(raw) {
		return nil
	}
	w, err := c.citationWalk()
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return w.value(d, "")
}

// citationWalk is the walk of a result against this run's roots and the
// artifacts it may cite.
func (c *cycle) citationWalk() (resultPathWalk, error) {
	state := c.e.opts.StateRoot
	if state == "" && len(c.recs) > 0 && c.recs[0].Roots != nil {
		state = c.recs[0].Roots.State
	}
	var roots []string
	for _, root := range []string{state, c.workspace()} {
		if root == "" {
			roots = append(roots, "")
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return resultPathWalk{}, err
		}
		roots = append(roots, abs)
	}
	return resultPathWalk{roots: roots, recorded: c.recordedArtifacts(roots[0])}, nil
}

// recordedArtifacts are the absolute paths a result may cite: the
// artifacts the engine allocated for a step of this run whose submission it
// accepted. An allocated artifact is journaled at its allocated path whatever
// the submission said (agentOps.submit), so its identity is the engine's; a
// path a caller supplied for any other name, or a submission that was not
// accepted, proves nothing and is not citable.
func (c *cycle) recordedArtifacts(state string) []string {
	var out []string
	pending := map[string]JournalRecord{} // instance -> its latest submitted record
	for _, r := range c.recs {
		switch r.Kind {
		case JournalSubmitted:
			pending[r.Instance] = r
		case JournalAccepted:
			sub, ok := pending[r.Instance]
			delete(pending, r.Instance)
			if !ok || sub.State != r.State {
				continue
			}
			var d SubmittedData
			if json.Unmarshal(sub.Data, &d) != nil {
				continue
			}
			allocated := map[string]bool{}
			if in := c.st.insts[r.Instance]; in != nil && in.def != nil {
				if n := in.def.find(r.State); n != nil {
					for _, a := range allocatedOf(n) {
						allocated[a.name] = true
					}
				}
			}
			for name, a := range d.Artifacts {
				if !allocated[name] {
					continue // caller-supplied metadata, not an allocation
				}
				p := a.Path
				if state != "" {
					p = strings.ReplaceAll(p, statePathToken, state)
				}
				if filepath.IsAbs(p) {
					out = append(out, filepath.Clean(p))
				}
			}
		}
	}
	return out
}

// citationsMarked is raw with every citation (a string resultPathWalk.cites)
// inside the run directory replaced by a run-relative path reference, so the
// journal relocates with the run (snapshot forks, fanout) and readers resolve
// it against the run's own directory (decodeOwnedPathValues).
func (c *cycle) citationsMarked(raw json.RawMessage) (json.RawMessage, error) {
	runDir := c.resultDir()
	if c.e.recordedReplay || runDir == "" || !json.Valid(raw) {
		return raw, nil
	}
	runDir, err := filepath.Abs(runDir)
	if err != nil {
		return nil, err
	}
	w, err := c.citationWalk()
	if err != nil {
		return nil, err
	}
	var wire any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&wire); err != nil {
		return nil, err
	}
	changed := false
	wire, err = transformPathJSON(wire, func(value any) (any, bool, error) {
		s, ok := value.(string)
		if !ok || !w.cites(s) {
			return nil, false, nil
		}
		rel, err := filepath.Rel(runDir, filepath.Clean(s))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, false, nil
		}
		changed = true
		return ownedPath{Base: RunRelative, Path: filepath.ToSlash(rel), Cite: true}.value(), true, nil
	})
	if err != nil || !changed {
		return raw, err
	}
	return json.Marshal(wire)
}

// resultPathWalk walks a result's values with the field path of each.
type resultPathWalk struct {
	roots    []string // the state root and the workspace, absolute ("" when unknown)
	recorded []string // recordedArtifacts
}

// cites reports whether value is a recorded artifact of this run or a path
// inside one.
func (w resultPathWalk) cites(value string) bool {
	if !filepath.IsAbs(value) {
		return false
	}
	v := filepath.Clean(value)
	for _, a := range w.recorded {
		if v == a || strings.HasPrefix(v, a+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// value walks tokens rather than a map so duplicate keys cannot hide an
// earlier string value. Object keys themselves are not result values.
func (w resultPathWalk) value(d *json.Decoder, field string) error {
	token, _ := d.Token() // resultRunPaths validated the complete JSON document.
	switch value := token.(type) {
	case string:
		// A run-owned path is judged as one even when the state directory is
		// inside the workspace (.superpowers/state): a citation stands.
		if w.roots[0] != "" && strings.Contains(value, w.roots[0]) {
			if w.cites(value) {
				return nil
			}
			return &resultPathError{kind: errResultRunPaths, field: field, value: value}
		}
		if w.roots[1] != "" && filepath.IsAbs(value) {
			relative, err := filepath.Rel(w.roots[1], value)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
				return &resultPathError{kind: errResultWorkspacePath, field: field, value: value, instead: filepath.ToSlash(relative)}
			}
		}
	case json.Delim:
		switch value {
		case '{':
			var seen []string // the keys so far, in order
			for d.More() {
				key, _ := d.Token()
				name, _ := key.(string)
				// encoding/json matches a field by Unicode simple case
				// folding (so "ſtatus" is "status"): strings.EqualFold.
				for _, first := range seen {
					if strings.EqualFold(first, name) {
						return &resultAmbiguousError{field: field, first: first, again: name}
					}
				}
				seen = append(seen, name)
				if strings.HasPrefix(name, "$wf.") {
					// The engine's own path-marker form: a result gives plain
					// values, and the engine marks its citations itself.
					return &resultPathError{kind: errResultRunPaths, field: field, value: name}
				}
				sub := name
				if field != "" {
					sub = field + "." + name
				}
				if err := w.value(d, sub); err != nil {
					return err
				}
			}
			_, _ = d.Token()
		case '[':
			for i := 0; d.More(); i++ {
				if err := w.value(d, fmt.Sprintf("%s[%d]", field, i)); err != nil {
					return err
				}
			}
			_, _ = d.Token()
		}
	}
	return nil
}
