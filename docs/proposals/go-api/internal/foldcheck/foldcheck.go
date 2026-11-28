// Package foldcheck switches on, for tests, a check that each run state the
// engine builds by folding only a journal's new records onto a fold made
// before the lock equals the fold of the whole journal.
package foldcheck

import "sync/atomic"

var on atomic.Bool

// Enable turns the check on for the rest of the process.
func Enable() { on.Store(true) }

// On reports whether the check is on.
func On() bool { return on.Load() }
