package wf

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
)

// PathRoots records origin anchors once, for callers that resume in place.
// A relocated caller supplies Workspace and StateRoot; explicitly owned path
// fields use those roots. Domain data and pinned harness evidence stay unchanged.
type PathRoots struct{ Workspace, State string }

const workspacePathToken = "\x00wf-workspace\x00"
const statePathToken = "\x00wf-state\x00"
const literalNullToken = "\x00wf-literal-null\x00"

// rootedJournal keeps path transport outside the fold, checks and adapters.
// The per-run cache lives only while that journal's lock is held.
type rootedJournal struct {
	Journal
	workspace, state string
	mu               sync.Mutex
	active           map[RunID]PathRoots
	// locks counts each run's held leases: a run's roots are dropped when
	// the last one is let go, not by one call while another holds the run
	//.
	locks map[RunID]int
}

func (j *rootedJournal) Lock(run RunID) (func(), error) {
	unlock, err := j.Journal.Lock(run)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	if j.locks == nil {
		j.locks = map[RunID]int{}
	}
	j.locks[run]++
	j.mu.Unlock()
	return func() {
		j.mu.Lock()
		if j.locks[run]--; j.locks[run] <= 0 {
			delete(j.locks, run)
			delete(j.active, run)
		}
		j.mu.Unlock()
		unlock()
	}, nil
}

// othersWaiting and yieldLock pass through to a journal that can yield its
// lock; the run's roots stay as they are, since the run stays this call's.
func (j *rootedJournal) othersWaiting(run RunID) bool {
	y, ok := j.Journal.(lockYielder)
	return ok && y.othersWaiting(run)
}

func (j *rootedJournal) yieldLock(run RunID) error {
	y, ok := j.Journal.(lockYielder)
	if !ok {
		return nil
	}
	return y.yieldLock(run)
}

func (j *rootedJournal) set(run RunID, roots PathRoots) {
	j.mu.Lock()
	j.active[run] = roots
	j.mu.Unlock()
}

func (j *rootedJournal) Records(run RunID) ([]JournalRecord, error) {
	recs, err := j.Journal.Records(run)
	roots := PathRoots{Workspace: j.workspace, State: j.state}
	if err != nil {
		j.set(run, roots)
		return nil, err
	}
	out, roots, err := j.rooted(recs)
	j.set(run, roots)
	return out, err
}

// ReadCommitted is Records with no lock, for a journal that can read so; a
// journal that cannot is errNoLockFreeRead, and is read under its lock. The
// read's roots stay its own: it holds nothing, so it records none.
func (j *rootedJournal) ReadCommitted(run RunID) ([]JournalRecord, error) {
	cr, ok := j.Journal.(committedReader)
	if !ok {
		return nil, errNoLockFreeRead
	}
	recs, err := cr.ReadCommitted(run)
	if err != nil {
		return nil, err
	}
	out, _, err := j.rooted(recs)
	return out, err
}

// errNoLockFreeRead is a journal that reads a run only under its lock.
var errNoLockFreeRead = errors.New("journal reads no run without its lock")

// rooted carries recs' paths from the roots they were recorded under to
// this journal's, and returns those roots.
func (j *rootedJournal) rooted(recs []JournalRecord) (_ []JournalRecord, roots PathRoots, err error) {
	roots = PathRoots{Workspace: j.workspace, State: j.state}
	var origin PathRoots
	if len(recs) > 0 {
		if recs[0].Roots != nil {
			origin = *recs[0].Roots
		} else {
			var started RunStartedData
			_ = json.Unmarshal(recs[0].Data, &started)
			origin.Workspace = started.Workspace
			if dir := recordedRunDir(recs, ""); dir != "" {
				origin.State = filepath.Dir(filepath.Dir(dir))
			}
		}
	}
	if roots.Workspace == "" {
		roots.Workspace = origin.Workspace
	}
	out := append([]JournalRecord(nil), recs...)
	if len(out) > 0 && out[0].Roots == nil {
		out[0].Roots = &origin
	}
	for i := range out {
		convert := func(s string) string {
			if out[i].RelativePaths {
				return strings.NewReplacer(workspacePathToken, roots.Workspace, statePathToken, roots.State, literalNullToken, "\x00").Replace(s)
			}
			// Older journals can also move when both original roots are known.
			return relocateString(s, origin, roots)
		}
		out[i].Data, err = mapJournalPaths(out[i].Kind, out[i].Data, convert)
		if err != nil {
			return nil, roots, err
		}
	}
	return out, roots, nil
}

func (j *rootedJournal) Append(rec JournalRecord) error {
	j.mu.Lock()
	roots := j.active[rec.Run]
	j.mu.Unlock()
	if rec.Kind == JournalRunStarted {
		var started RunStartedData
		if err := json.Unmarshal(rec.Data, &started); err != nil {
			return err
		}
		if roots.Workspace == "" {
			roots.Workspace = started.Workspace
		}
		if roots.State == "" {
			roots.State = j.state
		}
		rec.Roots = &PathRoots{Workspace: roots.Workspace, State: roots.State}
		j.set(rec.Run, roots)
	}
	convert := func(s string) string {
		s = strings.ReplaceAll(s, "\x00", literalNullToken)
		// State directories often live inside the workspace; encode them first.
		if len(roots.State) >= len(roots.Workspace) {
			return replaceRoot(replaceRoot(s, roots.State, statePathToken), roots.Workspace, workspacePathToken)
		}
		return replaceRoot(replaceRoot(s, roots.Workspace, workspacePathToken), roots.State, statePathToken)
	}
	var err error
	rec.Data, err = mapJournalPaths(rec.Kind, rec.Data, convert)
	if err != nil {
		return err
	}
	rec.RelativePaths = true
	return j.Journal.Append(rec)
}

// replaceRoot replaces a path prefix, never text containing a path.
func replaceRoot(path, root, replacement string) string {
	if root == "" || root == replacement {
		return path
	}
	if path == root {
		return replacement
	}
	if strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") {
		return replacement + strings.TrimPrefix(path, strings.TrimSuffix(root, "/"))
	}
	return path
}

// mapJournalPaths is the complete relocation schema. Everything not listed is
// opaque history, including params, outputs, prompts, adapter arguments/results,
// and harness roots/transcripts. These strings have no path type and must never
// be inferred to be paths from their contents. New owned path fields must be
// explicitly added here with a regression test.
func mapJournalPaths(kind JournalKind, raw json.RawMessage, convert func(string) string) (json.RawMessage, error) {
	var paths [][]string
	switch kind {
	case JournalRunStarted:
		paths = [][]string{{"Workspace"}, {"Assets"}, {"AssetRoots", "*"}, {"Worktrees", "*"}}
	case JournalMigrated:
		paths = [][]string{{"Assets"}, {"AssetRoots", "*"}}
	case JournalInstructionIssued:
		paths = [][]string{{"Workspace"}}
	case JournalSubmitted:
		paths = [][]string{{"Artifacts", "*", "Path"}}
	case JournalEffectIntent:
		var intent struct{ Command bool }
		if err := json.Unmarshal(raw, &intent); err != nil {
			return nil, err
		}
		if !intent.Command {
			return raw, nil
		}
		paths = [][]string{{"Effect", "Args", "dir"}}
	case JournalPromptFetched:
		paths = [][]string{{"PromptDir"}}
	default:
		return raw, nil
	}
	for _, path := range paths {
		var err error
		raw, err = mapPathField(raw, path, convert)
		if err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func mapPathField(raw json.RawMessage, path []string, convert func(string) string) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return raw, nil
	}
	if len(path) == 0 {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		next := convert(value)
		if next == value {
			return raw, nil
		}
		return json.Marshal(next)
	}
	if path[0] == "*" && strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		changed := false
		for i, item := range items {
			next, err := mapPathField(item, path[1:], convert)
			if err != nil {
				return nil, err
			}
			if string(next) != string(item) {
				items[i], changed = next, true
			}
		}
		if !changed {
			return raw, nil
		}
		return json.Marshal(items)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	changed := false
	for key, value := range fields {
		if path[0] != "*" && key != path[0] {
			continue
		}
		next, err := mapPathField(value, path[1:], convert)
		if err != nil {
			return nil, err
		}
		if string(next) != string(value) {
			fields[key] = next
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(fields)
}

func (j *rootedJournal) lastHolder(run RunID) (LockHolder, bool) {
	if h, ok := j.Journal.(interface {
		lastHolder(RunID) (LockHolder, bool)
	}); ok {
		return h.lastHolder(run)
	}
	return LockHolder{}, false
}

func rootedFileJournal(state, workspace string) Journal {
	state, _ = filepath.Abs(state)
	if workspace != "" {
		workspace, _ = filepath.Abs(workspace)
	}
	return &rootedJournal{Journal: FileJournal(state), workspace: workspace, state: state, active: map[RunID]PathRoots{}}
}

// Encode before decoding so new roots cannot be mistaken for old roots.
func relocateString(s string, from, to PathRoots) string {
	s = strings.ReplaceAll(s, "\x00", literalNullToken)
	if len(from.State) >= len(from.Workspace) {
		s = replaceRoot(replaceRoot(s, from.State, statePathToken), from.Workspace, workspacePathToken)
	} else {
		s = replaceRoot(replaceRoot(s, from.Workspace, workspacePathToken), from.State, statePathToken)
	}
	return strings.NewReplacer(workspacePathToken, to.Workspace, statePathToken, to.State, literalNullToken, "\x00").Replace(s)
}

// ReadJournal reads a consistent, decoded snapshot of a stored run. stateDir
// locates its state files; the recorded workspace root is used. On-disk path
// tokens are an internal representation: consumers should use this reader
// rather than FileJournal.Records or unmarshalling journal.jsonl themselves.
func ReadJournal(stateDir string, run RunID) ([]JournalRecord, error) {
	return ReadJournalWithRoots(stateDir, run, PathRoots{})
}

// ReadJournalWithRoots reads a moved run using explicit current path roots.
// Empty Workspace keeps the recorded workspace; empty State uses stateDir.
// It preserves historical data and pinned harness evidence exactly as the
// engine does. Reading does not run adapters or repair worktree links.
func ReadJournalWithRoots(stateDir string, run RunID, roots PathRoots) ([]JournalRecord, error) {
	if roots.State == "" {
		roots.State = stateDir
	}
	state, err := filepath.Abs(roots.State)
	if err != nil {
		return nil, err
	}
	workspace := roots.Workspace
	if workspace != "" {
		workspace, err = filepath.Abs(workspace)
		if err != nil {
			return nil, err
		}
	}
	j := &rootedJournal{Journal: FileJournal(stateDir), workspace: workspace, state: state, active: map[RunID]PathRoots{}}
	unlock, err := j.Lock(run)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return j.Records(run)
}

func (j *rootedJournal) recordedHolder(run RunID) (LockHolder, bool) {
	if h, ok := j.Journal.(interface {
		recordedHolder(RunID) (LockHolder, bool)
	}); ok {
		return h.recordedHolder(run)
	}
	return LockHolder{}, false
}

func (j *rootedJournal) lockState(run RunID) LockState {
	if h, ok := j.Journal.(interface{ lockState(RunID) LockState }); ok {
		return h.lockState(run)
	}
	return LockState{}
}

func (j *rootedJournal) heldNow(run RunID) (held, known bool) {
	h, ok := j.Journal.(interface{ heldNow(RunID) (bool, bool) })
	if !ok {
		return false, true
	}
	return h.heldNow(run)
}
