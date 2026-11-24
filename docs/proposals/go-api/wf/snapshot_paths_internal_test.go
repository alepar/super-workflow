package wf

import (
	"errors"
	"fmt"
	"testing"
)

// Only an untyped source path refuses a fork alone; a corrupt journal is
// reported as it is.
func TestForkRefusalClassifiesOnlyUntypedSourcePaths(t *testing.T) {
	untyped := fmt.Errorf("journal 3 accepted: %w", &untypedSourcePathError{"Output records untyped paths"})
	if err := forkRefusal(untyped); !errors.Is(err, errForkUnrelocatable) || !errors.Is(err, ErrUnavailable) {
		t.Errorf("untyped source path: %v", err)
	}
	corrupt := fmt.Errorf("%w: Output path param marker %q has no recorded ownership", ErrCorruptJournal, "p")
	if err := forkRefusal(corrupt); errors.Is(err, errForkUnrelocatable) || !errors.Is(err, ErrCorruptJournal) {
		t.Errorf("corrupt journal: %v", err)
	}
}
