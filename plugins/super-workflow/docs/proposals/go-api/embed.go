// Package goapi is the root of the Go API proposal module.
// The executable CLI prints coordinator instructions when a run starts.
package goapi

// CoordinatorPrompt remains for callers compiled against the old prompt template.
//
// Deprecated: capture the bootstrap printed to stderr by a successful run
// command. It contains the actual executable, run ID, and state directory.
const CoordinatorPrompt = "Use the coordinator bootstrap printed by the successful run command on stderr. Execute its loop command exactly as printed; there is no prompt template to fill."
