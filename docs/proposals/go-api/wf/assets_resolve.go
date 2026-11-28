package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Assets is the asset root an extra verb (MainConfig.Extra) runs shipped
// files from, resolved as the driving verbs resolve a run's: --assets, else
// the run's pinned roots, else the definition's declared root.
//
// Owns: choosing the root and serving a file's verified bytes under it (Read,
// Materialize), never bytes a pinned run did not pin. Does not: run anything,
// or take the run's lock.
type Assets struct {
	Root   string            // the root of the run's (or invocation's) definition
	Roots  map[string]string // every definition's root, keyed by definition path
	Run    RunID             // the run whose journal was read; "" when none was found
	Pinned bool              // Root is the run's pin: Read serves only bytes it pinned

	def  *Def
	hash string // the run's recorded definition hash, when Pinned
}

// ResolveAssets takes --assets, --state-dir, --run-id and --workspace out of
// args (the rest is the verb's own) and resolves the asset root:
//   - --assets, when given: the invocation's explicit choice, before any run
//     lookup, not held to a pin;
//   - else the run (--run-id, else <state-dir>/run-id, else the only run under
//     <state-dir>/runs): its pinned roots (run_started, then each migration),
//     read without its lock, with Pinned set;
//   - else, with no run at all, the definition's declared root.
//
// Several runs and no pointer, a pointer or --run-id naming a run with no
// journal, an unreadable journal, or no root at all is an error.
func ResolveAssets(cfg MainConfig, args []string) (Assets, []string, error) {
	c := &cli{cfg: cfg}
	rest, stateDir, runID, err := c.splitCommon(args)
	if err != nil {
		return Assets{}, nil, err
	}
	var run RunID
	if runID != "" {
		if run, err = ParseRunID(runID); err != nil {
			return Assets{}, nil, err
		}
	}
	a, err := c.resolveAssets(stateDir, run)
	if err != nil {
		return Assets{}, nil, err
	}
	if a.Root == "" {
		return Assets{}, nil, errors.New("no asset root: the definition declares none; pass --assets <dir>")
	}
	return a, rest, nil
}

func (c *cli) resolveAssets(stateDir string, run RunID) (Assets, error) {
	if c.assets != "" {
		// The invocation's explicit choice wins before any run lookup.
		d, err := c.cfg.assetsDef("")
		if err != nil {
			return Assets{}, err
		}
		roots := assetRoots(d, c.assets)
		return Assets{Root: effectiveAssets(nil, c.assets), Roots: roots, def: d}, nil
	}
	if run == "" {
		var err error
		if run, err = selectRun(stateDir); err != nil {
			return Assets{}, err
		}
	}
	if run == "" {
		d, err := c.cfg.assetsDef("")
		if err != nil {
			return Assets{}, err
		}
		roots := assetRoots(d, "")
		return Assets{Root: roots[d.name], Roots: roots, def: d}, nil
	}
	recs, err := rootedFileJournal(stateDir, c.workspace).(committedReader).ReadCommitted(run)
	if err == nil && len(recs) == 0 {
		err = errors.New("no journal")
	}
	if err != nil {
		return Assets{}, fmt.Errorf("run %s: %w; pass --run-id or --assets", run, err)
	}
	var started *RunStartedData
	hash := ""
	for _, r := range recs {
		switch r.Kind {
		case JournalRunStarted:
			var s RunStartedData
			if err := json.Unmarshal(r.Data, &s); err != nil {
				return Assets{}, fmt.Errorf("run %s: %w", run, err)
			}
			started, hash = &s, r.DefHash
		case JournalMigrated:
			var m MigratedData
			if err := json.Unmarshal(r.Data, &m); err != nil || started == nil {
				return Assets{}, fmt.Errorf("run %s: unreadable migration: %v", run, err)
			}
			if m.AssetRoots != nil {
				started.AssetRoots = m.AssetRoots
			}
			if m.Assets != "" {
				started.Assets = m.Assets
			}
			hash = m.ToHash
		}
	}
	if started == nil {
		return Assets{}, fmt.Errorf("run %s: its journal has no run_started", run)
	}
	d, err := c.cfg.assetsDef(started.Def)
	if err != nil {
		return Assets{}, err
	}
	roots := (&cycle{e: &Engine{}, st: &runState{started: started}}).selectedAssetRoots(d)
	return Assets{Root: roots[d.name], Roots: roots, Run: run, Pinned: true, def: d, hash: hash}, nil
}

// assetsDef is the registered definition named name, else, for "", the first
// registration (the one a named binary's `run` starts by default).
func (cfg MainConfig) assetsDef(name string) (*Def, error) {
	for _, r := range cfg.Defs {
		if name == "" || r.Def.name == name {
			return r.Def, nil
		}
	}
	if name == "" {
		return nil, errors.New("no definition is registered")
	}
	return nil, fmt.Errorf("the run's definition %s is not registered in this binary", name)
}

// selectRun is the run a driving verb resolves without --run-id: the one
// <state-dir>/run-id names (it must have a journal), else the only run under
// <state-dir>/runs, else "" when there is none. Several runs and no pointer
// is an error, as it is for the driving verbs; so is any discovery error but
// a missing pointer or runs directory.
func selectRun(stateDir string) (RunID, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, runIDFile))
	switch {
	case err == nil:
		if id := strings.TrimSpace(string(raw)); id != "" {
			if _, err := os.Stat(filepath.Join(stateDir, "runs", id)); err != nil {
				return "", fmt.Errorf("%s names run %s, which has no journal in %s; pass --run-id or --assets", runIDFile, id, stateDir)
			}
			return ParseRunID(id)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("reading %s: %w; pass --run-id or --assets", runIDFile, err)
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, "runs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("listing the runs in %s: %w; pass --run-id or --assets", stateDir, err)
	}
	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	switch len(runs) {
	case 0:
		return "", nil
	case 1:
		return ParseRunID(runs[0])
	}
	return "", fmt.Errorf("several runs in %s (%s); pass --run-id or --assets", stateDir, strings.Join(runs, ", "))
}

// Read is the content of the asset rel names under Root, read through Root
// confined: a link whose target leaves Root is never followed. For a pinned
// run the asset must be one the definition references, and the definition
// with every asset read now (through confined roots, rel as these very bytes)
// must hash as the run recorded: the bytes returned are the bytes checked, so
// a verb never runs bytes the run did not pin.
func (a Assets) Read(rel string) ([]byte, error) {
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("asset %q: the path must stay within the asset root", rel)
	}
	roots := map[string]*os.Root{}
	defer func() {
		for _, r := range roots {
			r.Close()
		}
	}()
	openRoot := func(dir string) (*os.Root, error) {
		if r, ok := roots[dir]; ok {
			return r, nil
		}
		r, err := os.OpenRoot(dir)
		if err != nil {
			return nil, err
		}
		roots[dir] = r
		return r, nil
	}
	root, err := openRoot(a.Root)
	if err != nil {
		return nil, fmt.Errorf("asset root %s: %w", a.Root, err)
	}
	data, err := readConfined(root, rel)
	if err != nil {
		return nil, fmt.Errorf("asset %q: %w", rel, err)
	}
	if !a.Pinned {
		return data, nil
	}
	open := func(dir string) assetFS {
		r, err := openRoot(dir)
		if err != nil {
			return failedAssets{err}
		}
		fsys := confinedAssets{r}
		if dir == a.Root {
			return servedAsset{confinedAssets: fsys, rel: rel, data: data}
		}
		return fsys
	}
	if !a.references(rel, open) {
		return nil, fmt.Errorf("asset %q is not pinned by run %s: its definition never references it", rel, a.Run)
	}
	if got := definitionHashAssetsIn(a.def, a.Roots, open); got != a.hash {
		return nil, fmt.Errorf("asset %q: run %s pinned other bytes (or another definition) than this binary sees under %s; adopt the run first (resume --adopt)", rel, a.Run, a.Root)
	}
	return data, nil
}

// Materialize writes Read's verified bytes to a new private file (mode 0500,
// in the system temporary directory) and returns its path, for a verb that
// must hand a path to a program. The asset's own path is never returned: the
// file there may change after the check. The caller removes the file.
func (a Assets) Materialize(rel string) (string, error) {
	data, err := a.Read(rel)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "wf-asset-*-"+filepath.Base(rel))
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o500); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// readConfined reads the regular file name under root; root refuses a path
// or link that leaves it.
func readConfined(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(f)
}

// confinedAssets reads assets through an os.Root.
type confinedAssets struct{ r *os.Root }

func (c confinedAssets) Stat(name string) (os.FileInfo, error) { return c.r.Stat(name) }
func (c confinedAssets) ReadFile(name string) ([]byte, error)  { return readConfined(c.r, name) }

// servedAsset is confinedAssets answering rel with the bytes Read serves.
type servedAsset struct {
	confinedAssets
	rel  string
	data []byte
}

func (s servedAsset) Stat(name string) (os.FileInfo, error) {
	if name == s.rel {
		return capturedFile{name: filepath.Base(name), size: int64(len(s.data))}, nil
	}
	return s.confinedAssets.Stat(name)
}

func (s servedAsset) ReadFile(name string) ([]byte, error) {
	if name == s.rel {
		return s.data, nil
	}
	return s.confinedAssets.ReadFile(name)
}

// failedAssets is a root that could not be opened: every read fails.
type failedAssets struct{ err error }

func (f failedAssets) Stat(string) (os.FileInfo, error) { return nil, f.err }
func (f failedAssets) ReadFile(string) ([]byte, error)  { return nil, f.err }

// references reports whether a definition whose root is a.Root references
// rel as an asset.
func (a Assets) references(rel string, open func(string) assetFS) bool {
	found := false
	var walk func(DefView, string)
	walk = func(v DefView, prefix string) {
		key := prefix + v.Name
		if a.Roots[key] == a.Root {
			contents, _ := assetContentsIn(a.Root, v.States, open(a.Root))
			if _, ok := contents[rel]; ok {
				found = true
			}
		}
		for _, child := range v.Children {
			walk(child, key+"/")
		}
	}
	walk(inspectStructure(a.def), "")
	return found
}

// capturedFile is the metadata of the bytes Read captured: a regular file.
type capturedFile struct {
	name string
	size int64
}

func (f capturedFile) Name() string       { return f.name }
func (f capturedFile) Size() int64        { return f.size }
func (f capturedFile) Mode() os.FileMode  { return 0o444 }
func (f capturedFile) ModTime() time.Time { return time.Time{} }
func (f capturedFile) IsDir() bool        { return false }
func (f capturedFile) Sys() any           { return nil }
