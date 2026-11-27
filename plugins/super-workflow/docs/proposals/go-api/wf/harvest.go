package wf

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CorpusEntry is an accepted agent output with its source journal location.
// State uses child definition paths with the root definition omitted, matching
// instruction states and wftest fixture keys.
// PromptHash is absent for journals written before prompt hashes were recorded.
type CorpusEntry struct {
	Context         *CheckContexts `json:",omitempty"`
	PromptShapeHash string         `json:",omitempty"`
	State           string
	Output          json.RawMessage
	PromptHash      string `json:",omitempty"`
	Run             RunID
	Seq             int64
	Instance        string `json:",omitempty"`
}

// Harvest writes accepted agent outputs to dir. Filenames derive only from
// provenance, never from journal-controlled path components. Repeating a harvest
// is harmless; differing existing entries are reported and never overwritten.
// Agent identity requires an issued instruction or a recorded agent shape;
// unclassified legacy accepted records are skipped.
func Harvest(records []JournalRecord, dir string) ([]CorpusEntry, error) {
	type issueKey struct {
		run             RunID
		instance, state string
		attempt         int
	}
	type instanceKey struct {
		run RunID
		id  string
	}
	type definitionPath struct{ name, path string }
	definitions := map[instanceKey]definitionPath{}
	shapes := map[RunID]map[string]string{}
	issued := map[issueKey]bool{}
	hashes := map[issueKey]string{}
	shapesHashes := map[issueKey]string{}
	var entries []CorpusEntry
	for _, r := range records {
		key := issueKey{r.Run, r.Instance, r.State, r.Attempt}
		switch r.Kind {
		case JournalRunStarted:
			var d RunStartedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return nil, fmt.Errorf("harvest seq %d: %w", r.Seq, err)
			}
			definitions[instanceKey{r.Run, ""}] = definitionPath{name: d.Def}
			shapes[r.Run] = d.Shape
		case JournalChildSpawned:
			var d ChildSpawnedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return nil, fmt.Errorf("harvest seq %d: %w", r.Seq, err)
			}
			parent := definitions[instanceKey{r.Run, r.Instance}]
			path := d.Def
			if parent.path != "" {
				path = parent.path + "/" + d.Def
			}
			definitions[instanceKey{r.Run, d.ID}] = definitionPath{name: d.Def, path: path}
		case JournalInstructionIssued:
			var d struct{ PromptHash, PromptShapeHash string }
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return nil, fmt.Errorf("harvest seq %d: %w", r.Seq, err)
			}
			hashes[key] = d.PromptHash
			shapesHashes[key] = d.PromptShapeHash
			issued[key] = true
		case JournalPromptFetched:
			var d struct{ PromptHash, PromptShapeHash string }
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return nil, fmt.Errorf("harvest seq %d: %w", r.Seq, err)
			}
			if d.PromptShapeHash != "" {
				shapesHashes[key] = d.PromptShapeHash
			}
			if d.PromptHash != "" {
				hashes[key] = d.PromptHash
			}
		case JournalAccepted:
			var d AcceptedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return nil, fmt.Errorf("harvest seq %d: %w", r.Seq, err)
			}
			def := definitions[instanceKey{r.Run, r.Instance}]
			shape := shapes[r.Run][def.name+"/"+r.State]
			isAgent := shape == "agent" || strings.HasPrefix(shape, "agent ")
			if d.Computed || (!issued[key] && !isAgent) {
				continue
			}
			if !json.Valid(d.Output) {
				return nil, fmt.Errorf("harvest seq %d: missing or invalid output", r.Seq)
			}
			state := r.State
			if def.path != "" {
				state = def.path + "/" + state
			}
			context, err := resolveCheckContexts(d.Context, r, records)
			if err != nil {
				return nil, fmt.Errorf("harvest seq %d: %w", r.Seq, err)
			}
			// A citation is a run-relative reference: the corpus holds it as
			// the path string it resolves to, as calibration decodes it
			//.
			output, err := resolveCitations(d.Output, harvestRunDir(records))
			if err != nil {
				return nil, fmt.Errorf("harvest seq %d: %w", r.Seq, err)
			}
			entries = append(entries, CorpusEntry{Context: context, State: state, Output: output, PromptHash: hashes[key], PromptShapeHash: shapesHashes[key], Run: r.Run, Seq: r.Seq, Instance: r.Instance})
			delete(hashes, key)
			delete(shapesHashes, key)
			delete(issued, key)
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, e := range entries {
		id, _ := json.Marshal(struct {
			Run RunID
			Seq int64
		}{e.Run, e.Seq})
		name := fmt.Sprintf("%x.json", sha256.Sum256(id))
		raw, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return nil, err
		}
		raw = append(raw, '\n')
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if os.IsExist(err) {
			info, statErr := root.Lstat(name)
			if statErr != nil {
				return nil, statErr
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("harvest: existing entry %s is not a regular file", name)
			}
			prior, readErr := root.ReadFile(name)
			if readErr != nil {
				return nil, readErr
			}
			if bytes.Equal(prior, raw) {
				continue
			}
			return nil, fmt.Errorf("harvest: existing entry %s differs", name)
		}
		if err != nil {
			return nil, err
		}
		_, writeErr := f.Write(raw)
		closeErr := f.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return entries, nil
}

// LoadCorpus reads harvested entries in deterministic filename order. Non-JSON
// files are ignored so explanatory files can live beside the corpus.
func LoadCorpus(dir string) ([]CorpusEntry, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var entries []CorpusEntry
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		if !f.Type().IsRegular() {
			return nil, fmt.Errorf("corpus: %s is not a regular file", f.Name())
		}
		raw, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, err
		}
		var e CorpusEntry
		if err = json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("corpus %s: %w", f.Name(), err)
		}
		if e.State == "" || !json.Valid(e.Output) {
			return nil, fmt.Errorf("corpus %s: missing state or output", f.Name())
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// allocationDir is the engine's allocation directory under records/artifacts:
// <slug>-<12 hex digits> (artifactDir).
var allocationDir = regexp.MustCompile(`-[0-9a-f]{12}$`)

// allocationRunDir is the run directory of an allocated artifact's path,
// stripping the allocation suffix (records/artifacts/<slug>-<hash>/...) from
// the end, so a RunDir layout that itself contains records/artifacts is not
// cut short.
func allocationRunDir(p string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	sep := string(filepath.Separator)
	marker := sep + filepath.Join("records", "artifacts") + sep
	for end := len(p); ; {
		i := strings.LastIndex(p[:end], marker)
		if i <= 0 {
			return "", false
		}
		next, _, _ := strings.Cut(p[i+len(marker):], sep)
		if allocationDir.MatchString(next) {
			return p[:i], true
		}
		end = i
	}
}

// harvestRunDir is the run directory the run's own records name, whatever
// layout the engine's RunDir gave it: an allocated artifact's path
// is <run dir>/records/artifacts/..., a fetched prompt's <run dir>/prompt/...;
// a path journaled under the state root is resolved with the recorded roots.
// "" when the records name none.
func harvestRunDir(records []JournalRecord) string {
	state := ""
	if len(records) > 0 && records[0].Roots != nil {
		state = records[0].Roots.State
	}
	resolve := func(p string) string {
		if state != "" {
			p = strings.ReplaceAll(p, statePathToken, state)
		}
		return filepath.Clean(p)
	}
	sep := string(filepath.Separator)
	for _, r := range records {
		if r.Kind != JournalSubmitted {
			continue
		}
		var d SubmittedData
		if json.Unmarshal(r.Data, &d) != nil {
			continue
		}
		for _, a := range d.Artifacts {
			p := resolve(a.Path)
			if dir, ok := allocationRunDir(p); ok {
				return dir
			}
		}
	}
	for _, r := range records {
		if r.Kind != JournalPromptFetched {
			continue
		}
		var d PromptFetchedData
		if json.Unmarshal(r.Data, &d) != nil || d.PromptDir == "" {
			continue
		}
		p := resolve(d.PromptDir)
		if i := strings.LastIndex(p, sep+"prompt"+sep); i > 0 && filepath.IsAbs(p) {
			return p[:i]
		}
	}
	return ""
}
