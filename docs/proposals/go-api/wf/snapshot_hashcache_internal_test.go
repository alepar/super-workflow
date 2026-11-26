package wf

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func hashOf(t *testing.T, cache *hashCache, path string) []string {
	t.Helper()
	dir, err := openSnapshotDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	ids, err := fileIDs(dir, filepath.Base(path), make([]byte, storeBlock), cache)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// An unchanged file's ids come from the cache, through a save and a load;
// a rewrite that keeps size and mtime still changes ctime and is hashed
// again; a file written within hashCacheRace of its hashing is not cached.
func TestHashCacheTrustsOnlyUnchangedSettledFiles(t *testing.T) {
	store := t.TempDir()
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	c := &hashCache{path: filepath.Join(store, hashCacheName), used: map[hashKey][]string{}}
	want := gitObjectID("blob", []byte("alpha"))
	if ids := hashOf(t, c, src); len(ids) != 1 || ids[0] != want {
		t.Fatalf("ids %v", ids)
	}
	if len(c.used) != 0 {
		// ctime is now: the file is too fresh to trust yet.
		t.Fatalf("a just-changed file was cached: %v", c.used)
	}
	// Age the cache's notion of now instead of the file's ctime, which user
	// space cannot set: record the entry as if hashed later.
	fi, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	c.put(src, fi, time.Now().Add(time.Hour), []string{want})
	c.save()
	loaded := &hashCache{path: c.path, used: map[hashKey][]string{}}
	loaded.old, err = loadHashCache(c.path)
	if err != nil || len(loaded.old) != 1 {
		t.Fatalf("load %v %v", loaded.old, err)
	}
	if ids, ok := loaded.get(src, fi); !ok || ids[0] != want {
		t.Fatalf("cached ids %v %v", ids, ok)
	}
	// The cache answers without reading: a lie it holds is what fileIDs
	// returns while the file's stamps are unchanged.
	loaded.old[mustKey(t, src, fi)] = []string{"0000000000000000000000000000000000000000"}
	loaded.used = map[hashKey][]string{}
	if ids := hashOf(t, loaded, src); ids[0] != "0000000000000000000000000000000000000000" {
		t.Fatalf("an unchanged file was read again: %v", ids)
	}
	// The same file under another path (a hard link) is not a hit.
	other := filepath.Join(filepath.Dir(src), "g")
	if err := os.Link(src, other); err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.get(other, fi); ok {
		t.Fatal("a cached entry answered for another path")
	}
	// Same size, mtime put back: ctime moved, so the file is hashed again.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(src, []byte("omega"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	if ids := hashOf(t, loaded, src); ids[0] != gitObjectID("blob", []byte("omega")) {
		t.Fatalf("a rewritten file kept its cached ids: %v", ids)
	}
}

func mustKey(t *testing.T, path string, fi os.FileInfo) hashKey {
	k, ok := statKey(path, fi)
	if !ok {
		t.Fatal("no stat key")
	}
	return k
}

// A damaged or foreign cache file is an empty cache.
func TestHashCacheIgnoresDamagedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), hashCacheName)
	for _, body := range []string{"", "garbage", "\x01\x00\x00\x00\x00\x00\x00\x00\x01"} {
		os.WriteFile(p, []byte(body), 0o600)
		if m, err := loadHashCache(p); err == nil && len(m) > 0 {
			t.Fatalf("%q loaded as %v", body, m)
		}
	}
}
