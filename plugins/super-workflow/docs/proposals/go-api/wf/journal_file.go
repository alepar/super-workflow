package wf

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// fileLockWait bounds how long Lock retries a held lock when the caller's
// harness sets no budget (setLockDeadline). It outlasts the longest chain a
// verb drives under the lock (a done's merge, build check, close, snapshots
// and the next round's preparation), so a busy
// run is waited for, never reported as a child's problem. A test shortens it.
var fileLockWait = 2 * time.Minute

// lockProgressAfter is how long a lock wait runs before it prints its one
// progress line naming the holder.
var lockProgressAfter = 5 * time.Second

// fileLockPoll is how often a waiting call tries the run lock (and the gate
// before it). Every call of a run is serialised on that lock, so the time a
// lock sits free before the next waiter sees it adds to every handover; a
// try is one flock syscall.
const fileLockPoll = 5 * time.Millisecond

// fileWatchPoll is how often Watch stats the journal file.
var fileWatchPoll = 500 * time.Millisecond

const journalFile = "journal.jsonl"

type fileJournal struct {
	dir string
	// until, when set, is when every lock wait of this value gives up,
	// however many locks it takes (setLockDeadline); zero waits
	// fileLockWait per lock.
	until time.Time
	// owner is what this value's lock record says it runs for, and progress
	// is where a long lock wait says what it waits for (setLockOwner).
	owner    lockOwner
	progress io.Writer
	// committed, when set, collects the run and kind of every record this
	// value appends: what an invocation has already changed.
	committed *[]committedRecord
	mu        sync.Mutex // guards held and prev
	held      map[RunID]*fileRunLock
	prev      map[RunID]LockHolder // the record found in each run's lock file when this value locked it
}

// journalCache is a journal file as this process last read it: raw is its
// committed bytes, recs their records. A read whose file starts with exactly
// raw parses only what follows; any other file is read in full.
type journalCache struct {
	raw  []byte
	recs []JournalRecord
}

// readCaches holds each journal file's last read, by path, for every
// FileJournal value in the process: a later read, under the lock or not,
// parses only the lines appended since. One verb takes the lock several
// times, through more than one engine; without it each read parses the
// whole file (0.85 s of a 217 MB journal, sw-989). Comparing the cached
// bytes costs a few ms; it is what proves the prefix unchanged, so two
// values that name one file by different paths only miss.
var readCaches = struct {
	sync.Mutex
	m map[string]*journalCache
}{m: map[string]*journalCache{}}

// fileRunLock is one held lock. next is the Seq the next Append assigns; 0
// means the file has not been read since the lock was taken. f is the lock
// file. claimed reports that its holder record names this holder: it is
// written by the first Append, so an acquisition that appends nothing (a
// read, a refusal) leaves the lock file as it found it. torn, when set, is
// where a torn final line of the journal starts: the first Append cuts it
// off, a read only skips it.
type fileRunLock struct {
	next    int64
	f       *os.File
	claimed bool
	torn    int64 // -1: none
	// recorded is whether this lease wrote its holder record at
	// acquisition; orig is the lock file's contents it replaced, put back
	// on release when the lease appended nothing.
	recorded bool
	orig     []byte
	origMod  time.Time
}

// release closes the lease's descriptor, which lets the flock go; a lease
// whose lock was already let go (a yield that could not take it back) has
// none. A lease that appended marks its holder record released first, while
// it still holds the lock, so the record left behind is never read as a live
// holder; a lease that only read puts the file back as it found it.
func (l *fileRunLock) release() {
	if l.f != nil {
		switch {
		case l.claimed:
			markReleased(l.f) // best effort: the flock, not the record, is the lock
		case l.recorded:
			restoreLockFile(l.f, l.orig, l.origMod)
		}
		l.f.Close()
		l.f = nil
	}
}

// restoreLockFile puts back the contents a reading lease's holder record
// replaced, and the file's modification time, so a read leaves the run
// directory as it was (best effort, like the record itself).
func restoreLockFile(f *os.File, orig []byte, mod time.Time) {
	if err := f.Truncate(0); err != nil {
		return
	}
	if len(orig) > 0 {
		if _, err := f.WriteAt(orig, 0); err != nil {
			return
		}
	}
	_ = f.Sync()
	if !mod.IsZero() {
		_ = os.Chtimes(f.Name(), time.Time{}, mod) // a zero atime is left as it is
	}
}

// FileJournal is a Journal with one JSON Lines file per run at
// <dir>/runs/<run-id>/journal.jsonl, fsynced on every append. The lock is a
// flock on <dir>/runs/<run-id>/lock, so it excludes other processes and other
// FileJournal values alike.
func FileJournal(dir string) Journal {
	return &fileJournal{dir: dir, held: map[RunID]*fileRunLock{}, prev: map[RunID]LockHolder{}}
}

func (j *fileJournal) runDir(run RunID) string { return filepath.Join(j.dir, "runs", string(run)) }

func (j *fileJournal) Lock(run RunID) (func(), error) {
	if err := validRunID(run); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(j.runDir(run), 0o755); err != nil {
		return nil, err
	}
	// Each Lock is its own lease: a yield takes the lock back into the same
	// lease, and release lets go of this lease's lock only, never another
	// call's that holds the run now.
	l := &fileRunLock{}
	if err := j.acquire(run, l); err != nil {
		return nil, err
	}
	return func() {
		j.mu.Lock()
		if j.held[run] == l {
			delete(j.held, run)
		}
		j.mu.Unlock()
		l.release()
	}, nil
}

// acquire takes run's lock through its turnstile and records it held: the
// gate first, then the lock, then the gate is let go. A waiter holds the gate
// while it waits, so a holder that lets the lock go to it (yieldLock) cannot
// take it straight back: it queues on the gate behind the waiter.
func (j *fileJournal) acquire(run RunID, l *fileRunLock) error {
	f, err := os.OpenFile(filepath.Join(j.runDir(run), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	// The gate is made by a run's first appending acquisition (claim), so a
	// call that only reads a run, or is refused, creates no file in it
	//; without one, this acquisition waits on the lock alone.
	gate, err := os.OpenFile(filepath.Join(j.runDir(run), gateFile), os.O_RDWR, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		gate = nil
	case err != nil:
		f.Close()
		return err
	default:
		defer gate.Close() // closing the gate's only descriptor lets it go
	}
	deadline := time.Now().Add(fileLockWait)
	if !j.until.IsZero() {
		deadline = j.until
	}
	start := time.Now()
	printed := false
	waiting := func() {
		if j.progress == nil || printed {
			return
		}
		printed = true
		fmt.Fprintf(j.progress, "waiting for the run: held by %s; this wait is normal and ends on its own, within %s, the most this call waits for the lock%s\n", heldBy(f), max(time.Until(deadline), 0).Round(time.Second), lockWaitTail(j.owner.Verb))
	}
	err = nil
	if gate != nil {
		err = flockWait(gate, deadline, start, waiting)
	}
	if err == nil {
		err = flockWait(f, deadline, start, waiting)
	}
	if err != nil {
		who := heldBy(f)
		f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock run %s: %w", run, err)
		}
		return fmt.Errorf("%w: held by %s", ErrRunLocked, who)
	}
	// The holder record is written here, at acquisition, so a call that
	// holds the lock without appending is named to the calls that wait for
	// it. A lease that appends nothing puts the file's earlier contents back
	// on release, so a run another run's agent only read or was refused on
	// stays as it was.
	prev, hadPrev := readHolder(f)
	orig, rerr := io.ReadAll(io.NewSectionReader(f, 0, lockFileMax))
	var origMod time.Time
	if fi, err := f.Stat(); err == nil {
		origMod = fi.ModTime()
	}
	recorded := rerr == nil && writeHolder(f, j.owner) == nil
	j.mu.Lock()
	// Each acquisition writes its own holder record: after a yield, the
	// record names this lease again, not the call that went in between.
	l.f, l.next, l.claimed, l.torn, l.recorded, l.orig, l.origMod = f, 0, false, -1, recorded, orig, origMod
	j.held[run] = l
	if hadPrev {
		j.prev[run] = prev
	} else {
		delete(j.prev, run)
	}
	j.mu.Unlock()
	return nil
}

// othersWaiting reports whether another call waits for run's lock: it holds
// the turnstile's gate (acquire). A holder never holds the gate itself.
func (j *fileJournal) othersWaiting(run RunID) bool {
	gate, err := os.OpenFile(filepath.Join(j.runDir(run), gateFile), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer gate.Close()
	err = syscall.Flock(int(gate.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// yieldLock lets run's held lock go to the call waiting for it and takes it
// back through the turnstile, after that call. The journal is whole at
// every record, so what the waiter appends is read like any other process's
// (Records parses only the new lines). When the lock cannot be taken back in
// time, run is no longer held and the error says so: the caller stops
// appending, and the run's next driver goes on from the journal.
func (j *fileJournal) yieldLock(run RunID) error {
	j.mu.Lock()
	l, ok := j.held[run]
	if ok {
		delete(j.held, run)
	}
	j.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrJournalNotLocked, run)
	}
	l.release()
	if err := j.acquire(run, l); err != nil {
		return fmt.Errorf("yielded the run lock to a waiting call and could not take it back: %w", err)
	}
	return nil
}

// flockWait takes the flock without blocking, polling until deadline has
// passed (one try when it has already); the last error is EWOULDBLOCK when
// the lock stayed held. waiting, when set, runs once, after the wait has
// lasted lockProgressAfter since start.
func flockWait(f *os.File, deadline, start time.Time, waiting func()) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			return err
		case !time.Now().Before(deadline):
			return err
		}
		if waiting != nil && time.Since(start) >= lockProgressAfter {
			waiting()
			waiting = nil
		}
		time.Sleep(fileLockPoll)
	}
}

// setLockDeadline makes every lock wait of j end at until, for a process
// that must answer before its caller's tool call is cut off: the verb's
// waits together, not each, stay under the limit. A journal that is not a
// file journal is left as it is.
func setLockDeadline(j Journal, until time.Time) {
	switch x := j.(type) {
	case *fileJournal:
		x.until = until
	case *rootedJournal:
		setLockDeadline(x.Journal, until)
	}
}

// setLockOwner makes j's lock record name what the verb runs for, its long
// lock waits print their progress line to progress (nil: none), and every
// record it appends is noted in committed (nil: none). A journal that is not
// a file journal is left as it is.
func setLockOwner(j Journal, o lockOwner, progress io.Writer, committed *[]committedRecord) {
	switch x := j.(type) {
	case *fileJournal:
		x.owner, x.progress, x.committed = o, progress, committed
	case *rootedJournal:
		setLockOwner(x.Journal, o, progress, committed)
	}
}

// lockWaitTail is what a lock wait's progress line adds for verb: a
// coordinator's next is told the wait is no stop and what to do past it.
func lockWaitTail(verb string) string {
	if verb != "next" {
		return ""
	}
	return ". A next waiting for the lock is not a stop: do not interrupt it. If it runs past that, run status, then next again"
}

// lockOwner is what a CLI verb's lock record says it runs for.
type lockOwner struct{ Verb, Unit, Session string }

// committedRecord is one record an invocation appended: the run it belongs
// to (a rewind or fork writes the new run, not its source) and its kind.
type committedRecord struct {
	Run  RunID
	Kind JournalKind
}

type holderRecord struct {
	Pid     int       `json:"pid"`
	Since   time.Time `json:"since"`
	Verb    string    `json:"verb,omitempty"`
	Unit    string    `json:"unit,omitempty"`
	Session string    `json:"session,omitempty"`
	Host    string    `json:"host,omitempty"`
	PidNS   uint64    `json:"pidns,omitempty"`
}

// releaseStamp is the line a lease's release writes after the holder record
// (markReleased): the record itself is never rewritten to carry it.
type releaseStamp struct {
	Released time.Time `json:"released"`
}

// readHolder reads the holder record from the lock file, and its release
// stamp when a whole one follows it; ok is false when the file is empty or
// the record does not parse. A stamp cut short is no release.
func readHolder(f *os.File) (LockHolder, bool) {
	data, err := io.ReadAll(io.NewSectionReader(f, 0, lockFileMax))
	line, stamp, _ := bytes.Cut(data, []byte("\n"))
	var r holderRecord
	if err != nil || len(line) > holderRecordMax || json.Unmarshal(line, &r) != nil || r.Pid == 0 {
		return LockHolder{}, false
	}
	h := LockHolder{Pid: r.Pid, Since: r.Since, Verb: r.Verb, Unit: r.Unit, Session: r.Session, Host: r.Host, PidNS: r.PidNS}
	var s releaseStamp
	if json.Unmarshal(stamp, &s) == nil {
		h.Released = s.Released
	}
	return h, true
}

// writeHolder replaces the lock file's contents with this process's record. It
// stays in place on release; the next holder overwrites it.
// The encoded record is bounded to what readHolder reads: while it is
// longer, its longest text field is halved and it is encoded again (JSON
// escaping can make a field six times its raw length).
func writeHolder(f *os.File, o lockOwner) error {
	host, _ := os.Hostname()
	r := holderRecord{Pid: os.Getpid(), Since: time.Now().UTC(), Verb: o.Verb, Unit: o.Unit, Session: o.Session, Host: host, PidNS: pidNamespace("self")}
	data, err := json.Marshal(r)
	for err == nil && len(data) > holderRecordMax {
		longest := slices.MaxFunc([]*string{&r.Verb, &r.Unit, &r.Session, &r.Host}, func(a, b *string) int { return len(*a) - len(*b) })
		cut := halve(*longest)
		if len(cut) >= len(*longest) {
			break // nothing left to cut
		}
		*longest = cut
		data, err = json.Marshal(r)
	}
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return err
	}
	return f.Sync()
}

// markReleased writes the release stamp on its own line after the holder
// record in f, in place (the file and its inode stay). The record is never
// truncated or rewritten, so a short write or a crash mid-stamp leaves it
// whole, and readHolder reads a torn stamp as no release. Each claim's
// writeHolder replaces the file, so a record has at most one stamp; the
// truncate after the write only cuts what lies past this one.
func markReleased(f *os.File) {
	data, err := io.ReadAll(io.NewSectionReader(f, 0, holderRecordMax+1))
	line, _, _ := bytes.Cut(data, []byte("\n"))
	var r holderRecord
	if err != nil || len(line) > holderRecordMax || json.Unmarshal(line, &r) != nil || r.Pid == 0 {
		return
	}
	stamp, err := json.Marshal(releaseStamp{Released: time.Now().UTC()})
	if err != nil || len(stamp) > releaseStampMax {
		return
	}
	tail := append([]byte("\n"), stamp...)
	if _, err := f.WriteAt(tail, int64(len(line))); err != nil {
		return
	}
	_ = f.Truncate(int64(len(line) + len(tail)))
	_ = f.Sync()
}

// lastHolder is the record of the run's previous holder, as found in the lock
// file when this value last took the lock.
func (j *fileJournal) lastHolder(run RunID) (LockHolder, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	h, ok := j.prev[run]
	return h, ok
}

func (j *fileJournal) lockOf(run RunID) (*fileRunLock, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	l, ok := j.held[run]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrJournalNotLocked, run)
	}
	return l, nil
}

func (j *fileJournal) Append(r JournalRecord) error {
	l, err := j.lockOf(r.Run)
	if err != nil {
		return err
	}
	if l.next == 0 {
		if _, err := j.load(r.Run, l); err != nil {
			return err
		}
	}
	if err := j.claim(r.Run, l); err != nil {
		return err
	}
	r.Seq = l.next
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := j.write(r.Run, append(line, '\n')); err != nil {
		l.next = 0 // a partial write may be on disk: re-read, which truncates it
		return err
	}
	l.next++
	if j.committed != nil {
		*j.committed = append(*j.committed, committedRecord{Run: r.Run, Kind: r.Kind})
	}
	return nil
}

// write appends and fsyncs; the first write of a run also fsyncs the directory
// so the new file survives a crash.
func (j *fileJournal) write(run RunID, line []byte) error {
	path := filepath.Join(j.runDir(run), journalFile)
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		return syncPath(j.runDir(run))
	}
	return nil
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (j *fileJournal) Records(run RunID) ([]JournalRecord, error) {
	l, err := j.lockOf(run)
	if err != nil {
		return nil, err
	}
	recs, err := j.load(run, l)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRun, run)
	}
	return recs, nil
}

// claim makes the held lock's file name this holder and cuts off a torn
// final journal line, before the first record this acquisition appends:
// the writes a read-only acquisition never makes.
func (j *fileJournal) claim(run RunID, l *fileRunLock) error {
	if l.torn >= 0 {
		path := filepath.Join(j.runDir(run), journalFile)
		if err := os.Truncate(path, l.torn); err != nil {
			return err
		}
		if err := syncPath(path); err != nil {
			return err
		}
		l.torn = -1
	}
	if !l.claimed {
		if !l.recorded {
			if err := writeHolder(l.f, j.owner); err != nil {
				return fmt.Errorf("lock run %s: %w", run, err)
			}
			l.recorded = true
		}
		gate, err := os.OpenFile(filepath.Join(j.runDir(run), gateFile), os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return fmt.Errorf("lock run %s: %w", run, err)
		}
		gate.Close()
		l.claimed = true
	}
	return nil
}

// gateFile is the turnstile's gate beside a run's lock file (acquire).
const gateFile = "lock.gate"

// load reads the run's file under the lock and sets l.next, noting a torn
// final line in l.torn for the first Append to cut off. A final line with no
// newline did not commit, even if it parses.
// Its records come from the process's read cache where they can
// (parseCached).
func (j *fileJournal) load(run RunID, l *fileRunLock) ([]JournalRecord, error) {
	path := filepath.Join(j.runDir(run), journalFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		setCachedRead(path, nil)
		l.next = 1
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	recs, off, err := parseCached(path, data)
	if err != nil {
		return nil, err
	}
	if off < len(data) {
		l.torn = int64(off)
	}
	l.next = int64(len(recs) + 1)
	return copyRecords(recs), nil
}

// parseCached is parseCommitted of the journal file at path, read as data:
// when data begins with the very bytes the process read of path last time,
// their records come from the cache and only the rest is parsed; any other
// content, a rewrite or a shortening anywhere, is parsed in full. The cache
// then holds data's committed bytes. The records returned are the cache's:
// a caller hands out copies (copyRecords). A CLI process's soft memory
// limit grows with the file (raiseMemoryLimit).
func parseCached(path string, data []byte) ([]JournalRecord, int, error) {
	raiseMemoryLimit(len(data))
	var recs []JournalRecord
	base := 0
	if c := cachedRead(path); c != nil && len(data) >= len(c.raw) && bytes.Equal(data[:len(c.raw)], c.raw) {
		recs, base = c.recs, len(c.raw)
	}
	recs = slices.Clip(recs) // appending below never writes into the cached array
	recs, off, err := parseCommitted(data, base, recs)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s %v", ErrCorruptJournal, path, err)
	}
	setCachedRead(path, &journalCache{raw: data[:off:off], recs: recs})
	return recs, off, nil
}

// parseCommitted parses data's committed records from off on, after recs:
// every newline-terminated line, in Seq order. A final line that has no
// newline, or that does not parse, did not commit and ends the records (a
// torn write); a bad line before the last is an error. end is where the
// committed records end. Records, the CLI's journal peek and Runs share it,
// so they agree on what a run holds.
func parseCommitted(data []byte, off int, recs []JournalRecord) (_ []JournalRecord, end int, err error) {
	for off < len(data) {
		nl := bytes.IndexByte(data[off:], '\n')
		if nl < 0 {
			break
		}
		var r JournalRecord
		if err := json.Unmarshal(data[off:off+nl], &r); err != nil {
			if off+nl+1 == len(data) {
				break
			}
			return nil, 0, fmt.Errorf("line %d: %v", len(recs)+1, err)
		}
		if r.Seq != int64(len(recs)+1) {
			return nil, 0, fmt.Errorf("line %d: seq %d", len(recs)+1, r.Seq)
		}
		recs = append(recs, r)
		off += nl + 1
	}
	return recs, off, nil
}

// copyRecords is recs with their mutable parts copied: a caller may change
// what it is handed without changing the cache.
func copyRecords(recs []JournalRecord) []JournalRecord {
	out := slices.Clone(recs)
	for i := range out {
		if out[i].Data != nil {
			out[i].Data = slices.Clone(out[i].Data)
		}
		if out[i].Roots != nil {
			roots := *out[i].Roots
			out[i].Roots = &roots
		}
	}
	return out
}

func cachedRead(path string) *journalCache {
	readCaches.Lock()
	defer readCaches.Unlock()
	return readCaches.m[path]
}

func setCachedRead(path string, c *journalCache) {
	readCaches.Lock()
	defer readCaches.Unlock()
	if c == nil {
		delete(readCaches.m, path)
	} else {
		readCaches.m[path] = c
	}
}

func (j *fileJournal) Runs() ([]RunID, error) {
	entries, err := os.ReadDir(filepath.Join(j.dir, "runs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RunID
	for _, e := range entries { // os.ReadDir sorts by name
		run, err := ParseRunID(e.Name())
		if err == nil && committedFirst(filepath.Join(j.dir, "runs", e.Name(), journalFile)) {
			out = append(out, run)
		}
	}
	return out, nil
}

// committedFirst reports whether the journal at path holds a committed first
// record, as Records reads it: a run whose only line is torn has none, and
// is not listed (Records of it is ErrUnknownRun). A first line that is bad
// but not the last is listed: Records reports the corruption.
func committedFirst(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		return false // no newline: the first record never committed
	}
	// Decoded as Records decodes it: a bad first line that is also the
	// last is a torn write (no run); one with more after it, or a wrong
	// Seq, is corruption, listed so that Records reports it.
	recs, _, err := parseCommitted(line, 0, nil)
	if err != nil || len(recs) == 1 {
		return true
	}
	fi, err := f.Stat()
	return err == nil && int64(len(line)) < fi.Size()
}

// Watch polls the journal file's size and mtime every fileWatchPoll, so it
// sees appends by other processes too. The state at the call is the baseline.
func (j *fileJournal) Watch(run RunID, stop <-chan struct{}) <-chan struct{} {
	ch := make(chan struct{}, 1)
	path := filepath.Join(j.runDir(run), journalFile)
	type stamp struct {
		size int64
		mod  time.Time
	}
	read := func() stamp {
		fi, err := os.Stat(path)
		if err != nil {
			return stamp{size: -1}
		}
		return stamp{fi.Size(), fi.ModTime()}
	}
	last := read()
	go func() {
		t := time.NewTicker(fileWatchPoll)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if now := read(); now != last {
					last = now
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return ch
}

// describe names the holder as a person on this host can use it: the verb
// and its unit or session, the pid (with the host pid when the holder ran in
// another pid namespace and this process can see it), any other host, and
// since when.
func (h LockHolder) describe() string {
	var b strings.Builder
	switch {
	case h.Verb == "":
		b.WriteString("a process")
	case h.Unit != "":
		fmt.Fprintf(&b, "%s for unit %s", h.Verb, h.Unit)
	case h.Session != "":
		fmt.Fprintf(&b, "%s for session %s", h.Verb, h.Session)
	default:
		b.WriteString(h.Verb)
	}
	fmt.Fprintf(&b, ", pid %d", h.Pid)
	switch me, _ := os.Hostname(); {
	case h.Host != "" && h.Host != me:
		// Another host's namespaces and pids mean nothing here.
		fmt.Fprintf(&b, " on host %s", h.Host)
	case h.PidNS != 0 && h.PidNS != pidNamespace("self"):
		if host := hostPid(h.PidNS, h.Pid); host > 0 {
			fmt.Fprintf(&b, " in a sandbox (host pid %d)", host)
		} else {
			b.WriteString(" in a sandbox's pid namespace (its host pid is not visible from here)")
		}
	}
	fmt.Fprintf(&b, ", since %s", h.Since.Format(time.RFC3339))
	if !h.Released.IsZero() {
		fmt.Fprintf(&b, ", released at %s", h.Released.Format(time.RFC3339))
	}
	return b.String()
}

// localPid is h's pid as this process's pid namespace numbers it: its own
// pid when it ran on this host in this namespace (or recorded none), its
// host pid when it ran in a sandbox this process can map, 0 otherwise.
func (h LockHolder) localPid() int {
	if me, _ := os.Hostname(); h.Host != "" && h.Host != me {
		return 0
	}
	if h.PidNS != 0 && h.PidNS != pidNamespace("self") {
		return hostPid(h.PidNS, h.Pid)
	}
	return h.Pid
}

// pidNamespace is the inode of /proc/<pid>/ns/pid, 0 when it cannot be read
// (not Linux, or another user's process).
func pidNamespace(pid string) uint64 {
	link, err := os.Readlink(filepath.Join("/proc", pid, "ns", "pid"))
	if err != nil {
		return 0
	}
	var ino uint64
	if _, err := fmt.Sscanf(link, "pid:[%d]", &ino); err != nil {
		return 0
	}
	return ino
}

// hostPidScan bounds hostPid's walk of /proc: a diagnostic, so it gives up
// rather than slow the call that prints it.
const (
	hostPidMaxEntries = 8192
	hostPidMaxTime    = 100 * time.Millisecond
)

// hostPid finds the process that is pid inside pid namespace ns, as this
// process numbers it: the one process in that namespace whose NSpid ends in
// pid. 0 when none is visible, when more than one matches, or when the
// bounded walk ends first: the caller then shows the namespace-local pid.
func hostPid(ns uint64, pid int) int {
	d, err := os.Open("/proc")
	if err != nil {
		return 0
	}
	defer d.Close()
	names, err := d.Readdirnames(hostPidMaxEntries)
	if err != nil && len(names) == 0 {
		return 0
	}
	stop := time.Now().Add(hostPidMaxTime)
	found, want := 0, strconv.Itoa(pid)
	for _, name := range names {
		if time.Now().After(stop) {
			return 0
		}
		n, err := strconv.Atoi(name)
		if err != nil || pidNamespace(name) != ns {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", name, "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			if f := strings.Fields(line); len(f) > 1 && f[0] == "NSpid:" && f[len(f)-1] == want {
				if found != 0 {
					return 0 // two candidates: uncertain
				}
				found = n
			}
		}
	}
	return found
}

// holderRecordMax bounds an encoded holder record (writeHolder);
// releaseStampMax its release stamp line, which a UTC RFC 3339 time with
// nanoseconds keeps well inside. lockFileMax is what readHolder reads: the
// record, the newline and the stamp, so a stamp never pushes a record at its
// bound out of the read.
const (
	holderRecordMax = 1 << 10
	releaseStampMax = 64
	lockFileMax     = holderRecordMax + 1 + releaseStampMax
)

// halve cuts s to about half its bytes on a rune boundary, marking the cut;
// a string already cut once loses its mark first, so it is not repeated.
func halve(s string) string {
	s = strings.TrimSuffix(s, "…")
	cut := len(s) / 2
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// heldBy names who holds f's flock now: the pid the kernel's own table of
// locks (/proc/locks) shows holding it, and, apart, the record in the file,
// which is its last writer's (the first Append writes it). A pid alone does
// not prove which acquisition holds the lock (one process takes it many
// times), so the record's verb and unit are only ever "last written by",
// never the current hold's. When the table cannot say, the holder is
// unknown.
func heldBy(f *os.File) string {
	rec, ok := readHolder(f)
	pid, known := flockHolder(f)
	switch {
	case known && ok:
		return fmt.Sprintf("pid %d (last written by %s)", pid, rec.describe())
	case known:
		return fmt.Sprintf("pid %d (no call has written to the run yet)", pid)
	case ok:
		return "a process this host cannot name (last written by " + rec.describe() + ")"
	}
	return "a process that recorded no holder"
}

// flockHolder reads the pid holding a flock on f from /proc/locks, as this
// process's pid namespace numbers it; known is false when the table cannot
// be read or does not show one (a holder outside this namespace is shown as
// 0 or not at all).
func flockHolder(f *os.File) (pid int, known bool) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, false
	}
	held, pid, _ := lockTableHolder(st, recordPid(f))
	return pid, held && pid > 0
}

// procLocksPath is the kernel's lock table (a variable for tests).
var procLocksPath = "/proc/locks"

// pidHoldsFlock reports whether process pid holds a flock (not a wait for
// one) on the file with device dev and inode ino, by its own open files: an
// fd of it on that file whose fdinfo lists the flock (want is the table's
// "major:minor:inode" form) under pid itself. An fd shared with the lock's
// owner (a child given its parent's locked fd) lists the flock under the
// owner's pid, which this process's namespace numbers as it numbers pid: a
// shared open file description proves the descriptor, not the ownership,
// so that is no. False when pid's files cannot be read.
func pidHoldsFlock(pid int, dev, ino uint64, want string) bool {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	names, err := readDirNames(fdDir)
	if err != nil {
		return false
	}
	for _, name := range names {
		var st syscall.Stat_t
		if syscall.Stat(filepath.Join(fdDir, name), &st) != nil || uint64(st.Dev) != dev || st.Ino != ino {
			continue
		}
		info, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "fdinfo", name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(info), "\n") {
			// "lock:\t1: FLOCK  ADVISORY  WRITE 1234 103:04:56789 0 EOF"
			f := strings.Fields(strings.TrimPrefix(line, "lock:"))
			if strings.HasPrefix(line, "lock:") && len(f) >= 6 && f[1] == "FLOCK" && f[5] == want && f[4] == strconv.Itoa(pid) {
				return true
			}
		}
	}
	return false
}

// readDirNames lists dir's entry names.
func readDirNames(dir string) ([]string, error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Readdirnames(-1)
}

// recordPid is the pid this host's namespace numbers the lock record's
// writer by, 0 when no record is readable or the writer is another host's
// or another pid namespace's: the candidate lockTableHolder asks first.
func recordPid(f *os.File) int {
	rec, ok := readHolder(f)
	if !ok {
		return 0
	}
	return rec.localPid()
}

// lockTableHolder reads the kernel's lock table for a flock held (not waited
// for) on the file st describes: held when one is listed, pid its holder as
// this pid namespace numbers it (0 when it is outside it), known false when
// the table cannot be read. A table that lists none is not proof the lock is
// free (a holder can be missing from it), only that none is visible.
//
// candidate, when above 0, is a pid this host's namespace numbers that may
// hold it (the lock record's writer): it is asked first, through its own
// file descriptors (pidHoldsFlock). /proc/locks is a seq_file read a page at
// a time, each read its own walk of the system-wide lock list, so on a busy
// host locks removed between two reads shift the walk past entries that
// never moved, and a held lock reads as unlisted. A process's
// fdinfo is read whole in one read.
func lockTableHolder(st syscall.Stat_t, candidate int) (held bool, pid int, known bool) {
	dev := uint64(st.Dev)
	major := (dev>>8)&0xfff | (dev>>32)&^0xfff
	minor := dev&0xff | (dev>>12)&^0xff
	want := fmt.Sprintf("%02x:%02x:%d", major, minor, st.Ino)
	if candidate > 0 && pidHoldsFlock(candidate, dev, st.Ino, want) {
		return true, candidate, true
	}
	table, err := os.ReadFile(procLocksPath)
	if err != nil {
		return false, 0, false
	}
	for _, line := range strings.Split(string(table), "\n") {
		// "1: FLOCK  ADVISORY  WRITE 1234 103:04:56789 0 EOF"; a waiter's
		// line has "->" after the index.
		f := strings.Fields(line)
		if len(f) < 6 || f[1] != "FLOCK" || f[5] != want {
			continue
		}
		held = true
		if n, err := strconv.Atoi(f[4]); err == nil && n > 0 {
			return true, n, true
		}
	}
	return held, 0, true
}

// committedReader is a journal that reads a run's committed records without
// the run's lock: the read verbs (status, explain, journal) never wait on a
// holder, even one that runs them (an effect calling wf status on its own
// run, an earlier revision).
type committedReader interface {
	ReadCommitted(run RunID) ([]JournalRecord, error)
}

// ReadCommitted reads run's committed records with no lock, through the
// process's read cache (parseCached). Each record is
// one line written whole by one append, so the file is always a committed
// prefix plus at most a torn final line, which is skipped and never
// repaired here. A run with no committed record is ErrUnknownRun.
func (j *fileJournal) ReadCommitted(run RunID) ([]JournalRecord, error) {
	if err := validRunID(run); err != nil {
		return nil, err
	}
	path := filepath.Join(j.runDir(run), journalFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRun, run)
	}
	if err != nil {
		return nil, err
	}
	recs, _, err := parseCached(path, data)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRun, run)
	}
	return copyRecords(recs), nil
}

// ReadUnlocked is ReadCommitted for a fold before the lock (preFold),
// which only folds and compares what it reads: the records share their
// payloads with the read cache instead of copying them, so a 217 MB
// journal is not held twice while the verb's own read under the lock is.
// A caller never changes a record's Data or Roots.
func (j *fileJournal) ReadUnlocked(run RunID) ([]JournalRecord, error) {
	if err := validRunID(run); err != nil {
		return nil, err
	}
	path := filepath.Join(j.runDir(run), journalFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	recs, _, err := parseCached(path, data)
	if err != nil {
		return nil, err
	}
	return slices.Clone(recs), nil
}

// recordedHolder reads the holder record in run's lock file with no lock:
// what status shows of the lock when it reads the run lock-free.
func (j *fileJournal) recordedHolder(run RunID) (LockHolder, bool) {
	f, err := os.Open(filepath.Join(j.runDir(run), "lock"))
	if err != nil {
		return LockHolder{}, false
	}
	defer f.Close()
	return readHolder(f)
}

// lockState reads run's lock without taking it (wf lock): the kernel's lock
// table says whether a call holds it now and which pid, by the lock file's
// inode, with no flock of its own; the file holds its last writer's record.
func (j *fileJournal) lockState(run RunID) LockState {
	var st LockState
	path := filepath.Join(j.runDir(run), "lock")
	var fi syscall.Stat_t
	if err := syscall.Stat(path, &fi); err != nil {
		st.Known = errors.Is(err, fs.ErrNotExist) // no lock file: nothing holds it
		return st
	}
	candidate := 0
	if f, err := os.Open(path); err == nil {
		if h, ok := readHolder(f); ok {
			st.Holder = &h
			candidate = h.localPid()
		}
		f.Close()
	}
	st.Held, st.Pid, st.Known = lockTableHolder(fi, candidate)
	return st
}

// heldNow reports whether any call holds run's lock now, as the kernel's
// lock table shows it. It reads the table by the lock file's inode and takes
// no lock, so it never makes a writer's nonblocking acquire fail; the
// holder record in the file outlives its holder and is not read. A table that
// cannot be read is not known.
func (j *fileJournal) heldNow(run RunID) (held, known bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(j.runDir(run), "lock"), &st); err != nil {
		return false, errors.Is(err, fs.ErrNotExist)
	}
	candidate := 0
	if f, err := os.Open(filepath.Join(j.runDir(run), "lock")); err == nil {
		candidate = recordPid(f)
		f.Close()
	}
	held, _, known = lockTableHolder(st, candidate)
	return held, known
}
