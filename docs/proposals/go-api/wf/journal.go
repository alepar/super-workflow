package wf

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors a Journal returns; callers tell them apart with errors.Is.
var (
	// ErrUnknownRun: the run has no records.
	ErrUnknownRun = errors.New("unknown run")
	// ErrCorruptJournal: a line before the last does not parse or breaks the
	// Seq sequence. It is fatal; a torn final line is not this error.
	ErrCorruptJournal = errors.New("corrupt journal")
	// ErrJournalNotLocked: Append or Records without the run's Lock.
	ErrJournalNotLocked = errors.New("journal: run lock not held")
	// ErrRunLocked: another process holds the run's lock past the bounded
	// retry. The error names the holder.
	ErrRunLocked = errors.New("run locked")
)

// LockHolder is who holds (or last held) a run's file lock: the process and
// when it took the lock, and, when the CLI took it, the verb and the unit or
// session it ran for, the host, and the process's pid namespace (a sandboxed
// harness, such as Codex's, runs the verb as a low pid in a namespace of its
// own, which says nothing on the host).
type LockHolder struct {
	Pid     int
	Since   time.Time
	Verb    string `json:",omitempty"`
	Unit    string `json:",omitempty"`
	Session string `json:",omitempty"`
	Host    string `json:",omitempty"`
	PidNS   uint64 `json:",omitempty"`
	// Released is when the holder let the lock go, marked by its own release;
	// zero while it holds it, and after a holder that died (the kernel let
	// the flock go and nothing marked it).
	Released time.Time `json:",omitzero"`
}

// LockState is a run's lock as read without taking it (wf lock): Held when a
// process holds the flock now, Pid that process as /proc/locks names it (0
// when it cannot), and Holder the record its last writer left in the lock
// file, which says when that writer released it, if it did.
type LockState struct {
	Held bool
	// Known is false when the kernel's lock table cannot be read. Known and
	// not Held is no visible holder, never proof that the lock is free: a
	// hold the table does not list is still a hold.
	Known  bool
	Pid    int         `json:",omitempty"`
	Holder *LockHolder `json:",omitempty"`
}

// Journal is the append-only record of each run, plus the per-run lock.
//
// Owns: record storage, Seq assignment (strictly increasing from 1 per run,
// under the lock) and an exclusive per-run lock that spans the Engine's whole
// read-fold-append cycle, across processes where the backend has them. Holding
// it from before the read until after the last append is what makes the fold
// current and the appends consistent with it.
// Does not: interpret Data (opaque JSON here), fold records, or decide what a
// record means.
// Extend by: a new backend is one implementation of this interface, chosen by
// the constructor its caller uses.
type Journal interface {
	// Lock takes the run's exclusive lock and returns its release. The file
	// backend retries for a bounded time, then fails with ErrRunLocked naming
	// the holder; the memory backend blocks until the lock is free. It is not reentrant. A run with no records yet can
	// be locked: the first locked writer creates the run.
	Lock(run RunID) (unlock func(), err error)
	// Append adds a record to r.Run; Seq is assigned by the journal and the
	// caller's value is ignored. It needs the run's lock.
	Append(r JournalRecord) error
	// Records returns the run's records in sequence order, dropping a torn
	// final write (the next Append cuts it off; a read writes nothing). It
	// needs the run's lock. A run with no records is ErrUnknownRun.
	Records(run RunID) ([]JournalRecord, error)
	// Runs lists the runs that have records, sorted. It needs no lock.
	Runs() ([]RunID, error)
	// Watch returns a channel that receives (without blocking the writer) when
	// the run's journal changes, until stop is closed. FileJournal polls the
	// file's size and mtime, so it also sees another process's appends;
	// MemJournal signals on Append. A signal pending when the caller is busy
	// is kept (one at most). It needs no lock.
	Watch(run RunID, stop <-chan struct{}) <-chan struct{}
}

// validRunID rejects an id that is not one path component: a run id names a
// directory of the file journal, and both backends apply one rule.
func validRunID(run RunID) error {
	s := string(run)
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("invalid run id %q", s)
	}
	return nil
}
