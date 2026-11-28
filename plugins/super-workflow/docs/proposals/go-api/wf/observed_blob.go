package wf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// Observed blobs (sw-ta9r).
//
// Owns: keeping a large observation value out of the journal. An observed
// record whose value is longer than ObservedInline is journaled with Blob,
// its value's SHA-256 and size, and no Value; the value's bytes are in
// <run dir>/observed/<sha256>, written once and never rewritten. Reading the
// record back loads the bytes by hash and checks them, so a definition's
// Observed, the input digests and replay see the same value as an inline
// record.
// Does not: judge or reshape the value (the bytes are the adapter's, as an
// inline Value would hold them), or spill any other record's payload.
// Extend by: nothing; every observed record goes through emitObserved.

// ObservedInline is the longest observation value an observed record holds
// inline; a longer one is journaled as a Blob.
const ObservedInline = OutputTail

// ObservedBlob names the value of an observed record kept beside the journal.
type ObservedBlob struct {
	SHA256 string
	Size   int
}

// ObservedBlobDir is the directory of a run's observed blobs.
const ObservedBlobDir = "observed"

var observedBlobName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ObservedBlobPath is where runDir keeps the blob named sum.
func ObservedBlobPath(runDir, sum string) string {
	return filepath.Join(runDir, ObservedBlobDir, sum)
}

// ErrObservedBlob is a missing or damaged observed blob.
var ErrObservedBlob = errors.New("observed blob")

// emitObserved journals od. A value longer than ObservedInline is written to
// the run's blob directory first and journaled by Blob alone; od keeps its
// Value for the caller and gains the Blob. Without a run dir, or in a dry
// cycle, the value stays inline.
func (c *cycle) emitObserved(in *instance, od *ObservedData) error {
	rec := *od
	if dir := c.resultDir(); dir != "" && !c.dry && len(od.Value) > ObservedInline {
		b, err := writeObservedBlob(dir, od.Value)
		if err != nil {
			return err
		}
		od.Blob = &b
		rec.Blob, rec.Value = &b, nil
	}
	return c.emit(in, JournalObserved, 0, rec)
}

// writeObservedBlob stores value under its hash in runDir's blob directory.
// A blob already there is the same bytes by its name and is kept.
func writeObservedBlob(runDir string, value []byte) (ObservedBlob, error) {
	h := sha256.Sum256(value)
	b := ObservedBlob{SHA256: hex.EncodeToString(h[:]), Size: len(value)}
	p := ObservedBlobPath(runDir, b.SHA256)
	if _, err := os.Stat(p); err == nil {
		return b, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return b, fmt.Errorf("%w %s: %w", ErrObservedBlob, b.SHA256, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-")
	if err != nil {
		return b, fmt.Errorf("%w %s: %w", ErrObservedBlob, b.SHA256, err)
	}
	_, werr := tmp.Write(value)
	if serr := tmp.Sync(); werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), p)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return b, fmt.Errorf("%w %s: %w", ErrObservedBlob, b.SHA256, werr)
	}
	return b, nil
}

// readObservedBlob is the value of b from runDir, checked against its hash
// and size. The error names the blob.
func readObservedBlob(runDir string, b ObservedBlob) (json.RawMessage, error) {
	if !observedBlobName.MatchString(b.SHA256) {
		return nil, fmt.Errorf("%w %q: not a SHA-256 name", ErrObservedBlob, b.SHA256)
	}
	if runDir == "" {
		return nil, fmt.Errorf("%w %s: no run directory to read it from", ErrObservedBlob, b.SHA256)
	}
	p := ObservedBlobPath(runDir, b.SHA256)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w %s is absent: %s", ErrObservedBlob, b.SHA256, p)
	}
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrObservedBlob, b.SHA256, err)
	}
	h := sha256.Sum256(data)
	if len(data) != b.Size || hex.EncodeToString(h[:]) != b.SHA256 {
		return nil, fmt.Errorf("%w %s does not match its hash: %s", ErrObservedBlob, b.SHA256, p)
	}
	return data, nil
}

// observedValue is od's value: inline, or loaded from runDir by its Blob.
func observedValue(runDir string, od ObservedData) (json.RawMessage, error) {
	if od.Blob == nil || len(od.Value) > 0 {
		return od.Value, nil
	}
	return readObservedBlob(runDir, *od.Blob)
}

// loadObserved fills od.Value from its Blob when the record held none.
func (c *cycle) loadObserved(od *ObservedData) error {
	v, err := observedValue(c.resultDir(), *od)
	if err != nil {
		return err
	}
	od.Value = v
	return nil
}

// observedBlobs is every blob the observed records of recs name, once each.
func observedBlobs(recs []JournalRecord) []ObservedBlob {
	var out []ObservedBlob
	seen := map[string]bool{}
	for _, r := range recs {
		if r.Kind != JournalObserved || !bytes.Contains(r.Data, []byte(`"Blob"`)) {
			continue
		}
		var od ObservedData
		if json.Unmarshal(r.Data, &od) == nil && od.Blob != nil && !seen[od.Blob.SHA256] {
			seen[od.Blob.SHA256] = true
			out = append(out, *od.Blob)
		}
	}
	return out
}

// copyObservedBlobs copies the blobs recs name from one run dir to another,
// each checked against its hash. A blob missing from the source is an error
// naming it: the copy's journal would name bytes it cannot read.
func copyObservedBlobs(recs []JournalRecord, from, to string) error {
	for _, b := range observedBlobs(recs) {
		v, err := readObservedBlob(from, b)
		if err != nil {
			return err
		}
		if _, err := writeObservedBlob(to, v); err != nil {
			return err
		}
	}
	return nil
}
