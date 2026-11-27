package wf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WorldSnapshotData names a complete checkpoint just before Before is appended.
// Directory is relative to the run directory; adapter metadata is portable.
type WorldSnapshotData struct {
	// Unavailable records a non-quiescent point; it is never a restorable world.
	Unavailable string `json:",omitempty"`
	Version     int
	Before      JournalKind
	Directory   string
	// Tree is a version-2 checkpoint's root tree in the run's snapshot
	// store (Directory); a version-1 checkpoint is the directory itself.
	Tree     string `json:",omitempty"`
	Adapters map[string]json.RawMessage
	// Omitted lists run-data entries left out by checkpoint path: .git
	// metadata from scratch checkouts and unsupported special files.
	Omitted []string `json:",omitempty"`
}

type snapshotAdapter struct {
	name string
	rt   WorldSnapshotter
}

func (a *Adapters) snapshotAdapters() []snapshotAdapter {
	var out []snapshotAdapter
	for op, rt := range a.ops {
		names := rt.Ops()
		if len(names) == 0 || op != names[0] {
			continue
		}
		if s, ok := rt.(WorldSnapshotter); ok {
			name, _, _ := strings.Cut(op, ".")
			if name == "*" {
				name = "world"
			}
			out = append(out, snapshotAdapter{name, s})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
func (e *Engine) snapshotRunDir(run RunID) (string, error) {
	if e.opts.RunDir == nil || e.opts.RunDir(run) == "" {
		return "", fmt.Errorf("%w: snapshots require a run directory", ErrUnavailable)
	}
	return realPath(e.opts.RunDir(run))
}
func snapshotRelative(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	return rel, err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// checkpoint journals the world capture taken just before the record of kind
// before. A fork of it is a crash-restart: a world with live
// children is captured as it stands, as a crash would leave it, and the fork
// loses those children as orphans (forkOrphans). Live children may write the
// world during the capture, so it must be a consistent cut: every adapter's
// stamp (WorldStamper) is the same before and after it, or the offer is
// journaled unforkable ("world changed during capture"); a capture that fails,
// live or not, is journaled unforkable with why instead of failing the run's
// verb, since a checkpoint is never a reason to stop a healthy run;
// and a live capture is never reused. A command
// in flight is not captured: its detached runner's process identity cannot
// be carried into a fork.
func (c *cycle) checkpoint(instance, state string, attempt int, before JournalKind) error {
	live := false
	for _, in := range c.st.insts {
		if in.Cmd != nil && in.Cmd.Result == nil {
			return c.emitAs(instance, state, attempt, JournalWorldSnapshot, 0, WorldSnapshotData{Version: 1, Before: before, Unavailable: "world has a command in flight"})
		}
		live = live || (in.Unit != nil && in.Unit.State.live())
	}
	if !live {
		err := c.capture(instance, state, attempt, before, true)
		var journal journalWriteError
		if err == nil || errors.As(err, &journal) || errors.Is(err, ErrCorruptJournal) {
			return err
		}
		c.captured = nil
		return c.emitAs(instance, state, attempt, JournalWorldSnapshot, 0, WorldSnapshotData{Version: 1, Before: before, Unavailable: "capture failed: " + err.Error()})
	}
	c.captured = nil
	if err := c.capture(instance, state, attempt, before, false); err != nil {
		var journal journalWriteError
		if errors.As(err, &journal) || errors.Is(err, ErrCorruptJournal) {
			return err
		}
		why := "capture with live children failed: " + err.Error()
		var moved worldMovedError
		if errors.As(err, &moved) {
			why = moved.Error()
		}
		return c.emitAs(instance, state, attempt, JournalWorldSnapshot, 0, WorldSnapshotData{Version: 1, Before: before, Unavailable: why})
	}
	return nil
}

// worldMovedError is a live capture whose adapter's stamp changed during it
// (WorldStamper): it holds no single instant of the world.
type worldMovedError struct{ adapter string }

func (e worldMovedError) Error() string { return "world changed during capture: " + e.adapter }

// journalWriteError is a capture's failure to journal itself: never retried
// as an unforkable record, since the write may have committed.
type journalWriteError struct{ error }

func (e journalWriteError) Unwrap() error { return e.error }

// capture takes, stores and journals one world capture; reuse lets it name
// this cycle's last capture again when nothing changed since, and keep this
// one for the next.
func (c *cycle) capture(instance, state string, attempt int, before JournalKind, reuse bool) error {
	if prev := c.captured; reuse && prev != nil && c.capturedStamp != "" && c.runDataStamp() == c.capturedStamp {
		reuse := *prev
		reuse.Before = before
		if err := c.emitAs(instance, state, attempt, JournalWorldSnapshot, 0, reuse); err != nil {
			return journalWriteError{err}
		}
		c.captured = prev
		return nil
	}
	dir, err := c.e.snapshotRunDir(c.run)
	if err != nil {
		return err
	}
	adapters := c.e.reg.snapshotAdapters()
	if len(adapters) == 0 {
		return fmt.Errorf("%w: no world snapshot adapters registered", ErrUnavailable)
	}
	// Worktree ownership comes from declared effects, never every checkout in
	// the repository (which may belong to another run).
	if _, statErr := os.Lstat(filepath.Join(c.workspace(), ".beads")); statErr == nil {
		haveTracker := false
		for _, a := range adapters {
			haveTracker = haveTracker || a.name == "bd"
		}
		if !haveTracker {
			return fmt.Errorf("%w: tracker workspace requires bd snapshot adapter", ErrUnavailable)
		}
	}
	seenNames := map[string]bool{}
	for _, a := range adapters {
		if seenNames[a.name] {
			return fmt.Errorf("%w: duplicate snapshot adapter %s", ErrUnavailable, a.name)
		}
		seenNames[a.name] = true
	}
	// A successor carries on in the worktrees its predecessors added and did
	// not remove: its recorded
	// set first, then this run's own effects.
	trees, err := c.seedWorktrees(map[RunID]bool{c.run: true})
	if err != nil {
		return err
	}
	if err := c.collectWorktrees(c.recs, trees); err != nil {
		return err
	}
	var worktrees, stateWorktrees, exclude []string
	for p := range trees {
		// State-owned checkouts remain state-owned even when the original
		// state directory was nested inside the workspace.
		if rel, ok := snapshotRelative(c.e.opts.StateRoot, p); c.e.opts.StateRoot != "" && ok {
			stateWorktrees = append(stateWorktrees, rel)
		} else if rel, ok := snapshotRelative(c.workspace(), p); ok {
			worktrees = append(worktrees, rel)
		} else {
			return fmt.Errorf("%w: snapshot worktree outside workspace and state root: %s", ErrUnavailable, p)
		}
	}
	sort.Strings(worktrees)
	sort.Strings(stateWorktrees)
	if rel, ok := snapshotRelative(c.workspace(), c.e.opts.StateRoot); ok {
		exclude = append(exclude, rel)
	}
	if err = os.MkdirAll(filepath.Join(dir, "snapshots"), 0700); err != nil {
		return err
	}
	store, err := createSnapshotStore(filepath.Join(dir, "snapshots", storeDirName))
	if err != nil {
		return err
	}
	// The adapters' own small files are staged here and stored with the
	// rest; what they hand over by Store is read in place.
	tmp, err := os.MkdirTemp(filepath.Join(dir, "snapshots"), "stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	capture := store.capture()
	request := func(name string) SnapshotRequest {
		return SnapshotRequest{Workspace: c.workspace(), StateRoot: c.e.opts.StateRoot, Directory: filepath.Join(tmp, name), Worktrees: worktrees, StateWorktrees: stateWorktrees, Exclude: exclude,
			Include: c.e.opts.SnapshotFiles, Store: storeMount{capture, name}}
	}
	for _, a := range adapters {
		if err = a.rt.SnapshotPreflight(request(a.name)); err != nil {
			return fmt.Errorf("snapshot %s preflight: %w", a.name, err)
		}
	}
	// With children live the capture must be one instant of the world: each
	// adapter's stamp is taken before any adapter reads and again after the
	// last, and a world that moved in between is not a checkpoint.
	stamps := func() (map[string]string, error) {
		out := map[string]string{}
		for _, a := range adapters {
			s, ok := a.rt.(WorldStamper)
			if !ok {
				return nil, fmt.Errorf("snapshot %s cannot validate a capture with live children", a.name)
			}
			if out[a.name], err = s.SnapshotStamp(request(a.name)); err != nil {
				return nil, fmt.Errorf("snapshot %s stamp: %w", a.name, err)
			}
		}
		return out, nil
	}
	var stampedBefore map[string]string
	if !reuse {
		if stampedBefore, err = stamps(); err != nil {
			return err
		}
	}
	data := WorldSnapshotData{Version: 2, Before: before, Directory: filepath.Join("snapshots", storeDirName), Adapters: map[string]json.RawMessage{}}
	for _, a := range adapters {
		if err = os.MkdirAll(filepath.Join(tmp, a.name), 0700); err != nil {
			return err
		}
		raw, err := a.rt.Snapshot(request(a.name))
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", a.name, err)
		}
		data.Adapters[a.name] = raw
		if err = capture.add(a.name, filepath.Join(tmp, a.name), nil, false); err != nil {
			return fmt.Errorf("snapshot %s: %w", a.name, err)
		}
	}
	// Run-owned artifacts also belong to this historical frontier; storing
	// their current versions during a later fork would import future results.
	if h := snapshotRunDataHook; h != nil {
		h("before-copy")
	}
	for _, sub := range []string{"records", "out", "prompt"} {
		if err = capture.addRunData("run-data/"+sub, filepath.Join(dir, sub)); err != nil {
			return err
		}
	}
	// The objects are synced before the durable journal points at them; an
	// object no record names is harmless.
	if data.Tree, err = capture.commit(data.Adapters); err != nil {
		return err
	}
	capture.cache.save()
	data.Omitted = capture.omittedPaths()
	if h := snapshotRunDataHook; h != nil {
		h("after-copy")
	}
	if !reuse {
		after, err := stamps()
		if err != nil {
			return err
		}
		for _, a := range adapters {
			if stampedBefore[a.name] != after[a.name] {
				return worldMovedError{a.name}
			}
		}
	}
	if err = c.emitAs(instance, state, attempt, JournalWorldSnapshot, 0, data); err != nil {
		return journalWriteError{err}
	}
	if !reuse {
		return nil
	}
	// What a later checkpoint compares the live run data against: the bytes
	// this capture stored, not another read of the source, which a writer
	// outside the lock may change during the capture and change back.
	c.captured, c.capturedStamp = &data, capture.runDataDigest()
	return nil
}

// storeMount is the Store of one adapter's request: rel is under the
// adapter's checkpoint directory.
type storeMount struct {
	c       *storeCapture
	adapter string
}

func (m storeMount) PutDir(rel, src string, skip func(name string) bool) error {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("snapshot store: unsafe path %q", rel)
	}
	return m.c.add(m.adapter+"/"+rel, src, skip, false)
}

// snapshotRunDataHook, when set by a test, runs just before a capture reads
// the run data and just after it is published.
var snapshotRunDataHook func(when string)

// runDataStamp is runDataDigest of the live run data; "" (never equal to a
// capture's) when the run has no run directory or a read fails.
func (c *cycle) runDataStamp() string {
	if c.e.opts.RunDir == nil || c.e.opts.RunDir(c.run) == "" {
		return ""
	}
	d, err := runDataDigest(c.e.opts.RunDir(c.run))
	if err != nil {
		return ""
	}
	return d
}

// runDataDigest digests the run-data trees under root (records, out,
// prompt) as the snapshot store holds them: every entry's root-relative
// path, type and permissions, and every regular file's content ids. A
// missing tree is recorded as absent; any other read or walk error is
// returned. The engine creates directories there while it issues units, and
// a file's bytes can change with no change to its stat fields, so only this,
// equal to the digest of what a capture stored, lets a checkpoint name that
// capture again.
func runDataDigest(root string) (string, error) {
	cache := hashCacheFor(filepath.Join(root, "snapshots", storeDirName))
	var lines []string
	buf := make([]byte, storeBlock)
	for _, sub := range []string{"records", "out", "prompt"} {
		dir, err := openSnapshotDir(filepath.Join(root, sub))
		if errors.Is(err, fs.ErrNotExist) {
			lines = append(lines, "absent "+sub)
			continue
		}
		if err != nil {
			return "", err
		}
		info, err := dir.Stat()
		if err != nil {
			dir.Close()
			return "", err
		}
		lines = append(lines, digestLine(sub, info.Mode(), nil))
		err = walkSnapshotDir(dir, sub, func(parent *os.File, rel string, info fs.FileInfo) (bool, error) {
			name := info.Name()
			rel = filepath.ToSlash(rel)
			if isGitName(name) {
				lines = append(lines, omittedLine(rel))
				return false, nil
			}
			var ids []string
			mode := info.Mode()
			switch {
			case mode.IsRegular():
				var err error
				if ids, err = fileIDs(parent, name, buf, cache); err != nil {
					return false, err
				}
			case mode&fs.ModeSymlink != 0:
				target, err := os.Readlink(snapshotEntryPath(parent, name))
				if err != nil {
					return false, err
				}
				ids = []string{gitObjectID("blob", []byte(target))}
				mode = fs.ModeSymlink | 0o777
			case !mode.IsDir():
				lines = append(lines, omittedLine(unsupportedRunDataPath(rel, mode)))
				return false, nil
			}
			lines = append(lines, digestLine(rel, mode, ids))
			return mode.IsDir(), nil
		})
		dir.Close()
		if err != nil {
			return "", err
		}
	}
	return digestLines(lines), nil
}

func unsupportedRunDataPath(path string, mode fs.FileMode) string {
	return fmt.Sprintf("%s (unsupported file %v)", path, mode.Type())
}

// omittedLine is a digest line for an entry a capture left out.
func omittedLine(rel string) string { return fmt.Sprintf("omitted %q", rel) }

func digestLine(rel string, mode fs.FileMode, ids []string) string {
	return fmt.Sprintf("%q %v %v %s", rel, mode.Type(), mode.Perm(), strings.Join(ids, ","))
}

func digestLines(lines []string) string {
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		fmt.Fprintln(h, l)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// worldUnchanged are the record kinds that only journal the engine's own
// progress: appending one changes nothing a world capture copies (the
// workspace, the adapters' state, the run's records, out and prompt trees).
// A checkpoint whose cycle appended only these since its last capture names
// that capture again. Every other kind (an effect, a command, a cleanup, an
// accepted result) may have changed the world, so the next checkpoint
// captures anew. Only a capture taken with no live child is reused, so
// nothing outside the cycle writes the world in between either.
var worldUnchanged = map[JournalKind]bool{
	JournalWorldSnapshot:     true,
	JournalTransition:        true,
	JournalInstructionIssued: true,
	JournalChildSpawned:      true,
	JournalObserved:          true,
	JournalHandedOut:         true,
	JournalHeartbeat:         true,
	JournalMetadataAttached:  true,
	JournalNote:              true,

	JournalCoordinatorAttached:    true,
	JournalCoordinatorDetached:    true,
	JournalCoordinatorHandoff:     true,
	JournalCoordinatorContinued:   true,
	JournalCoordinatorStopStalled: true,
}

func syncCheckpoint(root string, syncFile func(string) error) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		} // parent fsync persists the link itself
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("snapshot contains unsupported file %s", p)
		}
		return syncFile(p)
	})
}

// errForkUnrelocatable: a record of the fork's journal prefix holds a path
// that cannot be moved to the fork's roots. It refuses that fork only; a
// fanout runs its other forks.
var errForkUnrelocatable = errors.New("fork refused: its journal prefix cannot be relocated")

// forkRefusal is err as ForkSnapshot reports a failed relocation check: an
// untyped source path refuses this fork alone (errForkUnrelocatable); any
// other failure, a corrupt journal above all, is returned as it is, and
// stops a fanout before any coordinator starts.
func forkRefusal(err error) error {
	var untyped *untypedSourcePathError
	if errors.As(err, &untyped) {
		return fmt.Errorf("%w: %w", errForkUnrelocatable, err)
	}
	return err
}

// checkForkDestination checks out, a real path (realPath), as a place to
// create a fork or a batch of them before anything is written there: it is
// not in or at a source root, does not exist, and its nearest existing
// ancestor has no symbolic link on its way. A link the caller's path went
// through is resolved before this, so one into a source root is refused as
// the overlap it is; a link that appears after resolution is refused here.
// It returns that ancestor.
func checkForkDestination(out string, sources ...string) (string, error) {
	for _, source := range sources {
		if source != "" {
			_, inside := snapshotRelative(source, out)
			if inside || filepath.Clean(out) == filepath.Clean(source) {
				return "", fmt.Errorf("%w: fork destination overlaps source world", ErrRejected)
			}
		}
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		return "", fmt.Errorf("%w: fork destination must not exist: %s", ErrRejected, out)
	}
	ancestor := filepath.Dir(out)
	for {
		if _, statErr := os.Lstat(ancestor); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(ancestor)
			if resolveErr != nil || resolved != ancestor {
				return "", fmt.Errorf("%w: fork destination has a symlink ancestor", ErrRejected)
			}
			return ancestor, nil
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("%w: no fork destination ancestor", ErrRejected)
		}
		ancestor = parent
	}
}

// SnapshotFork describes an independent workspace and journal restored from a
// checkpoint. Run keeps its identifier inside the new StateDir.
type SnapshotFork struct {
	Run                 RunID
	Workspace, StateDir string
	Seq                 int64
	// Installed is the path in the fork's workspace the invoking build
	// InstalledFrom was installed at (EngineOptions.InstalledBinary); empty
	// when none was declared or the restored world already held it. It is
	// not the run's own binary: RunHash is the definition hash the run
	// recorded, BuildHash the invoking build's, and the fork is resumed on
	// this build with next --adopt.
	Installed, InstalledFrom string
	RunHash, BuildHash       string
}

// forkPrep is what forkPreflight checked and a fork restores from.
type forkPrep struct {
	prefix    []JournalRecord
	def       *Def
	st        *runState
	data      WorldSnapshotData
	sourceDir string
	adapters  map[string]WorldSnapshotter
	names     []string // the adapters to restore, in restore order
}

// forkPreflight runs every read-only check a fork of run at checkpoint seq
// makes before it writes anything: the checkpoint is a world snapshot taken
// with no command in flight, of a supported version, its journal prefix
// relocatable (an untyped source path is errForkUnrelocatable), its
// directory well formed and its adapters available. A fanout plan runs the
// same checks to choose offers a fork can take.
func (e *Engine) forkPreflight(run RunID, recs []JournalRecord, seq int64) (*forkPrep, error) {
	if seq < 1 || seq > int64(len(recs)) || recs[seq-1].Kind != JournalWorldSnapshot {
		return nil, fmt.Errorf("%w: no world snapshot at seq %d", ErrRejected, seq)
	}
	def, err := e.defFor(recs[0])
	if err != nil {
		return nil, err
	}
	prefix := append([]JournalRecord(nil), recs[:seq]...)
	st, err := foldRun(prefix, def)
	if err != nil {
		return nil, err
	}
	// Its live children are lost as orphans in the fork (forkOrphans); a
	// command in flight is not carried (checkpoint).
	for _, in := range st.insts {
		if in.Cmd != nil && in.Cmd.Result == nil {
			return nil, fmt.Errorf("%w: checkpoint has a command in flight", ErrUnavailable)
		}
	}
	var data WorldSnapshotData
	if err = json.Unmarshal(recs[seq-1].Data, &data); err != nil {
		return nil, err
	}
	if data.Unavailable != "" {
		return nil, fmt.Errorf("%w: snapshot unavailable: %s", ErrUnavailable, data.Unavailable)
	}
	if data.Version > 2 {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, NewerFormat("world snapshot", data.Version, 2))
	}
	if data.Version != 1 && data.Version != 2 {
		return nil, fmt.Errorf("%w: unsupported snapshot version", ErrUnavailable)
	}
	sourceDir, err := e.snapshotRunDir(run)
	if err != nil {
		return nil, err
	}
	if err = normalizeSnapshotParams(prefix, def, st.started.Workspace, sourceDir, e.opts.StateRoot); err != nil {
		return nil, err
	}
	if err = snapshotRelocatable(prefix, PathRoots{Workspace: st.started.Workspace, State: e.opts.StateRoot}); err != nil {
		return nil, forkRefusal(err)
	}
	if filepath.IsAbs(data.Directory) || filepath.Clean(data.Directory) != data.Directory || !strings.HasPrefix(data.Directory, "snapshots"+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: invalid checkpoint path", ErrCorruptJournal)
	}
	adapters := map[string]WorldSnapshotter{}
	for _, a := range e.reg.snapshotAdapters() {
		adapters[a.name] = a.rt
	}
	names := make([]string, 0, len(data.Adapters))
	for name := range data.Adapters {
		if name == "" || filepath.Base(name) != name || name == "." || name == ".." || adapters[name] == nil {
			return nil, fmt.Errorf("%w: snapshot adapter %q unavailable", ErrUnavailable, name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	// Git creates the workspace; the tracker is then restored inside it.
	sort.SliceStable(names, func(i, j int) bool { return names[i] == "git" && names[j] != "git" })
	return &forkPrep{prefix: prefix, def: def, st: st, data: data, sourceDir: sourceDir, adapters: adapters, names: names}, nil
}

// uninheritedCoordinator is a snapshot fork's note in place of its source's
// coordinator record: the fork inherits no coordinator, but a binding
// record's segment models stay, since history never re-resolves
// them from the live tier table.
func uninheritedCoordinator(r JournalRecord) JournalRecord {
	var co CoordinatorData
	_ = json.Unmarshal(r.Data, &co)
	note := NoteData{Text: "source coordinator not inherited by snapshot fork"}
	if co.Models != nil {
		note.SegmentStart, note.Models = co.SegmentStart, co.Models
	}
	r.Kind = JournalNote
	r.Data, _ = json.Marshal(note)
	return r
}

// ForkSnapshot restores a recorded historical world into a new output directory.
// The source is read only. A fork is a crash-restart: units live at the
// checkpoint have their children in the source world only, and the fork's
// first cycle loses them as orphans (forkOrphans). A checkpoint with a
// command in flight cannot be forked: a detached runner's process identity
// cannot be carried into an isolated world.
func (e *Engine) ForkSnapshot(run RunID, seq int64, out string) (SnapshotFork, error) {
	return e.forkSnapshot(run, seq, out, nil)
}

// forkSnapshot is ForkSnapshot; mark, when set, adds to the fork's rewound
// record (an operator-chosen target).
func (e *Engine) forkSnapshot(run RunID, seq int64, out string, mark func(*RewoundData)) (SnapshotFork, error) {
	if err := e.CheckBoundaryModelMap(run); err != nil {
		return SnapshotFork{}, err
	}
	var recs []JournalRecord
	if err := e.withRecords(run, func(rs []JournalRecord) { recs = rs }); err != nil {
		return SnapshotFork{}, err
	}
	if err := legacyRefusal("fork", run, recs); err != nil {
		return SnapshotFork{}, err
	}
	pre, err := e.forkPreflight(run, recs, seq)
	if err != nil {
		return SnapshotFork{}, err
	}
	prefix, def, st, data, sourceDir, adapters, names := pre.prefix, pre.def, pre.st, pre.data, pre.sourceDir, pre.adapters, pre.names
	out, err = realPath(out)
	if err != nil {
		return SnapshotFork{}, err
	}
	ancestor, err := checkForkDestination(out, st.started.Workspace, e.opts.StateRoot)
	if err != nil {
		return SnapshotFork{}, err
	}
	if err = os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return SnapshotFork{}, err
	}
	if err = os.Mkdir(out, 0700); err != nil {
		return SnapshotFork{}, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(out)
		}
	}()
	report := SnapshotFork{Run: run, Seq: seq, Workspace: filepath.Join(out, "workspace"), StateDir: filepath.Join(out, "state")}
	// A version-1 checkpoint is a directory; a version-2 one is a tree of the
	// run's store, written out (only read from the store) into a directory of
	// the same layout inside the fork, and removed once restored.
	checkpoint := filepath.Join(sourceDir, data.Directory)
	if data.Version == 2 {
		// The fork gets its own store holding exactly the checkpoints its
		// journal prefix names, fetched with all they reach; the target is
		// written out from that copy.
		var trees []string
		seenTree := map[string]bool{}
		for _, record := range prefix {
			var saved WorldSnapshotData
			if record.Kind == JournalWorldSnapshot && json.Unmarshal(record.Data, &saved) == nil && saved.Version == 2 && saved.Tree != "" && !seenTree[saved.Tree] {
				seenTree[saved.Tree] = true
				trees = append(trees, saved.Tree)
			}
		}
		forkSnapshots := filepath.Join(report.StateDir, "runs", string(run), "snapshots")
		if err = os.MkdirAll(forkSnapshots, 0o700); err != nil {
			return SnapshotFork{}, err
		}
		store, err := createSnapshotStore(filepath.Join(forkSnapshots, storeDirName))
		if err != nil {
			return SnapshotFork{}, err
		}
		if err = store.fetchCheckpoints(filepath.Join(sourceDir, data.Directory), trees); err != nil {
			return SnapshotFork{}, err
		}
		checkpoint = filepath.Join(out, ".checkpoint")
		if err = store.materialize(data.Tree, checkpoint, data.Adapters); err != nil {
			return SnapshotFork{}, err
		}
	}
	omitted := slices.Clone(data.Omitted)
	for _, name := range names {
		req := RestoreRequest{Workspace: report.Workspace, StateRoot: report.StateDir, Directory: filepath.Join(checkpoint, name),
			Omit: func(what string) { omitted = append(omitted, name+": "+what) }}
		if err = adapters[name].Restore(req, data.Adapters[name]); err != nil {
			return SnapshotFork{}, fmt.Errorf("restore %s: %w", name, err)
		}
	}
	if report.Installed, report.InstalledFrom, err = e.installBinary(report.Workspace); err != nil {
		return SnapshotFork{}, err
	}
	if report.Installed != "" {
		// The run's hash as of the checkpoint (after any migration or
		// adoption in the prefix), against the hash this build would adopt
		// the fork onto, with the asset roots an adoption selects.
		report.RunHash, report.BuildHash = st.hash, definitionHashAssets(def, assetRoots(def, e.opts.Assets))
	}
	from := PathRoots{Workspace: st.started.Workspace, State: e.opts.StateRoot}
	to := PathRoots{Workspace: report.Workspace, State: report.StateDir}
	source := ForkSource{Workspace: from.Workspace, State: from.State, RunDir: sourceDir}
	outputs, err := relocateCommandOutputs(prefix, from, to)
	if err != nil {
		return SnapshotFork{}, err
	}
	var relocated []string
	origins := []PathRoots{from}
	if len(prefix) > 0 && prefix[0].Roots != nil {
		origins = append(origins, *prefix[0].Roots)
	}
	for i := range prefix {
		// A fork starts a new coordinator session; completed historical claims
		// remain pinned evidence, but no coordinator is inherited.
		if prefix[i].Kind == JournalCoordinator {
			prefix[i] = uninheritedCoordinator(prefix[i])
		}
		prefix[i].Data, err = mapJournalPaths(prefix[i].Kind, prefix[i].Data, func(s string) string { return relocateString(s, from, to) })
		if err != nil {
			return SnapshotFork{}, err
		}
		if prefix[i].Kind == JournalRunStarted {
			if prefix[i].Data, relocated, err = relocateUntypedParams(prefix[i].Data, origins, to); err != nil {
				return SnapshotFork{}, err
			}
		}
		prefix[i].Roots = nil
		prefix[i].RelativePaths = false
	}
	opts := e.opts
	opts.Workspace = report.Workspace
	opts.StateRoot = report.StateDir
	opts.RunDir = func(r RunID) string { return filepath.Join(report.StateDir, "runs", string(r)) }
	fork := NewEngine(FileJournal(report.StateDir), e.reg, opts).(*Engine)
	// Copy command evidence before restoring run-data links.
	if err = copyCommandOutputs(outputs); err != nil {
		return SnapshotFork{}, err
	}
	for _, sub := range []string{"records", "out", "prompt"} {
		if err = copySnapshotTree(filepath.Join(checkpoint, "run-data", sub), filepath.Join(opts.RunDir(run), sub), data.Version == 2); err != nil {
			return SnapshotFork{}, err
		}
	}
	if data.Version == 2 {
		if err = removeTree(checkpoint); err != nil {
			return SnapshotFork{}, err
		}
	}
	copied := map[string]bool{}
	for _, record := range prefix {
		if record.Kind != JournalWorldSnapshot {
			continue
		}
		var saved WorldSnapshotData
		if err = json.Unmarshal(record.Data, &saved); err != nil {
			return SnapshotFork{}, err
		}
		if saved.Directory == "" || copied[saved.Directory] || saved.Version == 2 {
			continue // a version-2 checkpoint came with the fork's store
		}
		if filepath.Clean(saved.Directory) != saved.Directory || !strings.HasPrefix(saved.Directory, "snapshots"+string(filepath.Separator)) {
			return SnapshotFork{}, fmt.Errorf("%w: invalid prior snapshot directory", ErrCorruptJournal)
		}
		if info, statErr := os.Stat(filepath.Join(sourceDir, saved.Directory)); statErr != nil || !info.IsDir() {
			return SnapshotFork{}, fmt.Errorf("%w: prior snapshot directory missing: %s", ErrUnavailable, saved.Directory)
		}
		if err = copySnapshotTree(filepath.Join(sourceDir, saved.Directory), filepath.Join(opts.RunDir(run), saved.Directory), false); err != nil {
			return SnapshotFork{}, err
		}
		copied[saved.Directory] = true
	}
	err = syncSnapshotPublication(out, ancestor, syncPath, func() error {
		// A journal write may commit before returning. Retain the world once
		// publication starts, even if the caller observes an interrupted write.
		complete = true
		rd := RewoundData{From: run, Seq: seq, State: "snapshot " + strconv.FormatInt(seq, 10), Source: &source, Omitted: omitted, Relocated: relocated}
		if mark != nil {
			mark(&rd)
		}
		_, err := fork.writeFork(run, prefix, rd)
		return err
	})
	if err != nil {
		return SnapshotFork{}, err
	}
	complete = true
	return report, nil
}

// copyCommandOutputs copies each full-output file, source to fork path; a
// source file already gone is left out, as the capture would not have it.
func copyCommandOutputs(files map[string]string) error {
	for _, src := range slices.Sorted(maps.Keys(files)) {
		dst := files[src]
		in, _, err := openSnapshotFile(src)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		body, err := io.ReadAll(in)
		in.Close()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// copySnapshotTree preserves modes and streams large files. For version-2
// run data it copies links themselves. Missing run-data trees are empty.
func copySnapshotTree(src, dst string, links bool) error {
	root, err := openSnapshotDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	// Directories get their modes after their contents are copied: a
	// read-only one must still be filled.
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	dirs := []dirMode{{dst, info.Mode().Perm()}}
	err = walkSnapshotDir(root, "", func(parent *os.File, rel string, info fs.FileInfo) (bool, error) {
		to := filepath.Join(dst, rel)
		mode := info.Mode()
		if mode.IsDir() {
			dirs = append(dirs, dirMode{to, mode.Perm()})
			return true, os.MkdirAll(to, 0o700)
		}
		if links && mode&fs.ModeSymlink != 0 {
			target, err := os.Readlink(snapshotEntryPath(parent, info.Name()))
			if err != nil {
				return false, err
			}
			return false, os.Symlink(target, to)
		}
		if !mode.IsRegular() {
			return false, fmt.Errorf("%w: snapshot artifact is not a regular file: %s", ErrUnavailable, filepath.Join(src, rel))
		}
		in, _, err := openSnapshotFileAt(parent, info.Name())
		if err != nil {
			return false, err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
		if err != nil {
			return false, err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return false, err
		}
		return false, closeErr
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}

// Publication follows durable files and each newly created directory entry.
func syncSnapshotPublication(root, ancestor string, syncFile func(string) error, publish func() error) error {
	if err := syncCheckpoint(root, syncFile); err != nil {
		return err
	}
	for p := filepath.Dir(root); ; p = filepath.Dir(p) {
		if err := syncFile(p); err != nil {
			return err
		}
		if p == ancestor {
			break
		}
		if filepath.Dir(p) == p {
			return fmt.Errorf("snapshot sync ancestor is not above checkpoint")
		}
	}
	return publish()
}

// installBinary installs the invoking build at the declared install path in
// a restored workspace that lacks it: the install path is ignored content a
// world snapshot does not carry, and the fork's own workflow commands run it.
// Every filesystem step goes through a root confined to the workspace, so a
// restored symlink cannot carry the install out of it. A world that holds
// the path keeps it when it is an executable regular file; anything else
// there is refused, named. The binary is staged privately beside its final
// name and linked into place whole, never replacing what is there.
func (e *Engine) installBinary(workspace string) (installed, from string, err error) {
	rel := e.opts.InstalledBinary
	if rel == "" {
		return "", "", nil
	}
	refuse := func(why string) (string, string, error) {
		return "", "", fmt.Errorf("%w: installed binary %s: %s", ErrRejected, rel, why)
	}
	if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel {
		return refuse("not a clean path relative to the workspace")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	switch info, err := root.Lstat(rel); {
	case err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0:
		return "", "", nil // the restored world's own
	case err == nil && info.Mode().IsRegular():
		return refuse("the restored file is not executable")
	case err == nil:
		return refuse("the restored world holds a " + fileKind(info.Mode()) + " there, not an executable file")
	case !errors.Is(err, fs.ErrNotExist):
		return refuse(err.Error())
	}
	dir := filepath.Dir(rel)
	if err = root.MkdirAll(dir, 0o755); err != nil {
		return refuse(err.Error())
	}
	src, from, err := e.invokingBuild()
	if err != nil {
		return "", "", err
	}
	defer src.Close()
	tmp := filepath.Join(dir, "."+filepath.Base(rel)+".installing-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return refuse(err.Error())
	}
	defer root.Remove(tmp) // after the link, only the staged name goes
	// io.Copy between files lets the kernel share extents (copy_file_range)
	// where the filesystem can: a fan-out of forks does not write the binary
	// out once per fork.
	if _, err = io.Copy(out, src); err == nil {
		if err = out.Chmod(0o755); err == nil {
			err = out.Sync()
		}
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", fmt.Errorf("install %s: %w", rel, err)
	}
	if err = root.Link(tmp, rel); err != nil {
		return refuse(err.Error())
	}
	return filepath.Join(workspace, rel), from, nil
}

// invokingBuild opens the running image: /proc/self/exe where there is one,
// since the executable's path may name a replacement installed after this
// process started. from is the path it was started as, for the report.
func (e *Engine) invokingBuild() (*os.File, string, error) {
	from := e.opts.caller.Executable
	if from == "" {
		var err error
		if from, err = os.Executable(); err != nil {
			return nil, "", fmt.Errorf("locating the running binary: %w", err)
		}
	}
	if f, err := os.Open("/proc/self/exe"); err == nil {
		return f, from, nil
	}
	f, err := os.Open(from)
	if err != nil {
		return nil, "", fmt.Errorf("opening the running binary: %w", err)
	}
	return f, from, nil
}

// fileKind names a non-regular file's type.
func fileKind(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsDir():
		return "directory"
	default:
		return "special file"
	}
}

// collectWorktrees applies the git worktree_add and worktree_remove effects
// one run's journal records applied to trees, the set of worktree paths a
// checkpoint captures.
func (c *cycle) collectWorktrees(recs []JournalRecord, trees map[string]bool) error {
	intents := map[int64]EffectData{}
	for _, rec := range recs {
		var effect EffectData
		switch rec.Kind {
		case JournalEffectIntent, JournalCleanupDeferred:
			var d struct{ Effect EffectData }
			if json.Unmarshal(rec.Data, &d) == nil {
				intents[rec.Seq] = d.Effect
			}
			continue
		case JournalEffectResult:
			var d EffectResultData
			if json.Unmarshal(rec.Data, &d) != nil || !d.OK {
				continue
			}
			effect = intents[d.Intent]
		case JournalCleanupApplied:
			var d CleanupAppliedData
			if json.Unmarshal(rec.Data, &d) != nil {
				continue
			}
			effect = intents[d.Deferred]
		default:
			continue
		}
		if effect.Adapter != "git" || (effect.Op != "worktree_add" && effect.Op != "worktree_remove") {
			continue
		}
		value, err := c.resolvePathValues(effect.Args["path"])
		if err != nil {
			return fmt.Errorf("snapshot worktree path: %w", err)
		}
		p, _ := value.(string)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.workspace(), p)
		}
		p = filepath.Clean(p)
		switch effect.Op {
		case "worktree_add":
			trees[p] = true
		case "worktree_remove":
			delete(trees, p)
		}
	}
	return nil
}

// seedWorktrees is the worktree set the run carries on in from its
// predecessor chain: its recorded Worktrees, or, on a successor from before
// that field, the chain's own effects read run by run (each ended run's
// records are final, read without its lock; endedCycle). seen holds the runs
// already on the path; a chain that repeats one is refused.
func (c *cycle) seedWorktrees(seen map[RunID]bool) (map[string]bool, error) {
	trees := map[string]bool{}
	started := c.st.started
	if started == nil {
		return trees, nil
	}
	if started.Worktrees != nil {
		for _, p := range *started.Worktrees {
			trees[filepath.Clean(p)] = true
		}
		return trees, nil
	}
	pred := started.Predecessor
	if pred == "" {
		return trees, nil
	}
	if seen[pred] {
		return nil, fmt.Errorf("%w: run %s's predecessor chain repeats run %s: refusing a corrupt chain", ErrUnavailable, c.run, pred)
	}
	seen[pred] = true
	pc, err := c.e.endedCycle(pred)
	if err != nil {
		return nil, err
	}
	trees, err = pc.seedWorktrees(seen)
	if err != nil {
		return nil, err
	}
	return trees, pc.collectWorktrees(pc.recs, trees)
}

// endedCycle is a read-only cycle over run's committed records, read
// without its lock. The caller holds its own run's lock, and never waits on
// another's while it does: a journal with no lock-free read leaves the
// predecessor unread, and the capture unavailable with why. It resolves the
// run's own effects' paths against its own params and run directory.
func (e *Engine) endedCycle(run RunID) (*cycle, error) {
	cr, ok := e.j.(committedReader)
	if !ok {
		return nil, fmt.Errorf("%w: reading predecessor %s: predecessor journal has no lock-free read", ErrUnavailable, run)
	}
	recs, err := cr.ReadCommitted(run)
	if errors.Is(err, errNoLockFreeRead) {
		return nil, fmt.Errorf("%w: reading predecessor %s: predecessor journal has no lock-free read", ErrUnavailable, run)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: reading predecessor %s: %v", ErrUnavailable, run, err)
	}
	return endedRunCycle(e, run, recs)
}

// endedRunCycle is the read-only cycle of run over recs, its run_started
// decoded without a fold.
func endedRunCycle(e *Engine, run RunID, recs []JournalRecord) (*cycle, error) {
	var started *RunStartedData
	for _, r := range recs {
		if r.Kind == JournalRunStarted {
			var d RunStartedData
			if json.Unmarshal(r.Data, &d) == nil {
				started = &d
			}
			break
		}
	}
	if started == nil {
		return nil, fmt.Errorf("%w: run %s has no run_started", ErrUnavailable, run)
	}
	return &cycle{e: e, run: run, recs: recs, st: &runState{started: started, insts: map[string]*instance{}}, dry: true, lockFree: true}, nil
}

// inheritedWorktrees is the worktree set a successor of the ended run (its
// records recs) carries on in, sorted: what that run carried on in plus the
// worktrees its own effects added and did not remove. A corrupt chain is
// refused.
func (e *Engine) inheritedWorktrees(run RunID, recs []JournalRecord) ([]string, error) {
	c, err := endedRunCycle(e, run, recs)
	if err != nil {
		return nil, err
	}
	trees, err := c.seedWorktrees(map[RunID]bool{run: true})
	if err != nil {
		return nil, err
	}
	if err := c.collectWorktrees(recs, trees); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(trees))
	for p := range trees {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
