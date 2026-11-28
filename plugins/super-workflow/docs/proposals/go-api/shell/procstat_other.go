//go:build !linux

package shell

// detachedSupported is false: the start-ticks identity needs Linux /proc.
const detachedSupported = false

// procStat has no /proc to read.
func procStat(int) (ticks uint64, state byte, ok bool) { return 0, 0, false }
