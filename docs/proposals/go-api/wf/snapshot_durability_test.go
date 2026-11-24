package wf

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotPublicationWaitsForFilesAndAncestorEntries(t *testing.T) {
	root := t.TempDir()
	checkpoint := filepath.Join(root, "new", "deep", "snapshots", "checkpoint")
	if err := os.MkdirAll(checkpoint, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(checkpoint, "world")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, failed := range []string{"", file, filepath.Dir(checkpoint), filepath.Join(root, "new"), root} {
		seen := map[string]bool{}
		published := false
		failure := errors.New("sync failed")
		err := syncSnapshotPublication(checkpoint, root, func(p string) error {
			seen[p] = true
			if p == failed {
				return failure
			}
			return nil
		}, func() error {
			for _, p := range []string{checkpoint, file, filepath.Dir(checkpoint), filepath.Join(root, "new", "deep"), filepath.Join(root, "new"), root} {
				if !seen[p] {
					t.Errorf("published before syncing %s", p)
				}
			}
			published = true
			return nil
		})
		if failed == "" {
			if err != nil || !published {
				t.Fatalf("publication failed: %v", err)
			}
		} else if !errors.Is(err, failure) || published {
			t.Fatalf("published after failed sync of %s: %v", failed, err)
		}
	}
}
