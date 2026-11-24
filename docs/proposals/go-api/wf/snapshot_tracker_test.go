package wf_test

import (
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	bdadapter "github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// This exercises adapter ordering and ownership through the engine: Git retains
// the tracked tracker index entries, while bd restores the actual tracker files.
func TestNativeEngineSnapshotRestoresTrackedEmbeddedTracker(t *testing.T) {
	for _, bin := range []string{"git", "bd"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s unavailable", bin)
		}
	}
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	command := func(dir, bin string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
		}
		return out
	}
	command(ws, "git", "init", "-q", "-b", "main")
	command(ws, "git", "config", "user.name", "Snapshot Test")
	command(ws, "git", "config", "user.email", "snapshot@example.invalid")
	command(ws, "bd", "init", "--quiet", "--prefix", "snap")
	command(ws, "git", "add", "-f", ".beads/config.yaml", ".beads/metadata.json")
	command(ws, "git", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-qm", "track tracker configuration")
	raw := command(ws, "bd", "--dolt-auto-commit", "off", "create", "pending before checkpoint", "--json")
	var issue struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &issue); err != nil || issue.ID == "" {
		t.Fatalf("create issue: %s %v", raw, err)
	}
	trackerBytes := func(dir string) map[string][32]byte {
		t.Helper()
		root := filepath.Join(dir, ".beads")
		result := map[string][32]byte{}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			result[rel] = sha256.Sum256(data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := trackerBytes(ws)
	beforeIndex := string(command(ws, "git", "ls-files", "--stage"))
	beforeHead := string(command(ws, "git", "rev-parse", "HEAD"))
	state := t.TempDir()
	reg := wf.NewAdapters()
	for _, adapter := range []wf.AdapterRuntime{gitadapter.Runtime(gitadapter.Config{}), bdadapter.Runtime(bdadapter.Config{})} {
		if err := reg.Register(adapter); err != nil {
			t.Fatal(err)
		}
	}
	def := snapshotDef()
	engine := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
	}).(*wf.Engine)
	run, err := engine.Run(def, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, record := range records {
		if record.Kind == wf.JournalWorldSnapshot {
			seq = record.Seq
			break
		}
	}
	if seq == 0 {
		t.Fatal("no engine world checkpoint")
	}
	fork, err := engine.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, trackerBytes(ws)) {
		t.Fatal("engine capture/fork changed source tracker bytes")
	}
	if got := string(command(ws, "git", "ls-files", "--stage")); got != beforeIndex {
		t.Fatal("engine changed source Git index")
	}
	if got := string(command(ws, "git", "rev-parse", "HEAD")); got != beforeHead {
		t.Fatal("engine changed source HEAD")
	}
	if got := string(command(fork.Workspace, "git", "ls-files", "--stage")); got != beforeIndex {
		t.Fatalf("fork lost original tracked tracker index entries:\n%s", got)
	}
	show := func(dir string) string {
		t.Helper()
		raw := command(dir, "bd", "show", issue.ID, "--json")
		var issues []struct {
			Title  string `json:"title"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &issues); err != nil || len(issues) != 1 {
			t.Fatalf("show issue: %s %v", raw, err)
		}
		if issues[0].Title != "pending before checkpoint" {
			t.Fatalf("fork lost pending working set: %s", raw)
		}
		return issues[0].Status
	}
	if got := show(fork.Workspace); got != "open" {
		t.Fatalf("restored issue status %s", got)
	}
	command(fork.Workspace, "bd", "update", issue.ID, "--status", "in_progress", "--json")
	if got := show(fork.Workspace); got != "in_progress" {
		t.Fatalf("fork mutation missing: %s", got)
	}
	if got := show(ws); got != "open" {
		t.Fatalf("fork mutation escaped into source: %s", got)
	}
	// Source mutation must likewise stay out of the restored world.
	command(ws, "bd", "close", issue.ID, "--reason", "source only", "--json")
	if got := show(fork.Workspace); got != "in_progress" {
		t.Fatalf("source mutation escaped into fork: %s", got)
	}
	if raw := command(fork.Workspace, "git", "status", "--porcelain", "--", ".beads/config.yaml", ".beads/metadata.json"); strings.TrimSpace(string(raw)) != "" {
		t.Fatalf("restored tracked tracker configuration differs: %s", raw)
	}
}
