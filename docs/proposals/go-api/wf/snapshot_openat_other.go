//go:build !linux

package wf

import (
	"fmt"
	"os"
)

const snapshotFDPath = "/dev/fd"

// Some Unix hosts expose directory descriptors as traversable paths.
// Opening through that descriptor keeps the parent pinned. A host that
// cannot traverse it fails the capture without opening the source by name.
func openSnapshotComponent(parent *os.File, name string, flags int) (*os.File, error) {
	info, err := parent.Stat()
	if err != nil {
		return nil, err
	}
	alias, err := os.Stat(snapshotEntryPath(parent, ""))
	if err != nil || !os.SameFile(info, alias) {
		return nil, fmt.Errorf("%w: snapshot source cannot traverse the descriptor for %s", ErrUnavailable, parent.Name())
	}
	return os.OpenFile(snapshotEntryPath(parent, name), flags, 0)
}
