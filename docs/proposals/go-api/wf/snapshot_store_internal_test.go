package wf

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// treeImage lists a directory tree: each entry's path, mode, and bytes or
// link target.
func treeImage(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v", rel, info.Mode())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			line += " -> " + target
		case info.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			line += fmt.Sprintf(" %x", sha256.Sum256(b))
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// storeState is every file of a store directory with its size and mtime.
func storeState(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			if info, err := os.Lstat(p); err == nil {
				out = append(out, fmt.Sprintf("%s %v %d %v", p, info.Mode(), info.Size(), info.ModTime().UnixNano()))
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// diskBytes is the bytes the files under dir occupy (their sizes).
func diskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func newStore(t *testing.T) *snapshotStore {
	t.Helper()
	s, err := createSnapshotStore(filepath.Join(t.TempDir(), storeDirName))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func captureOnce(t *testing.T, store *snapshotStore, src string) string {
	t.Helper()
	c := store.capture()
	if err := c.add("world", src, nil, true); err != nil {
		t.Fatal(err)
	}
	tree, err := c.commit(nil)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func restoredImage(t *testing.T, store *snapshotStore, tree string) []string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "restored")
	t.Cleanup(func() { _ = removeTree(dst) })
	if err := store.materialize(tree, dst, nil); err != nil {
		t.Fatal(err)
	}
	return treeImage(t, filepath.Join(dst, "world"))
}

func assertRestores(t *testing.T, store *snapshotStore, tree string, want []string) {
	t.Helper()
	if got := restoredImage(t, store, tree); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("restored world differs:\n got %v\nwant %v", got, want)
	}
}

func storeWorld(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	dirs := []struct {
		p    string
		mode fs.FileMode
	}{{"", 0o750}, {"sub", 0o700}, {"sub/deep", 0o755}, {"empty", 0o711}, {"empty/nested", 0o700}, {"ro", 0o555}}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(src, d.p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct {
		p    string
		body []byte
		mode fs.FileMode
	}{
		{"a.txt", []byte("alpha\n"), 0o644},
		{"secret", []byte("s"), 0o600},
		{"run.sh", []byte("#!/bin/sh\n"), 0o755},
		{"zero", nil, 0o640},
		{"sp ace \"q\"\nnl", []byte("odd name"), 0o644},
		{"ro/kept", []byte("in a read-only directory"), 0o444},
		{"sub/deep/big.bin", randBytes(t, 3<<20+123), 0o600},
	} {
		if err := os.WriteFile(filepath.Join(src, f.p), f.body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(src, f.p), f.mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../a.txt", filepath.Join(src, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chmod(filepath.Join(src, dirs[i].p), dirs[i].mode)
	}
	t.Cleanup(func() { _ = removeTree(src) })
	return src
}

// A checkpoint restores byte for byte, with modes, links, odd names and
// empty and read-only directories; reading it never writes into the store,
// and a fork's store fetched from it restores the same world.
func TestStoreRestoresBytesModesAndLinks(t *testing.T) {
	src := storeWorld(t)
	want := treeImage(t, src)
	store := newStore(t)
	tree := captureOnce(t, store, src)
	before := storeState(t, store.dir)
	assertRestores(t, openSnapshotStore(store.dir), tree, want)
	if after := storeState(t, store.dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatal("restoring wrote into the store")
	}
	fork := newStore(t)
	if err := fork.fetchCheckpoints(store.dir, []string{tree}); err != nil {
		t.Fatal(err)
	}
	assertRestores(t, fork, tree, want)
	if after := storeState(t, store.dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatal("a fork's fetch wrote into the source store")
	}
	dst := filepath.Join(t.TempDir(), "r")
	if err := store.materialize(tree, dst, nil); err != nil {
		t.Fatal(err)
	}
	if err := removeTree(dst); err != nil {
		t.Fatalf("a materialized tree with read-only directories cannot be removed: %v", err)
	}
}

// A name that is not UTF-8 is refused before anything is published.
func TestStoreRefusesNamesThatAreNotUTF8(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a\xffb"), []byte("x"), 0o600); err != nil {
		t.Skipf("this filesystem refuses the name: %v", err)
	}
	store := newStore(t)
	c := store.capture()
	if err := c.add("world", src, nil, true); err == nil || !strings.Contains(err.Error(), "not UTF-8") {
		t.Fatalf("add = %v", err)
	}
	if refs, _ := os.ReadDir(filepath.Join(store.dir, "refs", "checkpoints")); len(refs) != 0 {
		t.Fatal("published a checkpoint")
	}
}

// The cost budget, in bytes on disk: an unchanged world adds none; a 10 KB
// append to an 8 MB file adds about a block; a same-size rewrite inside one
// mtime tick, and a rewrite of a whole large file (a compaction), are
// captured; every earlier checkpoint still restores.
func TestStoreCostBudget(t *testing.T) {
	src := t.TempDir()
	journal := filepath.Join(src, "journal")
	small := filepath.Join(src, "manifest")
	if err := os.WriteFile(journal, randBytes(t, 8<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	type point struct {
		tree string
		want []string
	}
	var points []point
	take := func() string {
		tree := captureOnce(t, store, src)
		points = append(points, point{tree, treeImage(t, src)})
		return tree
	}
	first := take()

	size := diskBytes(t, store.dir)
	start := time.Now()
	if again := take(); again != first {
		t.Fatalf("unchanged world: tree %s, was %s", again, first)
	}
	if grew := diskBytes(t, store.dir) - size; grew != 0 {
		t.Errorf("unchanged world added %d bytes", grew)
	}
	t.Logf("unchanged 8 MB world (every reused object rehashed): %v, 0 bytes", time.Since(start))

	f, _ := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0o600)
	f.Write(randBytes(t, 10<<10))
	f.Close()
	size = diskBytes(t, store.dir)
	start = time.Now()
	appended := take()
	grew := diskBytes(t, store.dir) - size
	if grew > 300<<10 {
		t.Errorf("a 10 KB append added %d bytes", grew)
	}
	t.Logf("10 KB append: %v, %d bytes on disk", time.Since(start), grew)

	// Same size, same mtime, within one tick of the last capture.
	fi, _ := os.Stat(small)
	_ = os.WriteFile(small, []byte("v2"), 0o600)
	_ = os.Chtimes(small, fi.ModTime(), fi.ModTime())
	big, _ := os.ReadFile(journal)
	big[100] ^= 0xff
	jfi, _ := os.Stat(journal)
	_ = os.WriteFile(journal, big, 0o600)
	_ = os.Chtimes(journal, jfi.ModTime(), jfi.ModTime())
	if racy := take(); racy == appended {
		t.Fatal("a same-size rewrite in one tick was not captured")
	}

	// A compaction rewrites the whole file.
	if err := os.WriteFile(journal, randBytes(t, 6<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	size = diskBytes(t, store.dir)
	take()
	t.Logf("compaction to 6 MB: %d bytes on disk", diskBytes(t, store.dir)-size)
	for _, p := range points {
		assertRestores(t, store, p.tree, p.want)
	}
}

// 300 checkpoints of a world that appends 10 KB to its 8 MB journal and
// rewrites a small file each time keep the store, on disk, within 3x the
// final world; the first, a middle and the last restore.
func TestStoreThreeHundredCheckpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("writes ~30 MB")
	}
	src := t.TempDir()
	journal := filepath.Join(src, "journal")
	if err := os.WriteFile(journal, randBytes(t, 8<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	trees := map[int]string{}
	images := map[int][]string{}
	start := time.Now()
	for i := 0; i < 300; i++ {
		f, _ := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0o600)
		f.Write(randBytes(t, 10<<10))
		f.Close()
		if err := os.WriteFile(filepath.Join(src, "manifest"), []byte(fmt.Sprint(i)), 0o600); err != nil {
			t.Fatal(err)
		}
		tree := captureOnce(t, store, src)
		if i == 0 || i == 150 || i == 299 {
			trees[i], images[i] = tree, treeImage(t, src)
		}
	}
	world, total := diskBytes(t, src), diskBytes(t, store.dir)
	t.Logf("300 checkpoints in %v: store %d bytes on disk, world %d (%.2fx)", time.Since(start), total, world, float64(total)/float64(world))
	if total > 3*world {
		t.Errorf("store %d bytes is over 3x the world's %d", total, world)
	}
	for i, tree := range trees {
		assertRestores(t, store, tree, images[i])
	}
	// Each capture writes its own packs; the store rolls them up as it
	// goes instead of keeping one or two per checkpoint.
	if n := storePacks(t, store.dir); n > 2*storePackLimit {
		t.Errorf("%d packs after 300 checkpoints", n)
	}
}

func storePacks(t *testing.T, dir string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}

// A fork's store holds the checkpoints it fetched and what they reach, and
// nothing a later checkpoint added.
func TestStoreForkHoldsOnlyItsCheckpoints(t *testing.T) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a"), []byte("first"), 0o600)
	store := newStore(t)
	early := captureOnce(t, store, src)
	_ = os.WriteFile(filepath.Join(src, "b"), []byte("only later"), 0o600)
	late := captureOnce(t, store, src)
	fork := newStore(t)
	if err := fork.fetchCheckpoints(store.dir, []string{early}); err != nil {
		t.Fatal(err)
	}
	if fork.hasCheckpoint(late) {
		t.Fatal("the fork holds a later checkpoint")
	}
	if got, _ := fork.present([]string{late, gitObjectID("blob", []byte("only later"))}); len(got) != 0 {
		t.Fatalf("the fork holds later objects: %v", got)
	}
	if _, err := fork.validate(early, nil); err != nil {
		t.Fatal(err)
	}
}

// A capture interrupted before each publication barrier publishes nothing
// the journal could name; the next capture recovers, and the store passes
// git fsck, survives gc --prune=now and restores. A ref left empty by a
// crash inside its write is replaced, never taken for the checkpoint.
func TestStoreRecoversFromInterruptedCaptures(t *testing.T) {
	src := storeWorld(t)
	want := treeImage(t, src)
	for _, point := range []string{"before-import", "after-import", "before-sync", "truncated-ref"} {
		t.Run(point, func(t *testing.T) {
			store := newStore(t)
			var tree string
			if point == "truncated-ref" {
				tree = captureOnce(t, store, src)
				if err := os.Truncate(filepath.Join(store.dir, "refs", "checkpoints", tree), 0); err != nil {
					t.Fatal(err)
				}
				if store.hasCheckpoint(tree) {
					t.Fatal("an empty ref passes for the checkpoint")
				}
				if _, err := store.validate(tree, nil); err == nil {
					t.Fatal("an empty ref validates")
				}
			} else {
				crash := errors.New("crash at " + point)
				storeHook = func(p string) error {
					if p == point {
						return crash
					}
					return nil
				}
				c := store.capture()
				if err := c.add("world", src, nil, true); err != nil {
					t.Fatal(err)
				}
				_, err := c.commit(nil)
				storeHook = func(string) error { return nil }
				if !errors.Is(err, crash) {
					t.Fatalf("commit = %v", err)
				}
			}
			again := openSnapshotStore(store.dir)
			got := captureOnce(t, again, src)
			if tree != "" && got != tree {
				t.Fatalf("recapture %s, was %s", got, tree)
			}
			for _, args := range [][]string{{"fsck", "--strict", "--no-progress"}, {"gc", "--quiet", "--prune=now"}} {
				if out, err := again.git(args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			assertRestores(t, openSnapshotStore(store.dir), got, want)
		})
	}
	t.Cleanup(func() { storeHook = func(string) error { return nil } })
}

// mkCheckpoint publishes, by hand, a checkpoint whose root holds entries
// (git mktree lines), and returns its tree.
func mkCheckpoint(t *testing.T, s *snapshotStore, lines ...string) string {
	t.Helper()
	cmd := s.git("mktree", "--missing")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(string(out))
	commit, err := s.git("-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", tree, "-m", "x").Output()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.git("update-ref", storeRefs+tree, strings.TrimSpace(string(commit))).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return tree
}

func putBlob(t *testing.T, s *snapshotStore, body string) string {
	t.Helper()
	cmd := s.git("hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// A checkpoint whose tree, meta or manifest is missing, substituted or
// inconsistent is refused before anything is restored.
func TestStoreValidationRefusesDamagedCheckpoints(t *testing.T) {
	s := newStore(t)
	file := putBlob(t, s, "content")
	manifest := func(adapters string) string {
		return putBlob(t, s, `{"version":2,"adapters":`+adapters+`}`)
	}
	meta := func(entries string) string {
		return putBlob(t, s, `{"version":2,"entries":{"":{"mode":`+fmt.Sprint(uint32(fs.ModeDir|0o700))+`}`+entries+`}}`)
	}
	fileMeta := `,"f":{"mode":420},"world":{"mode":` + fmt.Sprint(uint32(fs.ModeDir|0o700)) + `}`
	good := []string{
		"100644 blob " + file + "\tf",
		"100644 blob " + meta(fileMeta) + "\t" + storeMetaName,
		"100644 blob " + manifest(`{"world":{"v":1}}`) + "\t" + storeManifestName,
	}
	adapters := map[string]json.RawMessage{"world": json.RawMessage(`{"v":1}`)}
	if _, err := s.validate(mkCheckpoint(t, s, good...), adapters); err != nil {
		t.Fatalf("the good checkpoint: %v", err)
	}
	missingBlob := strings.Repeat("ab", 20)
	blockTree := func(names ...string) string {
		var lines []string
		for _, n := range names {
			lines = append(lines, "100644 blob "+file+"\t"+n)
		}
		cmd := s.git("mktree")
		cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	for name, lines := range map[string][]string{
		"newer meta":     {good[0], "100644 blob " + putBlob(t, s, `{"version":3,"entries":{}}`) + "\t" + storeMetaName, good[2]},
		"newer manifest": {good[0], good[1], "100644 blob " + putBlob(t, s, `{"version":3,"adapters":{}}`) + "\t" + storeManifestName},
	} {
		if _, err := s.validate(mkCheckpoint(t, s, lines...), adapters); !errors.Is(err, ErrNewerFormat) || !strings.Contains(err.Error(), "is format 3; this engine reads up to 2") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, lines := range map[string][]string{
		"no meta":            {good[0], good[2]},
		"no manifest":        {good[0], good[1]},
		"other adapters":     {good[0], good[1], "100644 blob " + manifest(`{"world":{"v":2}}`) + "\t" + storeManifestName},
		"path not in meta":   {good[0], "100644 blob " + file + "\textra", good[1], good[2]},
		"meta file not tree": {good[0], "100644 blob " + meta(fileMeta+`,"gone":{"mode":420}`) + "\t" + storeMetaName, good[2]},
		"missing blob":       {"100644 blob " + missingBlob + "\tf", good[1], good[2]},
		"missing subtree":    {"040000 tree " + missingBlob + "\tf", "100644 blob " + meta(`,"f":{"mode":`+fmt.Sprint(uint32(fs.ModeDir|0o700))+`}`) + "\t" + storeMetaName, good[2]},
		"exec bit differs":   {"100755 blob " + file + "\tf", good[1], good[2]},
		"blocks not numbered 0..n-1": {"040000 tree " + blockTree("000000000", "000000002") + "\tf",
			"100644 blob " + meta(`,"f":{"mode":420,"blocks":2}`) + "\t" + storeMetaName, good[2]},
		"block count differs": {"040000 tree " + blockTree("000000000", "000000001", "000000002") + "\tf",
			"100644 blob " + meta(`,"f":{"mode":420,"blocks":2}`) + "\t" + storeMetaName, good[2]},
	} {
		if _, err := s.validate(mkCheckpoint(t, s, lines...), adapters); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

// Captures publish while forks fetch and restore earlier checkpoints: every
// fork restores exactly the world its checkpoint recorded.
func TestStoreConcurrentCaptureAndFork(t *testing.T) {
	src := t.TempDir()
	store := newStore(t)
	var mu sync.Mutex
	published := map[string][]string{}
	var order []string
	record := func() {
		tree := captureOnce(t, store, src)
		mu.Lock()
		if _, ok := published[tree]; !ok {
			order = append(order, tree)
		}
		published[tree] = treeImage(t, src)
		mu.Unlock()
	}
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("0"), 0o600)
	record()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i < 40; i++ {
			_ = os.WriteFile(filepath.Join(src, "f"), []byte(fmt.Sprint(i)), 0o600)
			_ = os.WriteFile(filepath.Join(src, fmt.Sprintf("n%d", i)), randBytes(t, 4<<10), 0o600)
			record()
		}
	}()
	for forks := 0; ; forks++ {
		mu.Lock()
		tree := order[len(order)/2]
		want := published[tree]
		mu.Unlock()
		fork := newStore(t)
		if err := fork.fetchCheckpoints(store.dir, []string{tree}); err != nil {
			t.Fatal(err)
		}
		assertRestores(t, fork, tree, want)
		select {
		case <-done:
			t.Logf("%d forks during %d captures", forks+1, len(order))
			return
		default:
		}
	}
}

// A file whose bytes change between a capture's hashing and its storing
// refuses the capture: nothing is stored under the name of bytes it lacks.
func TestStoreRefusesAFileChangedWhileStored(t *testing.T) {
	src := t.TempDir()
	f := filepath.Join(src, "f")
	_ = os.WriteFile(f, []byte("hashed"), 0o600)
	store := newStore(t)
	storeHook = func(p string) error {
		if p == "before-spool" {
			_ = os.WriteFile(f, []byte("stored"), 0o600)
		}
		return nil
	}
	defer func() { storeHook = func(string) error { return nil } }()
	c := store.capture()
	if err := c.add("world", src, nil, true); !errors.Is(err, errWorldChanged) {
		t.Fatalf("add = %v", err)
	}
	if refs, _ := os.ReadDir(filepath.Join(store.dir, "refs", "checkpoints")); len(refs) != 0 {
		t.Fatal("published a checkpoint")
	}
}

func TestStoreRefusesParentSwappedForSymlink(t *testing.T) {
	src, outside := t.TempDir(), t.TempDir()
	parent := filepath.Join(src, "dir")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	// Matching bytes let the old hash check pass even after redirection.
	for _, dir := range []string{parent, outside} {
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("matching content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := newStore(t)
	previous := storeHook
	t.Cleanup(func() { storeHook = previous })
	storeHook = func(phase string) error {
		if phase != "before-spool" {
			return nil
		}
		if err := os.Rename(parent, filepath.Join(src, "moved")); err != nil {
			return err
		}
		return os.Symlink(outside, parent)
	}
	if err := store.capture().addRunData("run-data/records", src); err == nil {
		t.Fatal("capture followed the replacement parent to an outside file")
	}
}

// add stores the bytes it read before it returns, while the adapter still
// keeps its source quiescent: a change, a compaction or a removal of the
// source afterwards leaves the capture as it was read.
func TestStoreCaptureKeepsWhatAddRead(t *testing.T) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "small"), []byte("as read"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "journal"), randBytes(t, 3<<20), 0o600)
	want := treeImage(t, src)
	store := newStore(t)
	c := store.capture()
	if err := c.add("world", src, nil, true); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(src, "small"), []byte("changed after"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "journal"), randBytes(t, 2<<20), 0o600)
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	tree, err := c.commit(nil)
	if err != nil {
		t.Fatal(err)
	}
	assertRestores(t, store, tree, want)
}

// A checkpoint whose ref exists but whose closure lost an object is not
// reused: the identical world captured again restores it from what it
// reads, and only then is it named again.
func TestStoreRecaptureRepairsAMissingObject(t *testing.T) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a"), []byte("alpha"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "b"), []byte("beta"), 0o600)
	want := treeImage(t, src)
	origin := newStore(t)
	tree := captureOnce(t, origin, src)
	store := newStore(t)
	if err := store.fetchCheckpoints(origin.dir, []string{tree}); err != nil {
		t.Fatal(err)
	}
	// Explode the fetched pack so one blob can be removed.
	packs, _ := filepath.Glob(filepath.Join(store.dir, "objects", "pack", "*.pack"))
	for _, p := range packs {
		f, _ := os.Open(p)
		cmd := store.git("unpack-objects", "-q")
		cmd.Stdin = f
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		f.Close()
		_ = os.Remove(p)
		_ = os.Remove(strings.TrimSuffix(p, ".pack") + ".idx")
	}
	gone := gitObjectID("blob", []byte("beta"))
	if err := os.Remove(filepath.Join(store.dir, "objects", gone[:2], gone[2:])); err != nil {
		t.Fatal(err)
	}
	if _, err := store.validate(tree, nil); err == nil {
		t.Fatal("a checkpoint missing a blob validates")
	}
	again := openSnapshotStore(store.dir)
	if got := captureOnce(t, again, src); got != tree {
		t.Fatalf("recapture %s, was %s", got, tree)
	}
	assertRestores(t, openSnapshotStore(store.dir), tree, want)
}

// A store whose refs are packed (no loose refs/checkpoints) is the same
// store: opening it keeps every checkpoint; a directory that is not a git
// repository is refused and left as it is.
func TestStoreOpenKeepsAnExistingStore(t *testing.T) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a"), []byte("alpha"), 0o600)
	origin := newStore(t)
	tree := captureOnce(t, origin, src)
	mirror := filepath.Join(t.TempDir(), storeDirName)
	if out, err := gitCommand("", "clone", "--quiet", "--mirror", origin.dir, mirror).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	_ = os.RemoveAll(filepath.Join(mirror, "refs", "checkpoints"))
	if !openSnapshotStore(mirror).hasCheckpoint(tree) {
		t.Fatal("the mirror lacks the checkpoint")
	}
	s, err := createSnapshotStore(mirror)
	if err != nil {
		t.Fatal(err)
	}
	if !s.hasCheckpoint(tree) {
		t.Fatal("opening an existing store lost its checkpoint")
	}
	junk := filepath.Join(t.TempDir(), storeDirName)
	_ = os.MkdirAll(junk, 0o700)
	_ = os.WriteFile(filepath.Join(junk, "keep"), []byte("x"), 0o600)
	if _, err := createSnapshotStore(junk); err == nil {
		t.Fatal("a directory that is not a store was taken")
	}
	if _, err := os.Stat(filepath.Join(junk, "keep")); err != nil {
		t.Fatal("a directory that is not a store was removed")
	}
}

// Adapter metadata is compared exactly, numbers as written; a manifest
// adapter needs its directory.
func TestStoreAdaptersCompareExactly(t *testing.T) {
	a := map[string]json.RawMessage{"x": json.RawMessage(`{"n":9007199254740993}`)}
	b := map[string]json.RawMessage{"x": json.RawMessage(`{"n":9007199254740992}`)}
	if sameAdapters(a, b) {
		t.Error("integers above 2^53 compare equal")
	}
	if !sameAdapters(a, map[string]json.RawMessage{"x": json.RawMessage(`{ "n" : 9007199254740993 }`)}) {
		t.Error("the same value compares unequal")
	}
	// A tree validated once is not read again, but every record's metadata
	// is still compared with its manifest.
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o600)
	store := newStore(t)
	c := store.capture()
	if err := c.add("x", src, nil, false); err != nil {
		t.Fatal(err)
	}
	tree, err := c.commit(a)
	if err != nil {
		t.Fatal(err)
	}
	reader := openSnapshotStore(store.dir)
	if _, err := reader.validate(tree, a); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.validate(tree, b); err == nil {
		t.Error("a second record with other metadata passed a cached tree")
	}
	if _, err := reader.validate(tree, map[string]json.RawMessage{"x": a["x"], "other": json.RawMessage(`{}`)}); err == nil {
		t.Error("metadata naming another adapter passed")
	}
}

// An object present under its name but holding other bytes (same size, same
// kind: git cat-file --batch-check cannot tell) refuses a recapture that
// would reuse it, instead of publishing a checkpoint no fork can restore.
func TestStoreRecaptureRefusesADamagedObject(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "verified earlier in this process"}[warm], func(t *testing.T) {
			recaptureDamaged(t, warm)
		})
	}
}

func recaptureDamaged(t *testing.T, warm bool) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a"), []byte("alpha"), 0o600)
	origin := newStore(t)
	tree := captureOnce(t, origin, src)
	store := newStore(t)
	if err := store.fetchCheckpoints(origin.dir, []string{tree}); err != nil {
		t.Fatal(err)
	}
	packs, _ := filepath.Glob(filepath.Join(store.dir, "objects", "pack", "*.pack"))
	for _, p := range packs {
		f, _ := os.Open(p)
		cmd := store.git("unpack-objects", "-q")
		cmd.Stdin = f
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		f.Close()
		_ = os.Remove(p)
		_ = os.Remove(strings.TrimSuffix(p, ".pack") + ".idx")
	}
	if warm {
		// This process captures (and so verifies) the world first: a
		// later capture must still read the object again.
		captureOnce(t, openSnapshotStore(store.dir), src)
	}
	id := gitObjectID("blob", []byte("alpha"))
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write([]byte("blob 5\x00omega"))
	zw.Close()
	loose := filepath.Join(store.dir, "objects", id[:2], id[2:])
	_ = os.Chmod(loose, 0o600)
	if err := os.WriteFile(loose, z.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.present([]string{id}); !got[id] {
		t.Fatal("batch-check no longer sees the object: the probe is moot")
	}
	again := openSnapshotStore(store.dir)
	c := again.capture()
	err := c.add("world", src, nil, true)
	if err == nil {
		_, err = c.commit(nil)
	}
	if err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("a recapture reusing a damaged object = %v", err)
	}
}

// The live run-data digest skips .git entries as a capture does, so an
// unchanged checkpoint of run data holding a nested repository is still
// reused.
func TestRunDataDigestMatchesACaptureWithNestedGit(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "records", "scratch", "repo", ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "records", "scratch", "repo", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "records", "scratch")
	if err := os.WriteFile(filepath.Join(scratch, "wt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("repo/a.txt", filepath.Join(scratch, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(scratch, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newStore(t).capture()
	for _, sub := range []string{"records", "out", "prompt"} {
		if err := c.addRunData("run-data/"+sub, filepath.Join(root, sub)); err != nil {
			t.Fatal(err)
		}
	}
	live, err := runDataDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if live != c.runDataDigest() {
		t.Fatal("the live digest differs from the capture's")
	}
	if err := os.WriteFile(filepath.Join(root, "records", "scratch", "repo", ".git", "HEAD"), []byte("ref: refs/heads/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again, _ := runDataDigest(root); again != live {
		t.Fatal("a change inside .git changed the digest")
	}
}

// A fast-import that dies mid-stream is reported with its own fatal
// message, never as the writer's broken pipe.
func TestStoreImportFailureReportsFastImportsMessage(t *testing.T) {
	s := newStore(t)
	err := s.importStream(func(w *bufio.Writer) error {
		w.WriteString("bogus command\n")
		if err := w.Flush(); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond) // fast-import reads it and exits
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 64; i++ {
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		}
		return w.Flush()
	})
	if err == nil || !strings.Contains(err.Error(), "fatal") || !strings.Contains(err.Error(), "bogus command") {
		t.Fatalf("import failure: %v", err)
	}
}

// Only run data is captured as plain files without .git: an adapter's
// payload promises copy semantics, so a .git entry there refuses the capture
// before anything is published, naming the path (an earlier revision review round 1, P1).
func TestStoreAdapterPayloadWithGitIsRefused(t *testing.T) {
	for _, name := range []string{".git", ".GiT"} {
		src := t.TempDir()
		if err := os.MkdirAll(filepath.Join(src, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, name, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		store := newStore(t)
		c := store.capture()
		err := c.add("git/data", src, nil, false)
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "git/data/"+name) {
			t.Fatalf("%s in an adapter payload: %v", name, err)
		}
	}
}

// Case variants of .git are omitted from run data too: git refuses them as
// path components on every platform (review round 1, P2).
func TestStoreRunDataOmitsGitCaseVariants(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"records/.GiT/HEAD", "records/a/.GIT", "records/keep.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := newStore(t)
	c := store.capture()
	if err := c.addRunData("run-data/records", filepath.Join(root, "records")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.commit(nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := strings.Join(c.omittedPaths(), ","); got != "run-data/records/.GiT,run-data/records/a/.GIT" {
		t.Fatalf("omitted %s", got)
	}
	live, err := runDataDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if live != c.runDataDigest() {
		t.Fatal("the live digest differs from the capture's")
	}
}

// The reuse stamp holds the omitted names, not their contents: adding or
// removing a nested .git changes it, so a reused checkpoint never carries a
// stale Omitted list (review round 1, P2).
func TestRunDataDigestSeesGitAddedOrRemoved(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "records", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := runDataDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "records", "repo", ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	added, _ := runDataDigest(root)
	if added == before {
		t.Fatal("a new .git left the digest unchanged")
	}
	if err := os.WriteFile(filepath.Join(root, "records", "repo", ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _ := runDataDigest(root); changed != added {
		t.Fatal("a change inside .git changed the digest")
	}
}

// A capture's packs are read back whole once and trusted while unchanged, so
// a later capture of the same world reads none of its objects back; any write
// to a trusted pack voids its trust, and its objects are read again.
func TestStoreTrustsUnchangedPacksOnly(t *testing.T) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a"), []byte("alpha"), 0o600)
	store := newStore(t)
	captureOnce(t, store, src)
	id := gitObjectID("blob", []byte("alpha"))
	if !store.trustedObjects()[id] {
		t.Fatalf("the capture's pack is not trusted: %v", store.trustedPacks(false))
	}
	// A fresh process (store value) trusts it too, from the record.
	if !openSnapshotStore(store.dir).trustedObjects()[id] {
		t.Fatal("trust did not persist")
	}
	packs, _ := filepath.Glob(filepath.Join(store.dir, "objects", "pack", "*.pack"))
	for _, p := range packs {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, 0o600)
		if err := os.WriteFile(p, body, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if openSnapshotStore(store.dir).trustedObjects()[id] {
		t.Fatal("a rewritten pack kept its trust")
	}
	// The next capture reads the object back, then trusts the pack again.
	captureOnce(t, openSnapshotStore(store.dir), src)
	if !openSnapshotStore(store.dir).trustedObjects()[id] {
		t.Fatal("a re-read pack was not trusted again")
	}
}
