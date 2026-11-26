package wf

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// The snapshot store keeps a run's world checkpoints (snapshot version 2) as
// commits of one bare git repository, <run dir>/snapshots/store.git, each
// kept by the ref refs/checkpoints/<tree>. A file is stored once however
// many checkpoints hold it, so an unchanged world adds no bytes and a
// changed one only what changed. A file larger than storeBlockMin is stored
// as storeBlock-sized blocks, so a file that grows by appending (Dolt's
// chunk journal) adds only its last block; one that is rewritten adds all
// of its blocks, once.
//
// Git does the storing: git fast-import writes a capture's new objects as
// one pack and its ref through git's lock file, with core.fsync; git fetch
// copies a fork exactly the checkpoints its journal names, with every
// object they reach; git cat-file reads objects, packed or loose, and every
// object read is hashed again here. The engine computes every id itself
// first, so an unchanged world needs no write, and a file whose bytes
// changed between its hashing and its writing refuses the capture rather
// than being stored under another world's name.
//
// A checkpoint's tree has the layout of a version-1 checkpoint directory
// (<adapter>/..., run-data/<sub>/...) plus, at its root, storeMetaName (each
// path's mode, which files are blocks, and the directories git drops when
// empty) and storeManifestName (the adapters' restore metadata, as the
// journal records it). A fork materializes it into a directory of that
// layout and restores from it as from a version-1 checkpoint.
//
// The hashes keep what was read, not an atomic copy of a world a writer
// changes meanwhile: an adapter keeps its source quiescent while it hands it
// over (bd holds its native gates through PutDir), and the engine captures
// only while no child or command is live.
const (
	storeDirName      = "store.git"
	storeMetaName     = ".wf-meta.json"
	storeManifestName = ".wf-manifest.json"
	storeBlockMin     = 1 << 20
	storeBlock        = 64 << 10
	storeRefs         = "refs/checkpoints/"
)

// verify checks, by content, the objects of ids (id to kind) a capture
// reuses: each is read through git cat-file --batch and hashed again, so an
// object stored under the wrong name, which --batch-check cannot see, refuses
// the capture instead of being published. done holds the objects this
// capture already verified or wrote: they are skipped, and nothing carries
// over from one publication to the next.
func (s *snapshotStore) verify(ids map[string]string, done map[string]bool) error {
	var todo []string
	for id := range ids {
		if !done[id] {
			todo = append(todo, id)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	// Objects of a pack read back whole and unchanged since are not read
	// again (trustedObjects).
	trusted := s.trustedObjects()
	kept := todo[:0]
	for _, id := range todo {
		if trusted[id] {
			done[id] = true
		} else {
			kept = append(kept, id)
		}
	}
	if todo = kept; len(todo) == 0 {
		return nil
	}
	sort.Strings(todo)
	r, err := s.reader()
	if err != nil {
		return err
	}
	defer r.close()
	for _, id := range todo {
		if _, err := r.get(id, ids[id]); err != nil {
			return fmt.Errorf("%w (the store holds a damaged object; restore it or start a new store)", err)
		}
		done[id] = true
	}
	return nil
}

// errWorldChanged: a file's bytes changed between a capture's hashing and its
// writing; nothing was published.
var errWorldChanged = errors.New("snapshot store: a captured file changed while it was stored")

type snapshotStore struct {
	dir string
	mu  sync.Mutex
	// verified are the objects this value read back and hashed again.
	verified map[string]string
	// views are the checkpoints this value validated, by tree: only what
	// passed is kept.
	views map[string]*checkpointView
	// trustSig and trustIDs memo trustedObjects for one trusted set.
	trustSig string
	trustIDs map[string]bool
}

// createSnapshotStore opens the store at dir, creating it when it does not
// exist: a new store is made whole in a staging directory and renamed into
// place, so no other process ever sees a partial one. An existing directory
// is never removed: a git repository there is the store (its optional
// directories are recreated when missing, as when its refs are packed); any
// other is an error, left for a person.
func createSnapshotStore(dir string) (*snapshotStore, error) {
	s := openSnapshotStore(dir)
	if _, err := os.Lstat(dir); err == nil {
		if !s.isRepository() {
			return nil, fmt.Errorf("%w: snapshot store %s is not a git repository; it is left as it is", ErrUnavailable, dir)
		}
		for _, d := range []string{"refs/checkpoints", "objects/pack"} {
			p := filepath.Join(dir, filepath.FromSlash(d))
			if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
				if err := os.MkdirAll(p, 0o700); err != nil {
					return nil, err
				}
				if err := syncPath(filepath.Dir(p)); err != nil {
					return nil, err
				}
			} else if err != nil {
				return nil, err
			}
		}
		return s, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	parent := filepath.Dir(dir)
	tmp, err := os.MkdirTemp(parent, ".store-init-")
	if err != nil {
		return nil, err
	}
	defer removeTree(tmp) // only the staging directory this call made
	repo := filepath.Join(tmp, storeDirName)
	if out, err := gitCommand("", "init", "--quiet", "--bare", repo).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("snapshot store: git init: %v: %s", err, out)
	}
	for _, d := range []string{"refs/checkpoints", "objects/pack"} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o700); err != nil {
			return nil, err
		}
	}
	if err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return syncPath(p)
	}); err != nil {
		return nil, err
	}
	if err := os.Rename(repo, dir); err != nil {
		return nil, err
	}
	if err := syncPath(parent); err != nil {
		return nil, err
	}
	return s, nil
}

// openSnapshotStore is the store at dir; reading writes nothing.
func openSnapshotStore(dir string) *snapshotStore {
	return &snapshotStore{dir: dir, verified: map[string]string{}, views: map[string]*checkpointView{}}
}

// isRepository reports whether git takes dir for a bare repository.
func (s *snapshotStore) isRepository() bool {
	out, err := s.git("rev-parse", "--is-bare-repository").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// gitCommand is git with the store's settings, isolated from the caller's
// repository environment and configuration.
func gitCommand(gitDir string, args ...string) *exec.Cmd {
	full := []string{"-c", "core.fsync=committed", "-c", "core.fsyncMethod=fsync", "-c", "gc.auto=0", "-c", "fastimport.unpackLimit=0"}
	if gitDir != "" {
		full = append(full, "--git-dir", gitDir)
	}
	cmd := exec.Command("git", append(full, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	return cmd
}

func (s *snapshotStore) git(args ...string) *exec.Cmd { return gitCommand(s.dir, args...) }

func gitObjectID(kind string, body []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(body))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// hasCheckpoint reports whether refs/checkpoints/<tree> names a commit of
// that tree: false for a missing, truncated or damaged ref.
func (s *snapshotStore) hasCheckpoint(tree string) bool {
	out, err := s.git("rev-parse", "--verify", "--quiet", storeRefs+tree+"^{tree}").Output()
	return err == nil && strings.TrimSpace(string(out)) == tree
}

// ---------------------------------------------------------------- capture

// storeEntry is one path of a checkpoint being captured.
type storeEntry struct {
	mode fs.FileMode
	ids  []string // a file's blob, or its blocks; a link's target blob
	link string
}

// storeCapture is one checkpoint being captured.
type storeCapture struct {
	s       *snapshotStore
	entries map[string]*storeEntry // by slash path below the root; "" is the root
	// verified are the objects this capture wrote or read back and hashed:
	// only for this one publication.
	verified map[string]bool
	// omitted lists run-data entries the capture left out, with reasons where needed.
	omitted []string
	// cache is the store's file-hash cache (hashCache).
	cache *hashCache
}

func (s *snapshotStore) capture() *storeCapture {
	return &storeCapture{s: s, entries: map[string]*storeEntry{"": {mode: fs.ModeDir | 0o700}}, verified: map[string]bool{}, cache: hashCacheFor(s.dir)}
}

// add reads src, a directory, into the checkpoint at rel. Every file's bytes
// are hashed as they are read, a storeBlock at a time, never whole in
// memory. A missing src adds nothing. skip, when set, leaves out the entries
// it names (by base name). links is whether symbolic links are taken;
// without it a link, like any file type other than a regular file or a
// directory, is refused, as is a name that is not UTF-8. A refusal comes
// before anything is published.
func (c *storeCapture) add(rel, src string, skip func(name string) bool, links bool) error {
	return c.addTree(rel, src, skip, links, false)
}

// addRunData captures run-owned files and symlinks. Nested repositories are
// captured as plain files, with .git entries omitted. Special files are
// omitted too, with their path and type recorded.
func (c *storeCapture) addRunData(rel, src string) error {
	return c.addTree(rel, src, nil, true, true)
}

// isGitName reports whether git refuses name as a path component: .git in
// any ASCII case.
func isGitName(name string) bool { return strings.EqualFold(name, ".git") }

func (c *storeCapture) addTree(rel, src string, skip func(name string) bool, links, runData bool) error {
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
	rel = path.Clean(filepath.ToSlash(rel))
	if rel == "." {
		rel = ""
	}
	if rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) || rel == storeMetaName || rel == storeManifestName {
		return fmt.Errorf("snapshot store: unsafe checkpoint path %q", rel)
	}
	for p := parentPath(rel); ; p = parentPath(p) {
		if e, ok := c.entries[p]; ok && !e.mode.IsDir() {
			return fmt.Errorf("snapshot store: %s is not a directory", p)
		} else if !ok {
			c.entries[p] = &storeEntry{mode: fs.ModeDir | 0o700}
		}
		if p == "" {
			break
		}
	}
	c.entries[rel] = &storeEntry{mode: fs.ModeDir | info.Mode().Perm()}
	buf := make([]byte, storeBlock)
	var read []readFile
	err = walkSnapshotDir(root, "", func(dir *os.File, sub string, info fs.FileInfo) (bool, error) {
		name := info.Name()
		if skip != nil && skip(name) {
			return false, nil
		}
		key := path.Join(rel, filepath.ToSlash(sub))
		// Git refuses a .git component. Run-data repositories and worktrees
		// are captured as plain files, with that entry listed as omitted.
		if isGitName(name) {
			if !runData {
				return false, fmt.Errorf("%w: snapshot store: %s: a .git entry cannot be stored", ErrUnavailable, key)
			}
			c.omitted = append(c.omitted, key)
			return false, nil
		}
		if !utf8.ValidString(name) {
			return false, fmt.Errorf("%w: snapshot store: file name %q in %s is not UTF-8", ErrUnavailable, name, dir.Name())
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			c.entries[key] = &storeEntry{mode: fs.ModeDir | mode.Perm()}
			return true, nil
		case mode&fs.ModeSymlink != 0 && links:
			target, err := os.Readlink(snapshotEntryPath(dir, name))
			if err != nil {
				return false, err
			}
			c.entries[key] = &storeEntry{mode: fs.ModeSymlink | 0o777, ids: []string{gitObjectID("blob", []byte(target))}, link: target}
			read = append(read, readFile{key: key})
		case mode.IsRegular():
			ids, err := fileIDs(dir, name, buf, c.cache)
			if err != nil {
				return false, err
			}
			c.entries[key] = &storeEntry{mode: mode.Perm(), ids: ids}
			read = append(read, readFile{key: key, src: sub})
		case runData:
			c.omitted = append(c.omitted, unsupportedRunDataPath(key, mode))
		default:
			return false, fmt.Errorf("%w: snapshot store: unsupported file %s (%v)", ErrUnavailable, filepath.Join(src, sub), mode.Type())
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	// The bytes go into the store now, while whatever keeps src quiescent
	// (bd's gates) still holds: commit never reads src again.
	return c.spool(root, read, buf)
}

// readFile is a file or link add read, to spool.
type readFile struct{ key, src string }

// spool writes the blobs of files the store lacks, read again and hashed
// against what add hashed, as one pack no ref names yet.
func (c *storeCapture) spool(root *os.File, files []readFile, buf []byte) error {
	var ids []string
	for _, f := range files {
		ids = append(ids, c.entries[f.key].ids...)
	}
	if len(ids) == 0 {
		return nil
	}
	present, err := c.s.present(ids)
	if err != nil {
		return err
	}
	var todo []readFile
	reused := map[string]string{}
	for _, f := range files {
		missing := false
		for _, id := range c.entries[f.key].ids {
			if present[id] {
				reused[id] = "blob"
			} else {
				missing = true
			}
		}
		if missing {
			todo = append(todo, f)
		}
	}
	if err := c.s.verify(reused, c.verified); err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}
	if err := storeHook("before-spool"); err != nil {
		return err
	}
	return c.s.importStream(func(w *bufio.Writer) error {
		written := map[string]bool{}
		blob := func(id string, body []byte) {
			if present[id] || written[id] {
				return
			}
			written[id] = true
			c.verified[id] = true // its bytes hash to id: they were just checked
			fmt.Fprintf(w, "blob\ndata %d\n", len(body))
			w.Write(body)
			w.WriteByte('\n')
		}
		for _, f := range todo {
			e := c.entries[f.key]
			if e.mode&fs.ModeSymlink != 0 {
				blob(e.ids[0], []byte(e.link))
				continue
			}
			if err := streamFile(root, f.src, e.ids, buf, blob); err != nil {
				return err
			}
		}
		return nil
	})
}

// importStream runs one git fast-import over what body writes, then done:
// either all of it is stored, or (body failing) none is published.
func (s *snapshotStore) importStream(body func(w *bufio.Writer) error) error {
	cmd := s.git("fast-import", "--quiet", "--force", "--done")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	w := bufio.NewWriterSize(stdin, 1<<20)
	streamErr := body(w)
	if streamErr == nil {
		_, streamErr = w.WriteString("done\n")
	}
	if streamErr == nil {
		streamErr = w.Flush()
	}
	if streamErr != nil {
		_ = cmd.Process.Kill() // fast-import stores nothing without done
	}
	stdin.Close()
	err = cmd.Wait()
	// A fast-import that died first is why the stream broke: its own fatal
	// message is the cause, not the writer's broken pipe.
	if msg := strings.TrimSpace(stderr.String()); err != nil && msg != "" {
		return fmt.Errorf("%w: snapshot store: git fast-import: %v: %s", ErrUnavailable, err, msg)
	}
	if streamErr == nil && err != nil {
		return fmt.Errorf("snapshot store: git fast-import: %v", err)
	}
	return streamErr
}

func parentPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// openSnapshotAt walks one component at a time from an open directory.
// No component may be a symlink, even if it points back into the tree.
func openSnapshotAt(root *os.File, rel string, flags int) (*os.File, error) {
	if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel {
		return nil, fmt.Errorf("invalid snapshot source path %q", rel)
	}
	dir := root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		openFlags := syscall.O_RDONLY | syscall.O_DIRECTORY
		if i == len(parts)-1 {
			openFlags = flags
		}
		next, err := openSnapshotComponent(dir, part, openFlags|syscall.O_NOFOLLOW)
		if dir != root {
			dir.Close()
		}
		if err != nil {
			return nil, err
		}
		dir = next
	}
	return dir, nil
}

func openSnapshotDir(name string) (*os.File, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	root, err := os.Open(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel, err := filepath.Rel(root.Name(), absolute)
	if err != nil {
		return nil, err
	}
	return openSnapshotAt(root, rel, syscall.O_RDONLY|syscall.O_DIRECTORY)
}

// snapshotEntryPath addresses one entry through the kernel's link for an
// already verified, open directory. Lstat and Readlink leave the entry itself
// unresolved. The caller keeps the directory open for the entire operation.
func snapshotEntryPath(dir *os.File, name string) string {
	return filepath.Join(snapshotFDPath, strconv.FormatUint(uint64(dir.Fd()), 10), name)
}

// walkSnapshotDir enumerates through directory descriptors, not the paths by
// which they were originally reached. The visitor chooses which dirs to enter.
func walkSnapshotDir(dir *os.File, prefix string, visit func(*os.File, string, fs.FileInfo) (bool, error)) error {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		rel := filepath.Join(prefix, name)
		info, err := os.Lstat(snapshotEntryPath(dir, name))
		if err != nil {
			return err
		}
		descend, err := visit(dir, rel, info)
		if err != nil {
			return err
		}
		if !descend {
			continue
		}
		child, err := openSnapshotAt(dir, name, syscall.O_RDONLY|syscall.O_DIRECTORY)
		if err != nil {
			return err
		}
		actual, err := child.Stat()
		if err == nil {
			if !os.SameFile(actual, info) {
				err = errWorldChanged
			} else {
				err = walkSnapshotDir(child, rel, visit)
			}
		}
		child.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func openSnapshotFile(name string) (*os.File, fs.FileInfo, error) {
	parent, err := openSnapshotDir(filepath.Dir(name))
	if err != nil {
		return nil, nil, err
	}
	defer parent.Close()
	return openSnapshotFileAt(parent, filepath.Base(name))
}

// openSnapshotFileAt accepts only regular files. O_NONBLOCK avoids waiting
// on a FIFO while checking the type of a replaced path.
func openSnapshotFileAt(root *os.File, rel string) (*os.File, fs.FileInfo, error) {
	f, err := openSnapshotAt(root, rel, syscall.O_RDONLY|syscall.O_NONBLOCK)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errWorldChanged
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// fileIDs is the blob ids the file at p is stored as: one blob, or one per
// storeBlock when it is larger than storeBlockMin. It reads the file once, a
// block at a time, into buf.
func fileIDs(dir *os.File, name string, buf []byte, cache *hashCache) ([]string, error) {
	f, fi, err := openSnapshotFileAt(dir, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	path := filepath.Join(dir.Name(), name)
	if ids, ok := cache.get(path, fi); ok {
		return ids, nil
	}
	start := time.Now()
	ids, err := hashFile(f, fi, buf)
	if err == nil {
		cache.put(path, fi, start, ids)
	}
	return ids, err
}

// hashFile is fileIDs of the open file f, read whole.
func hashFile(f *os.File, fi fs.FileInfo, buf []byte) ([]string, error) {
	if fi.Size() <= storeBlockMin {
		body, err := io.ReadAll(io.LimitReader(f, storeBlockMin+1))
		if err != nil {
			return nil, err
		}
		if len(body) > storeBlockMin {
			return nil, errWorldChanged
		}
		return []string{gitObjectID("blob", body)}, nil
	}
	var ids []string
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			ids = append(ids, gitObjectID("blob", buf[:n]))
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(ids) < 2 {
		return nil, errWorldChanged // shrank under the block threshold while read
	}
	return ids, nil
}

// storeMeta is a checkpoint's record of what git trees do not hold: every
// path's full mode (directories included, since git drops an empty one), and
// how many blocks a block-stored file has.
type storeMeta struct {
	Version int                       `json:"version"`
	Entries map[string]storeMetaEntry `json:"entries"`
}

type storeMetaEntry struct {
	Mode   uint32 `json:"mode"` // os.FileMode
	Blocks int    `json:"blocks,omitempty"`
}

// storeManifest binds a checkpoint's tree to the adapters' restore metadata
// the journal records for it.
type storeManifest struct {
	Version  int                        `json:"version"`
	Adapters map[string]json.RawMessage `json:"adapters"`
}

// blockName is block i's name: wide enough that name order is number order
// for any file (10^9 blocks of storeBlock).
func blockName(i int) string { return fmt.Sprintf("%09d", i) }

// gitMode is a file's or link's git tree mode.
func gitMode(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "120000"
	case mode&0o111 != 0:
		return "100755"
	}
	return "100644"
}

// storePlan is the checkpoint as computed here: its root tree and the meta
// and manifest blobs.
type storePlan struct {
	tree             string
	treeIDs          []string // every tree of the checkpoint, the root last
	meta, manifest   []byte
	metaID, manifeID string
}

func (c *storeCapture) plan(adapters map[string]json.RawMessage) (*storePlan, error) {
	meta := storeMeta{Version: 2, Entries: map[string]storeMetaEntry{}}
	kids := map[string][]string{}
	for p, e := range c.entries {
		m := storeMetaEntry{Mode: uint32(e.mode)}
		if e.mode.IsRegular() && len(e.ids) > 1 {
			m.Blocks = len(e.ids)
		}
		meta.Entries[p] = m
		if p != "" {
			kids[parentPath(p)] = append(kids[parentPath(p)], p)
		}
	}
	pl := &storePlan{}
	var err error
	if pl.meta, err = json.Marshal(meta); err != nil {
		return nil, err
	}
	if pl.manifest, err = json.Marshal(storeManifest{Version: 2, Adapters: adapters}); err != nil {
		return nil, err
	}
	pl.metaID, pl.manifeID = gitObjectID("blob", pl.meta), gitObjectID("blob", pl.manifest)
	var build func(dir string) string
	build = func(dir string) string {
		var entries []treeEntry
		for _, p := range kids[dir] {
			e, name := c.entries[p], path.Base(p)
			switch {
			case e.mode.IsDir():
				if id := build(p); id != "" {
					entries = append(entries, treeEntry{mode: "40000", name: name, id: id})
				}
			case len(e.ids) > 1:
				var blocks []treeEntry
				for i, id := range e.ids {
					blocks = append(blocks, treeEntry{mode: "100644", name: blockName(i), id: id})
				}
				id := gitObjectID("tree", formatTree(blocks))
				pl.treeIDs = append(pl.treeIDs, id)
				entries = append(entries, treeEntry{mode: "40000", name: name, id: id})
			default:
				entries = append(entries, treeEntry{mode: gitMode(e.mode), name: name, id: e.ids[0]})
			}
		}
		if dir == "" {
			entries = append(entries, treeEntry{mode: "100644", name: storeMetaName, id: pl.metaID}, treeEntry{mode: "100644", name: storeManifestName, id: pl.manifeID})
		}
		if len(entries) == 0 {
			return "" // git keeps no empty tree; the meta names the directory
		}
		id := gitObjectID("tree", formatTree(entries))
		pl.treeIDs = append(pl.treeIDs, id)
		return id
	}
	pl.tree = build("")
	return pl, nil
}

// storeHook, when set by a test, can fail a capture at a publication point.
var storeHook = func(point string) error { return nil }

// commit publishes the checkpoint, durably, and returns its root tree. It
// never reads a source again: add stored every file's bytes. The whole
// closure (every blob and tree, the meta and manifest) is checked present
// first; a checkpoint whose ref exists with all of it is reused (an
// unchanged world writes nothing), and otherwise the missing trees, the
// meta, the manifest and the ref are written and the closure checked again.
// A blob that add stored and is now missing refuses the commit. Either way
// the store's directory chain up to the run directory is synced again before
// commit returns, so a capture after a crash never relies on a sync the
// crashed one skipped.
func (c *storeCapture) commit(adapters map[string]json.RawMessage) (string, error) {
	pl, err := c.plan(adapters)
	if err != nil {
		return "", err
	}
	var blobs []string
	for _, e := range c.entries {
		blobs = append(blobs, e.ids...)
	}
	closure := append(append(append([]string(nil), blobs...), pl.metaID, pl.manifeID), pl.treeIDs...)
	complete := func() (bool, map[string]bool, error) {
		present, err := c.s.present(closure)
		if err != nil {
			return false, nil, err
		}
		for _, id := range blobs {
			if !present[id] {
				return false, nil, fmt.Errorf("%w: snapshot store lost blob %s of this capture", ErrUnavailable, id)
			}
		}
		reused := map[string]string{}
		for _, id := range closure {
			if !present[id] {
				return false, present, nil
			}
			reused[id] = "blob"
		}
		for _, id := range pl.treeIDs {
			reused[id] = "tree"
		}
		// Present is not enough: each object reused must hold its bytes.
		if err := c.s.verify(reused, c.verified); err != nil {
			return false, nil, err
		}
		return c.s.hasCheckpoint(pl.tree), present, nil
	}
	done, present, err := complete()
	if err != nil {
		return "", err
	}
	if !done {
		if err := c.write(pl, present); err != nil {
			return "", err
		}
		if done, _, err = complete(); err != nil {
			return "", err
		} else if !done {
			return "", fmt.Errorf("%w: the snapshot store did not record checkpoint %s", ErrUnavailable, pl.tree)
		}
	}
	if err := storeHook("before-sync"); err != nil {
		return "", err
	}
	if err := c.s.syncChain(); err != nil {
		return "", err
	}
	c.s.consolidate()
	c.s.trustPacks()
	return pl.tree, nil
}

// storePackLimit is how many packs the store holds before a capture rolls
// them up: each capture's fast-imports write packs of their own.
const storePackLimit = 32

// consolidate rolls the store's packs up once there are more than
// storePackLimit: a geometric repack merges the small recent packs into
// larger ones (and, on a store that never rolled up, all into one), so the
// count stays logarithmic in the objects. It runs after the checkpoint is
// published and synced, and git writes the new pack before it deletes the
// old ones, so a failure or a crash here loses nothing: the next capture
// tries again.
func (s *snapshotStore) consolidate() {
	packs, err := filepath.Glob(filepath.Join(s.dir, "objects", "pack", "*.pack"))
	if err != nil || len(packs) <= storePackLimit {
		return
	}
	if s.git("repack", "-d", "-q", "--geometric=2").Run() == nil {
		_ = s.syncChain()
	}
}

// syncChain syncs the directories a publication's entries live in: the pack
// and ref directories, the store, and its ancestors up to the run directory.
func (s *snapshotStore) syncChain() error {
	for _, d := range []string{
		filepath.Join(s.dir, "objects", "pack"), filepath.Join(s.dir, "objects"),
		filepath.Join(s.dir, "refs", "checkpoints"), filepath.Join(s.dir, "refs"), s.dir,
		filepath.Dir(s.dir), filepath.Dir(filepath.Dir(s.dir)),
	} {
		if err := syncPath(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// write stores the checkpoint's commit: its meta and manifest when missing,
// its trees (fast-import makes them from the paths) and its ref.
func (c *storeCapture) write(pl *storePlan, present map[string]bool) error {
	// A damaged or empty loose ref (a crash inside a ref write) is replaced,
	// not reused.
	if p := filepath.Join(c.s.dir, filepath.FromSlash(storeRefs+pl.tree)); fileExists(p) {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	if err := storeHook("before-import"); err != nil {
		return err
	}
	if err := c.s.importStream(func(w *bufio.Writer) error { return c.stream(w, pl, present) }); err != nil {
		return err
	}
	return storeHook("after-import")
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// stream writes the checkpoint's commit to fast-import: the meta and
// manifest, then one root commit holding every path by its blob id.
func (c *storeCapture) stream(w *bufio.Writer, pl *storePlan, present map[string]bool) error {
	inline := func(id string, body []byte, name string) {
		if present[id] {
			fmt.Fprintf(w, "M 100644 %s %s\n", id, name)
			return
		}
		fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n", name, len(body))
		w.Write(body)
		w.WriteByte('\n')
	}
	paths := make([]string, 0, len(c.entries))
	for p := range c.entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	r := storeRefs + pl.tree
	fmt.Fprintf(w, "reset %s\ncommit %s\ncommitter wf <wf@localhost> 0 +0000\ndata 0\ndeleteall\n", r, r)
	for _, p := range paths {
		e := c.entries[p]
		switch {
		case e.mode.IsDir():
		case len(e.ids) > 1:
			for i, id := range e.ids {
				fmt.Fprintf(w, "M 100644 %s %s\n", id, fastImportPath(p+"/"+blockName(i)))
			}
		default:
			fmt.Fprintf(w, "M %s %s %s\n", gitMode(e.mode), e.ids[0], fastImportPath(p))
		}
	}
	inline(pl.metaID, pl.meta, storeMetaName)
	inline(pl.manifeID, pl.manifest, storeManifestName)
	_, err := w.WriteString("\n")
	return err
}

// streamFile hands blob the parts of the file at src, read again: a part
// whose bytes no longer hash to the id add computed refuses the capture.
func streamFile(root *os.File, src string, ids []string, buf []byte, blob func(string, []byte)) error {
	f, _, err := openSnapshotFileAt(root, src)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(ids) == 1 {
		body, err := io.ReadAll(io.LimitReader(f, storeBlockMin+1))
		if err != nil {
			return err
		}
		if gitObjectID("blob", body) != ids[0] {
			return errWorldChanged
		}
		blob(ids[0], body)
		return nil
	}
	for _, id := range ids {
		n, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return errWorldChanged
		}
		if gitObjectID("blob", buf[:n]) != id {
			return errWorldChanged
		}
		blob(id, buf[:n])
	}
	return nil
}

// fastImportPath is p as fast-import reads a path: C-quoted, so any UTF-8
// name (a space, a newline, a leading quote) is taken literally.
func fastImportPath(p string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch ch := p[i]; {
		case ch == '"' || ch == '\\':
			b.WriteByte('\\')
			b.WriteByte(ch)
		case ch < 0x20 || ch == 0x7f:
			fmt.Fprintf(&b, "\\%03o", ch)
		default:
			b.WriteByte(ch)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// present is which of ids the store holds.
func (s *snapshotStore) present(ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	cmd := s.git("cat-file", "--batch-check=%(objectname) %(objecttype)")
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("snapshot store: git cat-file: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if id, kind, ok := strings.Cut(line, " "); ok && kind != "missing" {
			out[id] = true
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- trees

type treeEntry struct{ mode, name, id string }

// formatTree is a git tree object's body: entries sorted as git sorts them,
// a directory by its name with a trailing slash.
func formatTree(entries []treeEntry) []byte {
	key := func(e treeEntry) string {
		if e.mode == "40000" {
			return e.name + "/"
		}
		return e.name
	}
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
	var b bytes.Buffer
	for _, e := range entries {
		raw, _ := hex.DecodeString(e.id)
		fmt.Fprintf(&b, "%s %s\x00", e.mode, e.name)
		b.Write(raw)
	}
	return b.Bytes()
}

func parseTree(body []byte) ([]treeEntry, error) {
	var out []treeEntry
	for len(body) > 0 {
		head, rest, ok := bytes.Cut(body, []byte{0})
		mode, name, ok2 := strings.Cut(string(head), " ")
		if !ok || !ok2 || len(rest) < 20 || name == "" || strings.Contains(name, "/") || name == "." || name == ".." {
			return nil, fmt.Errorf("%w: snapshot store tree is damaged", ErrUnavailable)
		}
		out = append(out, treeEntry{mode: mode, name: name, id: hex.EncodeToString(rest[:20])})
		body = rest[20:]
	}
	return out, nil
}

// ---------------------------------------------------------------- reading

// objectReader reads objects through one git cat-file --batch and hashes
// each again: an object whose bytes do not hash to its id is refused.
type objectReader struct {
	s   *snapshotStore
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func (s *snapshotStore) reader() (*objectReader, error) {
	cmd := s.git("cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &objectReader{s: s, cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20)}, nil
}

func (r *objectReader) close() {
	r.in.Close()
	_ = r.cmd.Wait()
}

// get is object id, which must be of kind ("" for any).
func (r *objectReader) get(id, kind string) ([]byte, error) {
	if _, err := fmt.Fprintln(r.in, id); err != nil {
		return nil, err
	}
	head, err := r.out.ReadString('\n')
	if err != nil {
		return nil, err
	}
	f := strings.Fields(head)
	if len(f) == 2 && f[1] == "missing" {
		return nil, fmt.Errorf("%w: snapshot store object %s missing", ErrUnavailable, id)
	}
	if len(f) != 3 || f[0] != id {
		return nil, fmt.Errorf("%w: snapshot store: unexpected cat-file reply %q", ErrUnavailable, head)
	}
	n, err := strconv.Atoi(f[2])
	if err != nil {
		return nil, err
	}
	body := make([]byte, n+1)
	if _, err := io.ReadFull(r.out, body); err != nil {
		return nil, err
	}
	body = body[:n]
	if gitObjectID(f[1], body) != id {
		return nil, fmt.Errorf("%w: snapshot store object %s is damaged", ErrUnavailable, id)
	}
	if kind != "" && f[1] != kind {
		return nil, fmt.Errorf("%w: snapshot store object %s is a %s, not a %s", ErrUnavailable, id, f[1], kind)
	}
	r.s.mu.Lock()
	r.s.verified[id] = f[1]
	r.s.mu.Unlock()
	return body, nil
}

// check is get for an object whose bytes are not needed: one this value
// already verified is not read again.
func (r *objectReader) check(id, kind string) error {
	r.s.mu.Lock()
	got, ok := r.s.verified[id]
	r.s.mu.Unlock()
	if ok && got == kind {
		return nil
	}
	_, err := r.get(id, kind)
	return err
}

// checkpointView is a validated checkpoint: its meta, and every file's or
// link's blob ids in order.
type checkpointView struct {
	meta     storeMeta
	files    map[string][]string
	adapters map[string]json.RawMessage // the manifest's
}

// validate checks checkpoint tree against the adapters one journal record
// gives it, before anything is restored from it: the tree (view) and the
// record's metadata, compared exactly with the manifest the tree holds.
func (s *snapshotStore) validate(tree string, adapters map[string]json.RawMessage) (*checkpointView, error) {
	v, err := s.view(tree)
	if err != nil {
		return nil, err
	}
	if !sameAdapters(v.adapters, adapters) {
		return nil, fmt.Errorf("%w: checkpoint %s: its adapters differ from the journal's", ErrUnavailable, tree)
	}
	return v, nil
}

// view reads and checks checkpoint tree: the ref names it; every object it
// reaches is present, hashes to its id and has the kind its entry says; the
// meta names exactly the tree's paths (and any empty directories git
// dropped) with matching kinds and modes, and a directory for every adapter
// the manifest names; a block file's tree holds exactly its blocks 0..n-1.
// A tree that passed is not read again by this value; a failure is not
// kept.
func (s *snapshotStore) view(tree string) (*checkpointView, error) {
	s.mu.Lock()
	cached := s.views[tree]
	s.mu.Unlock()
	if cached != nil && s.hasCheckpoint(tree) {
		return cached, nil
	}

	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: checkpoint %s: %s", ErrUnavailable, tree, fmt.Sprintf(format, args...))
	}
	if !s.hasCheckpoint(tree) {
		return nil, bad("no ref in the snapshot store")
	}
	r, err := s.reader()
	if err != nil {
		return nil, err
	}
	defer r.close()
	body, err := r.get(tree, "tree")
	if err != nil {
		return nil, err
	}
	top, err := parseTree(body)
	if err != nil {
		return nil, err
	}
	v := &checkpointView{files: map[string][]string{}}
	var manifest storeManifest
	gotMeta, gotManifest := false, false
	for _, e := range top {
		if e.name != storeMetaName && e.name != storeManifestName {
			continue
		}
		if e.mode != "100644" {
			return nil, bad("%s has mode %s", e.name, e.mode)
		}
		raw, err := r.get(e.id, "blob")
		if err != nil {
			return nil, err
		}
		if e.name == storeMetaName {
			gotMeta = json.Unmarshal(raw, &v.meta) == nil && v.meta.Version == 2
			if v.meta.Version > 2 {
				return nil, fmt.Errorf("%w: checkpoint %s: %w", ErrUnavailable, tree, NewerFormat("its meta", v.meta.Version, 2))
			}
		} else {
			gotManifest = json.Unmarshal(raw, &manifest) == nil && manifest.Version == 2
			if manifest.Version > 2 {
				return nil, fmt.Errorf("%w: checkpoint %s: %w", ErrUnavailable, tree, NewerFormat("its manifest", manifest.Version, 2))
			}
		}
	}
	if !gotMeta || !gotManifest {
		return nil, bad("its meta or manifest is missing or unreadable")
	}
	v.adapters = manifest.Adapters
	root, ok := v.meta.Entries[""]
	if !ok || !fs.FileMode(root.Mode).IsDir() {
		return nil, bad("its meta has no root directory")
	}
	seen := map[string]bool{"": true}
	var walk func(entries []treeEntry, at string) error
	walk = func(entries []treeEntry, at string) error {
		for _, e := range entries {
			if at == "" && (e.name == storeMetaName || e.name == storeManifestName) {
				continue
			}
			p := path.Join(at, e.name)
			m, ok := v.meta.Entries[p]
			if !ok {
				return bad("%s is not in its meta", p)
			}
			seen[p] = true
			mode := fs.FileMode(m.Mode)
			switch {
			case mode.IsRegular() && m.Blocks > 0:
				if e.mode != "40000" || m.Blocks < 2 {
					return bad("%s is a block file but not a tree of blocks", p)
				}
				body, err := r.get(e.id, "tree")
				if err != nil {
					return err
				}
				blocks, err := parseTree(body)
				if err != nil {
					return err
				}
				if len(blocks) != m.Blocks {
					return bad("%s has %d blocks, its meta %d", p, len(blocks), m.Blocks)
				}
				for i, b := range blocks {
					if b.mode != "100644" || b.name != blockName(i) {
						return bad("%s block %d is %s %s", p, i, b.mode, b.name)
					}
					if err := r.check(b.id, "blob"); err != nil {
						return err
					}
					v.files[p] = append(v.files[p], b.id)
				}
			case mode.IsDir():
				if e.mode != "40000" {
					return bad("%s is a directory but not a tree", p)
				}
				body, err := r.get(e.id, "tree")
				if err != nil {
					return err
				}
				kids, err := parseTree(body)
				if err != nil {
					return err
				}
				if err := walk(kids, p); err != nil {
					return err
				}
			case mode.IsRegular() || mode&fs.ModeSymlink != 0:
				if e.mode != gitMode(mode) {
					return bad("%s has git mode %s, its meta %v", p, e.mode, mode)
				}
				if err := r.check(e.id, "blob"); err != nil {
					return err
				}
				v.files[p] = []string{e.id}
			default:
				return bad("%s has unsupported mode %v", p, mode)
			}
		}
		return nil
	}
	if err := walk(top, ""); err != nil {
		return nil, err
	}
	for p, m := range v.meta.Entries {
		if p != "" && (!utf8.ValidString(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") || path.IsAbs(p) || p == "..") {
			return nil, bad("meta path %q", p)
		}
		if p != "" {
			if pm, ok := v.meta.Entries[parentPath(p)]; !ok || !fs.FileMode(pm.Mode).IsDir() {
				return nil, bad("%s has no directory in its meta", p)
			}
		}
		if !seen[p] && !fs.FileMode(m.Mode).IsDir() {
			return nil, bad("%s is in its meta but not its tree", p)
		}
	}
	for name := range v.adapters {
		if m, ok := v.meta.Entries[name]; !ok || !fs.FileMode(m.Mode).IsDir() || strings.Contains(name, "/") {
			return nil, bad("adapter %q has no directory", name)
		}
	}
	s.mu.Lock()
	s.views[tree] = v
	s.mu.Unlock()
	return v, nil
}

// sameAdapters compares two adapters' metadata as JSON values, numbers
// exactly as written (no float rounding).
func sameAdapters(a, b map[string]json.RawMessage) bool {
	norm := func(m map[string]json.RawMessage) (map[string]any, bool) {
		out := map[string]any{}
		for k, v := range m {
			dec := json.NewDecoder(bytes.NewReader(v))
			dec.UseNumber()
			var x any
			if err := dec.Decode(&x); err != nil {
				return nil, false
			}
			out[k] = x
		}
		return out, true
	}
	na, oka := norm(a)
	nb, okb := norm(b)
	return oka && okb && reflect.DeepEqual(na, nb)
}

// materialize writes checkpoint tree, validated against adapters, into dst
// (which must not exist) as a version-1 checkpoint directory: the same
// paths, bytes, modes, links and empty directories. It only reads the store.
func (s *snapshotStore) materialize(tree, dst string, adapters map[string]json.RawMessage) error {
	v, err := s.validate(tree, adapters)
	if err != nil {
		return err
	}
	r, err := s.reader()
	if err != nil {
		return err
	}
	defer r.close()
	paths := make([]string, 0, len(v.meta.Entries))
	for p := range v.meta.Entries {
		paths = append(paths, p)
	}
	sort.Strings(paths) // a directory before what it holds
	for _, p := range paths {
		mode := fs.FileMode(v.meta.Entries[p].Mode)
		target := filepath.Join(dst, filepath.FromSlash(p))
		switch {
		case mode.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			link, err := r.get(v.files[p][0], "blob")
			if err != nil {
				return err
			}
			if err := os.Symlink(string(link), target); err != nil {
				return err
			}
		default:
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			for _, id := range v.files[p] {
				data, err := r.get(id, "blob")
				if err == nil {
					_, err = f.Write(data)
				}
				if err != nil {
					f.Close()
					return err
				}
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := os.Chmod(target, mode.Perm()); err != nil {
				return err
			}
		}
	}
	for i := len(paths) - 1; i >= 0; i-- {
		if mode := fs.FileMode(v.meta.Entries[paths[i]].Mode); mode.IsDir() {
			if err := os.Chmod(filepath.Join(dst, filepath.FromSlash(paths[i])), mode.Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

// fetchCheckpoints copies the checkpoints trees of the store at src into s,
// with every object they reach and nothing else, through git fetch; the
// source is only read.
func (s *snapshotStore) fetchCheckpoints(src string, trees []string) error {
	if len(trees) == 0 {
		return nil
	}
	args := []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc", src}
	for _, t := range trees {
		args = append(args, "+"+storeRefs+t+":"+storeRefs+t)
	}
	if out, err := s.git(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: snapshot store fetch: %v: %s", ErrUnavailable, err, out)
	}
	return s.syncChain()
}

// removeTree removes a tree that may hold read-only directories (a
// materialized checkpoint keeps the source's modes): each is made writable
// first.
func removeTree(root string) error {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// omittedPaths is the sorted list of omitted run-data entries.
func (c *storeCapture) omittedPaths() []string {
	out := append([]string(nil), c.omitted...)
	sort.Strings(out)
	return out
}

// runDataDigest is runDataDigest of the run data this capture holds, from
// the ids it hashed: what it stores.
func (c *storeCapture) runDataDigest() string {
	var lines []string
	for _, sub := range []string{"records", "out", "prompt"} {
		top := "run-data/" + sub
		if _, ok := c.entries[top]; !ok {
			lines = append(lines, "absent "+sub)
			continue
		}
		for p, e := range c.entries {
			if p == top || strings.HasPrefix(p, top+"/") {
				lines = append(lines, digestLine(strings.TrimPrefix(p, "run-data/"), e.mode, e.ids))
			}
		}
		for _, p := range c.omitted {
			if strings.HasPrefix(p, top+"/") {
				lines = append(lines, omittedLine(strings.TrimPrefix(p, "run-data/")))
			}
		}
	}
	return digestLines(lines)
}
