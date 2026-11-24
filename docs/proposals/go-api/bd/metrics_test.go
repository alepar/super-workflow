package bd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The tests' own bd calls (tempTracker's bd init, a test's direct bd) run
// with metrics off, as the adapter's do: bd's metrics flush writes under
// HOME, which is a test TempDir, and a flush still writing as the test
// returns fails the TempDir cleanup (an earlier revision: "directory not empty").
func TestTrackerSetupWritesNoMetrics(t *testing.T) {
	dir := tempTracker(t)
	s := snapshotter(t)
	_ = s.SnapshotPreflight(wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "c")})
	events := filepath.Join(os.Getenv("HOME"), ".beads", "eventsData")
	if entries, err := os.ReadDir(events); err == nil && len(entries) > 0 {
		t.Fatalf("bd wrote %d metrics files under the test's HOME (%s)", len(entries), events)
	}
}

// BD_DISABLE_METRICS in the environment, or in the tracker's .env, is no
// storage override: a snapshot is not refused for it.
func TestSnapshotAcceptsMetricsOff(t *testing.T) {
	dir := tempTracker(t)
	s := snapshotter(t)
	req := wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "c")}
	if os.Getenv("BD_DISABLE_METRICS") != "1" {
		t.Fatal("the package's bd runs with metrics on")
	}
	if err := s.SnapshotPreflight(req); err != nil {
		t.Fatalf("snapshot with BD_DISABLE_METRICS=1 in the environment: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", ".env"), []byte("BD_DISABLE_METRICS=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotPreflight(req); err != nil {
		t.Fatalf("snapshot with BD_DISABLE_METRICS=1 in .beads/.env: %v", err)
	}
}
