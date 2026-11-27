package wf

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Trust is also re-earned whenever a pack's size, mtime or ctime differs
// from what was recorded.
//
// A capture verifies, by content, every object it reuses (verify). Reading
// each one back on every capture makes a checkpoint of an unchanged world
// cost the world's size, so the store records which of its packs were read
// back whole: a pack, with its index, whose identity and stamps (hashKey)
// are unchanged since then holds the bytes that were verified, and its
// objects are not read again. Any write to a pack or its index through the
// file system changes its ctime and voids its trust; loose objects and new
// or rewritten packs are always read. A restore still reads and hashes
// every object it uses.
const storeTrustName = "wf-trusted-packs"

type packTrust struct{ pack, idx hashKey }

// trustedObjects are the ids of every object in a trusted, unchanged pack.
// It is read once per store value while the trusted set is the same, not
// once per verify (a capture verifies per directory it adds).
func (s *snapshotStore) trustedObjects() map[string]bool {
	packs := s.trustedPacks(true)
	names := make([]string, 0, len(packs))
	for name, t := range packs {
		names = append(names, fmt.Sprint(name, t))
	}
	sort.Strings(names)
	sig := strings.Join(names, "\n")
	s.mu.Lock()
	if s.trustIDs != nil && s.trustSig == sig {
		ids := s.trustIDs
		s.mu.Unlock()
		return ids
	}
	s.mu.Unlock()
	out := map[string]bool{}
	for name := range packs {
		ids, err := readPackIndex(filepath.Join(s.dir, "objects", "pack", strings.TrimSuffix(name, ".pack")+".idx"))
		if err != nil {
			continue
		}
		for _, id := range ids {
			out[id] = true
		}
	}
	s.mu.Lock()
	s.trustSig, s.trustIDs = sig, out
	s.mu.Unlock()
	return out
}

// trustedPacks is the trust record by pack file name; current keeps only
// the packs whose pack and index files are unchanged since recorded.
func (s *snapshotStore) trustedPacks(current bool) map[string]packTrust {
	out := map[string]packTrust{}
	raw, err := os.ReadFile(filepath.Join(s.dir, storeTrustName))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) != 11 || !strings.HasPrefix(f[0], "pack-") || !strings.HasSuffix(f[0], ".pack") || strings.ContainsRune(f[0], '/') {
			continue
		}
		var n [10]int64
		ok := true
		for i := range n {
			v, err := strconv.ParseInt(f[i+1], 10, 64)
			ok = ok && err == nil
			n[i] = v
		}
		if !ok {
			continue
		}
		key := func(w []int64) hashKey {
			return hashKey{dev: uint64(w[0]), ino: uint64(w[1]), size: w[2], mtime: w[3], ctime: w[4]}
		}
		t := packTrust{key(n[0:5]), key(n[5:10])}
		if current {
			pk, pok := s.packKey(f[0], ".pack")
			ik, iok := s.packKey(f[0], ".idx")
			if !pok || !iok || pk != t.pack || ik != t.idx {
				continue
			}
		}
		out[f[0]] = t
	}
	return out
}

func (s *snapshotStore) packKey(name, ext string) (hashKey, bool) {
	p := filepath.Join(s.dir, "objects", "pack", strings.TrimSuffix(name, ".pack")+ext)
	fi, err := os.Lstat(p)
	if err != nil {
		return hashKey{}, false
	}
	k, ok := statKey("", fi) // the record names the pack
	return k, ok
}

// trustPacks reads back whole every pack not yet trusted and records the
// ones that hold exactly their indexes' objects, each hashing to its name.
// It runs after a publication; a failure only leaves a pack untrusted, so a
// later capture reusing its objects reads them (and refuses a damaged one).
func (s *snapshotStore) trustPacks() {
	packs, err := filepath.Glob(filepath.Join(s.dir, "objects", "pack", "pack-*.pack"))
	if err != nil {
		return
	}
	old := s.trustedPacks(true)
	next := map[string]packTrust{}
	var r *objectReader
	defer func() {
		if r != nil {
			r.close()
		}
	}()
	for _, p := range packs {
		name := filepath.Base(p)
		if t, ok := old[name]; ok {
			next[name] = t
			continue
		}
		pk, pok := s.packKey(name, ".pack")
		ik, iok := s.packKey(name, ".idx")
		if !pok || !iok {
			continue
		}
		ids, err := readPackIndex(strings.TrimSuffix(p, ".pack") + ".idx")
		if err != nil {
			continue
		}
		if r == nil {
			if r, err = s.reader(); err != nil {
				return
			}
		}
		good := true
		for _, id := range ids {
			if _, err := r.get(id, ""); err != nil {
				good = false
				break
			}
		}
		// A pack rewritten meanwhile is not the one read.
		pk2, _ := s.packKey(name, ".pack")
		ik2, _ := s.packKey(name, ".idx")
		if good && pk2 == pk && ik2 == ik {
			next[name] = packTrust{pk, ik}
		}
	}
	var b bytes.Buffer
	for name, t := range next {
		fmt.Fprintf(&b, "%s %d %d %d %d %d %d %d %d %d %d\n", name, t.pack.dev, t.pack.ino, t.pack.size, t.pack.mtime, t.pack.ctime, t.idx.dev, t.idx.ino, t.idx.size, t.idx.mtime, t.idx.ctime)
	}
	tmp, err := os.CreateTemp(s.dir, storeTrustName+".tmp-")
	if err != nil {
		return
	}
	_, err = tmp.Write(b.Bytes())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(s.dir, storeTrustName))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
}

// readPackIndex is the object ids a version-2 pack index lists.
func readPackIndex(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var head [2]uint32
	if err := binary.Read(r, binary.BigEndian, &head); err != nil {
		return nil, err
	}
	if head[0] != 0xff744f63 || head[1] != 2 {
		return nil, errors.New("pack index: not version 2")
	}
	var fanout [256]uint32
	if err := binary.Read(r, binary.BigEndian, &fanout); err != nil {
		return nil, err
	}
	n := fanout[255]
	fi, err := f.Stat()
	if err != nil || int64(n)*20 > fi.Size() {
		return nil, errors.New("pack index: damaged")
	}
	ids := make([]string, 0, n)
	raw := make([]byte, 20)
	for range n {
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, err
		}
		ids = append(ids, hex.EncodeToString(raw))
	}
	return ids, nil
}
