package wftest

import (
	"sync"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// CrashJournal wraps a wf.Journal and kills the process that writes to it: once
// armed for a record kind, the next Append of that kind is written (it is
// durable) and then panics with the crash signal, so the rest of the cycle is
// lost exactly as in a process death right after that write. ArmBefore panics
// without writing, as in a death just before it.
//
// Owns: the armed kind, whether the crash comes before or after the write, and
// the kinds that fired.
// Does not: decide when a crash is useful; a Sim arms it from a CrashAt step.
// Extend by: nothing; a new record kind needs no change here.
type CrashJournal struct {
	wf.Journal
	mu     sync.Mutex
	armed  wf.JournalKind
	before bool
	fired  []wf.JournalKind
}

// NewCrashJournal wraps j. Nothing is armed.
func NewCrashJournal(j wf.Journal) *CrashJournal { return &CrashJournal{Journal: j} }

// Arm makes the next Append of kind panic after it has been written.
func (c *CrashJournal) Arm(kind wf.JournalKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.before = kind, false
}

// ArmBefore makes the next Append of kind panic without writing the record.
func (c *CrashJournal) ArmBefore(kind wf.JournalKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.before = kind, true
}

// Fired lists the kinds whose append crashed the writer, in order.
func (c *CrashJournal) Fired() []wf.JournalKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wf.JournalKind(nil), c.fired...)
}

// Append implements wf.Journal.
func (c *CrashJournal) Append(r wf.JournalRecord) error {
	c.mu.Lock()
	hit := c.armed != 0 && c.armed == r.Kind
	before := hit && c.before
	if hit {
		c.armed, c.before = 0, false
		c.fired = append(c.fired, r.Kind)
	}
	c.mu.Unlock()
	if before {
		panic(crashSignal{adapter: "journal", op: r.Kind.String()})
	}
	if err := c.Journal.Append(r); err != nil {
		return err
	}
	if hit {
		panic(crashSignal{adapter: "journal", op: r.Kind.String()})
	}
	return nil
}

// Survive runs f and reports whether it was cut short by an armed crash. Any
// other panic is re-raised.
func Survive(f func()) (crashed bool) {
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(crashSignal); !ok {
				panic(v)
			}
			crashed = true
		}
	}()
	f()
	return false
}
