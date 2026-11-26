package wf

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// hashCache remembers the blob ids (fileIDs) of files a capture hashed, by
// the file's identity and change stamps, so a later capture of an unchanged
// file reads nothing: checkpoints cost what changed, not the world's size.
// A file is the same file with the same bytes while its device, inode, size,
// mtime and ctime are all unchanged; ctime cannot be set from user space, so
// any write or rename-over shows. An entry is recorded only for a file whose
// mtime and ctime are older than hashCacheRace before it was hashed (git's
// racy-index rule): a write in the same timestamp tick as the hash cannot hide.
//
// The cache lives in one file of the run's snapshot store (hashCacheName),
// never fetched into a fork (inodes are local). A missing, unreadable or
// damaged file is an empty cache. Lookups and additions are in memory; save
// writes the entries this process used or added, so files no longer captured
// drop out. Callers hold the run lock, so saves never race. The file ends in
// a SHA-256 of what precedes it; one that does not match is empty.
type hashCache struct {
	mu   sync.Mutex
	path string
	old  map[hashKey][]string // as loaded
	used map[hashKey][]string // looked up or added by this process
}

type hashKey struct {
	path               string // the file's path when hashed: inode reuse elsewhere is never a hit
	dev, ino           uint64
	size, mtime, ctime int64
}

const (
	hashCacheName    = "wf-hash-cache"
	hashCacheVersion = 2
	hashCacheRace    = 2 * time.Second
)

var (
	hashCachesMu sync.Mutex
	hashCaches   = map[string]*hashCache{}
)

// hashCacheFor is the process's cache for the store directory storeDir,
// loaded on first use.
func hashCacheFor(storeDir string) *hashCache {
	hashCachesMu.Lock()
	defer hashCachesMu.Unlock()
	if c := hashCaches[storeDir]; c != nil {
		return c
	}
	c := &hashCache{path: filepath.Join(storeDir, hashCacheName), used: map[hashKey][]string{}}
	c.old, _ = loadHashCache(c.path)
	hashCaches[storeDir] = c
	return c
}

func statKey(path string, fi fs.FileInfo) (hashKey, bool) {
	if !fi.Mode().IsRegular() {
		return hashKey{}, false
	}
	k, ok := statIdentity(fi)
	k.path = path
	return k, ok
}

// get is the ids recorded for the file at path with fi's identity and stamps.
func (c *hashCache) get(path string, fi fs.FileInfo) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	k, ok := statKey(path, fi)
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ids, ok := c.used[k]
	if !ok {
		if ids, ok = c.old[k]; ok {
			c.used[k] = ids
		}
	}
	return ids, ok
}

// put records ids for the file fi described when hashing began at start.
func (c *hashCache) put(path string, fi fs.FileInfo, start time.Time, ids []string) {
	if c == nil {
		return
	}
	k, ok := statKey(path, fi)
	if !ok {
		return
	}
	limit := start.Add(-hashCacheRace).UnixNano()
	if k.mtime >= limit || k.ctime >= limit {
		return
	}
	c.mu.Lock()
	c.used[k] = ids
	c.mu.Unlock()
}

// save writes the entries this process used or added, atomically; a failure
// leaves the old cache, which only costs a later capture its hashing.
func (c *hashCache) save() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(c.path), hashCacheName+".tmp-")
	if err != nil {
		return
	}
	var body bytes.Buffer
	err = writeHashCache(&body, c.used)
	if err == nil {
		sum := sha256.Sum256(body.Bytes())
		body.Write(sum[:])
		_, err = tmp.Write(body.Bytes())
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return
	}
	// What the next save writes is what is used from now on, so entries for
	// files no longer captured drop out within a process too.
	c.old = c.used
	c.used = map[hashKey][]string{}
}

// The file is a version word, then per entry the path's length and bytes,
// the key's five words, the id count and each id's 20 raw bytes, all
// little-endian, then the SHA-256 of all of that.
func writeHashCache(w io.Writer, m map[hashKey][]string) error {
	put := func(v uint64) error { return binary.Write(w, binary.LittleEndian, v) }
	if err := put(hashCacheVersion); err != nil {
		return err
	}
	for k, ids := range m {
		if err := put(uint64(len(k.path))); err != nil {
			return err
		}
		if _, err := io.WriteString(w, k.path); err != nil {
			return err
		}
		for _, v := range []uint64{k.dev, k.ino, uint64(k.size), uint64(k.mtime), uint64(k.ctime), uint64(len(ids))} {
			if err := put(v); err != nil {
				return err
			}
		}
		for _, id := range ids {
			raw, err := hex.DecodeString(id)
			if err != nil || len(raw) != 20 {
				return errors.New("hash cache: bad id")
			}
			if _, err := w.Write(raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadHashCache(path string) (map[hashKey][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < sha256.Size+8 {
		return nil, errors.New("hash cache: short")
	}
	body, sum := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if got := sha256.Sum256(body); !bytes.Equal(got[:], sum) {
		return nil, errors.New("hash cache: damaged")
	}
	r := bytes.NewReader(body)
	var v uint64
	if err := binary.Read(r, binary.LittleEndian, &v); err != nil || v != hashCacheVersion {
		return nil, errors.New("hash cache: unknown version")
	}
	m := map[hashKey][]string{}
	var words [6]uint64
	id := make([]byte, 20)
	for r.Len() > 0 {
		var n uint64
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil || n > uint64(r.Len()) {
			return nil, errors.New("hash cache: damaged")
		}
		path := make([]byte, n)
		if _, err := io.ReadFull(r, path); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &words); err != nil {
			return nil, err
		}
		if words[5] > uint64(r.Len())/20 {
			return nil, errors.New("hash cache: damaged")
		}
		ids := make([]string, 0, words[5])
		for range words[5] {
			if _, err := io.ReadFull(r, id); err != nil {
				return nil, err
			}
			ids = append(ids, hex.EncodeToString(id))
		}
		m[hashKey{string(path), words[0], words[1], int64(words[2]), int64(words[3]), int64(words[4])}] = ids
	}
	return m, nil
}
