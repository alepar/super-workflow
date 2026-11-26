package bd_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func snapshotter(t *testing.T) wf.WorldSnapshotter {
	t.Helper()
	s, ok := bd.Runtime(bd.Config{}).(wf.WorldSnapshotter)
	if !ok {
		t.Fatal("bd runtime lacks WorldSnapshotter")
	}
	return s
}

func TestSnapshotWithoutTracker(t *testing.T) {
	s := snapshotter(t)
	req := wf.SnapshotRequest{Workspace: t.TempDir(), Directory: filepath.Join(t.TempDir(), "snapshot")}
	if err := s.SnapshotPreflight(req); err != nil {
		t.Fatal(err)
	}
	meta, err := s.Snapshot(req)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(meta) {
		t.Fatalf("invalid metadata %s", meta)
	}
	dst := t.TempDir()
	if err := s.Restore(wf.RestoreRequest{Workspace: dst, Directory: req.Directory}, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("no-tracker restore created tracker: %v", err)
	}
}

func TestSnapshotRefusesTrackerBeforeWriting(t *testing.T) {
	for _, mode := range []string{"embedded", "server", "shared-server", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			s := snapshotter(t)
			req := wf.SnapshotRequest{Workspace: t.TempDir(), Directory: filepath.Join(t.TempDir(), "snapshot")}
			tracker := filepath.Join(req.Workspace, ".beads")
			if err := os.Mkdir(tracker, 0700); err != nil {
				t.Fatal(err)
			}
			meta := []byte(`{"backend":"dolt","dolt_mode":"` + mode + `"}`)
			path := filepath.Join(tracker, "metadata.json")
			if err := os.WriteFile(path, meta, 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.SnapshotPreflight(req); err == nil {
				t.Fatal("unsupported tracker accepted")
			}
			if _, err := s.Snapshot(req); err == nil {
				t.Fatal("unsupported tracker captured")
			}
			if _, err := os.Stat(req.Directory); !os.IsNotExist(err) {
				t.Fatalf("partial snapshot created: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(meta) {
				t.Fatalf("source metadata changed: %s %v", got, err)
			}
		})
	}
}

func TestSnapshotRefusesRedirectAndInvalidMetadata(t *testing.T) {
	s := snapshotter(t)
	req := wf.SnapshotRequest{Workspace: t.TempDir(), Directory: filepath.Join(t.TempDir(), "snapshot")}
	t.Setenv("BEADS_DIR", t.TempDir())
	if err := s.SnapshotPreflight(req); err == nil || !strings.Contains(err.Error(), "BEADS_DIR") {
		t.Fatalf("redirect accepted: %v", err)
	}
	t.Setenv("BEADS_DIR", "")
	for _, meta := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"version":1,"tracker":"embedded"}`)} {
		if err := s.Restore(wf.RestoreRequest{Workspace: t.TempDir()}, meta); err == nil {
			t.Fatalf("invalid metadata accepted: %s", meta)
		}
	}
}

func TestEmbeddedSnapshotForksAreIsolated(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	s := snapshotter(t)
	original := apply(t, rt, dir, bd.Create(wf.KeyOf("snapshot", "original"), bd.Issue{Title: "original", Type: "task", Priority: 2}))
	cmd := exec.Command("bd", "--dolt-auto-commit", "off", "create", "pending working set", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("create pending issue: %s %v", out, err)
	}
	var pending struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &pending); err != nil {
		t.Fatal(err)
	}
	req := wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "checkpoint")}
	if err := s.SnapshotPreflight(req); err != nil {
		t.Fatal(err)
	}
	before := trackerBytes(t, dir)
	meta, err := s.Snapshot(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, trackerBytes(t, dir)) {
		t.Fatal("snapshot mutated source tracker bytes")
	}
	forks := []string{t.TempDir(), t.TempDir()}
	for _, fork := range forks {
		if err := s.Restore(wf.RestoreRequest{Workspace: fork, Directory: req.Directory}, meta); err != nil {
			t.Fatal(err)
		}
		forkSnapshot := wf.SnapshotRequest{Workspace: fork, Directory: filepath.Join(t.TempDir(), "fork-checkpoint")}
		if err := s.SnapshotPreflight(forkSnapshot); err != nil {
			t.Fatalf("restored tracker preflight: %v", err)
		}
		apply(t, rt, fork, bd.Create(wf.KeyOf("snapshot", "fork-write"), bd.Issue{Title: "first fork write", Type: "task", Priority: 2}))
		if _, err := s.Snapshot(forkSnapshot); err != nil {
			t.Fatalf("snapshot after restored tracker write: %v", err)
		}
		if got := read[bd.Issue](t, rt, fork, bd.Show(pending.ID).Desc()); got.Title != "pending working set" {
			t.Fatalf("lost pending working set: %+v", got)
		}
		got := read[bd.Issue](t, rt, fork, bd.Show(original.ID).Desc())
		if got.Title != "original" {
			t.Fatalf("fork lost issue: %+v", got)
		}
	}
	apply(t, rt, forks[0], bd.UpdateStatus(wf.KeyOf("snapshot", "one"), original.ID, bd.InProgress))
	apply(t, rt, forks[1], bd.Close(wf.KeyOf("snapshot", "two"), original.ID, "done"))
	for i, path := range append([]string{dir}, forks...) {
		got := read[bd.Issue](t, rt, path, bd.Show(original.ID).Desc())
		want := []bd.Status{bd.Open, bd.InProgress, bd.Closed}[i]
		if got.Status != want {
			t.Fatalf("world %d status=%s want=%s", i, got.Status, want)
		}
	}
}

// The native CLI must refuse writes while either of its native gates is held.
// This probes the exact deployed binary, in addition to the pinned-source contract.
func TestEmbeddedSnapshotNativeGateContention(t *testing.T) {
	dir := tempTracker(t)
	cmd := exec.Command("bd", "list", "--json")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open tracker: %s %v", out, err)
	}
	for _, gate := range []string{".beads.gate.lock", ".beads/embeddeddolt.gate.lock"} {
		t.Run(gate, func(t *testing.T) {
			f, err := os.OpenFile(filepath.Join(dir, gate), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bd", "create", "must not exist", "--json")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "maintenance operation") {
				t.Fatalf("bd ignored native gate: %s %v", out, err)
			}
			req := wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "checkpoint")}
			if err := snapshotter(t).SnapshotPreflight(req); !errors.Is(err, wf.ErrUnavailable) {
				t.Fatal("preflight accepted busy tracker")
			}
			if _, err := snapshotter(t).Snapshot(req); err == nil {
				t.Fatal("snapshot copied busy tracker")
			}
			if _, err := os.Stat(req.Directory); !os.IsNotExist(err) {
				t.Fatalf("busy snapshot wrote artifact: %v", err)
			}
		})
	}
}

func trackerBytes(t *testing.T, workspace string) map[string][32]byte {
	t.Helper()
	files := map[string][32]byte{}
	root := filepath.Join(workspace, ".beads")
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
		rel, _ := filepath.Rel(root, path)
		files[rel] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A live capture validates itself by stamping the tracker before and after
// (the workflow run review P1 #1): the stamp moves with a committed write and with a
// working-set write, never with a read or with the capture itself.
func TestSnapshotStampSeesTrackerWrites(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	s, ok := rt.(wf.WorldStamper)
	if !ok {
		t.Fatal("bd runtime lacks WorldStamper")
	}
	req := func() wf.SnapshotRequest {
		return wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "checkpoint")}
	}
	stamp := func() string {
		t.Helper()
		v, err := s.SnapshotStamp(req())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	issue := apply(t, rt, dir, bd.Create(wf.KeyOf("stamp", "one"), bd.Issue{Title: "one", Type: "task", Priority: 2}))
	first := stamp()
	read[bd.Issue](t, rt, dir, bd.Show(issue.ID).Desc())
	if _, err := snapshotter(t).Snapshot(req()); err != nil {
		t.Fatal(err)
	}
	if stamp() != first {
		t.Fatal("a read or the capture moved the stamp")
	}
	apply(t, rt, dir, bd.UpdateStatus(wf.KeyOf("stamp", "two"), issue.ID, bd.InProgress))
	committed := stamp()
	if committed == first {
		t.Fatal("a committed tracker write did not move the stamp")
	}
	cmd := exec.Command("bd", "--dolt-auto-commit", "off", "create", "working set only", "--json")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	if stamp() == committed {
		t.Fatal("a working-set write did not move the stamp")
	}
}

// The gates are held for each observation (stamp, capture, stamp), not
// between them (review round 2, P1 #1): a tracker write between the first
// stamp and the capture, or between the capture and the last stamp, still
// leaves the two stamps different, so the engine records the capture
// unavailable. Only a change undone within the window (A to B to A) passes.
func TestSnapshotStampSeesWritesBetweenObservations(t *testing.T) {
	for _, when := range []string{"before capture", "after capture"} {
		t.Run(when, func(t *testing.T) {
			dir := tempTracker(t)
			rt := bd.Runtime(bd.Config{})
			s := rt.(wf.WorldStamper)
			req := func() wf.SnapshotRequest {
				return wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "checkpoint")}
			}
			issue := apply(t, rt, dir, bd.Create(wf.KeyOf("obs", "one"), bd.Issue{Title: "one", Type: "task", Priority: 2}))
			first, err := s.SnapshotStamp(req())
			if err != nil {
				t.Fatal(err)
			}
			write := func() { apply(t, rt, dir, bd.UpdateStatus(wf.KeyOf("obs", "two"), issue.ID, bd.InProgress)) }
			if when == "before capture" {
				write()
			}
			if _, err := snapshotter(t).Snapshot(req()); err != nil {
				t.Fatal(err)
			}
			if when == "after capture" {
				write()
			}
			last, err := s.SnapshotStamp(req())
			if err != nil {
				t.Fatal(err)
			}
			if first == last {
				t.Fatalf("a write %s left the stamps equal", when)
			}
		})
	}
}

// A restored tracker keeps every file's and directory's mode: tracked hooks
// stay executable, so the restored workspace's git status stays clean.
func TestSnapshotRestoreKeepsTrackerModes(t *testing.T) {
	dir := tempTracker(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "hooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "README.md"), []byte("tracker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Shared-writable files and directories restore without group and world
	// write; exec bits stay.
	if err := os.Mkdir(filepath.Join(dir, ".beads", "shared"), 0o777); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]fs.FileMode{"shared/open": 0o666, "shared/run": 0o777} {
		if err := os.WriteFile(filepath.Join(dir, ".beads", name), []byte("x\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, ".beads", name), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, ".beads", "shared"), 0o777); err != nil {
		t.Fatal(err)
	}
	modes := func(root string) map[string]fs.FileMode {
		t.Helper()
		out := map[string]fs.FileMode{}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || strings.HasSuffix(d.Name(), ".gate.lock") || strings.HasSuffix(d.Name(), ".gate.lock.info") {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			out[rel] = info.Mode()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	s := snapshotter(t)
	req := wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "checkpoint")}
	meta, err := s.Snapshot(req)
	if err != nil {
		t.Fatal(err)
	}
	want := modes(filepath.Join(dir, ".beads"))
	fork := t.TempDir()
	if err := s.Restore(wf.RestoreRequest{Workspace: fork, Directory: req.Directory}, meta); err != nil {
		t.Fatal(err)
	}
	got := modes(filepath.Join(fork, ".beads"))
	for rel, mode := range want {
		if got[rel] != mode&^0o022 {
			t.Errorf("%s restored as %v, source %v", rel, got[rel], mode)
		}
	}
	for rel, mode := range map[string]fs.FileMode{"shared": fs.ModeDir | 0o755, "shared/open": 0o644, "shared/run": 0o755} {
		if got[rel] != mode {
			t.Errorf("%s restored as %v, want %v", rel, got[rel], mode)
		}
	}
}

// A tracker whose config.yaml holds ordinary settings (sync.remote, as the
// code-3 fixture tracks) is snapshotted, and its config.yaml restored as it
// was; one that moves the database is refused, naming the setting.
func TestSnapshotCarriesConfigAndRefusesOnlyStorageSettings(t *testing.T) {
	dir := tempTracker(t)
	s := snapshotter(t)
	config := filepath.Join(dir, ".beads", "config.yaml")
	body := []byte("sync:\n    remote: \"git+https://example.invalid/repo.git\"\nexport:\n  auto: false\n")
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	req := wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "checkpoint")}
	if err := s.SnapshotPreflight(req); err != nil {
		t.Fatalf("ordinary config refused: %v", err)
	}
	meta, err := s.Snapshot(req)
	if err != nil {
		t.Fatal(err)
	}
	fork := t.TempDir()
	if err := s.Restore(wf.RestoreRequest{Workspace: fork, Directory: req.Directory}, meta); err != nil {
		t.Fatal(err)
	}
	// The rest of the config is carried as is; the remote is blanked, since a
	// fork's tracker has none (review round 2).
	if got, err := os.ReadFile(filepath.Join(fork, ".beads", "config.yaml")); err != nil || string(got) != strings.Replace(string(body), `"git+https://example.invalid/repo.git"`, `""`, 1) {
		t.Fatalf("restored config.yaml = %q, %v", got, err)
	}

	// bd resolves the config as YAML: a flow-style mapping moves the
	// database as surely as a block one (bd config review round 1).
	// The check reads the file without bd, and refuses YAML it cannot
	// resolve where a storage or remote setting could be.
	for _, tc := range []struct{ name, yaml, want string }{
		{"comments", "# all defaults\n\n", ""},
		{"dotted sync remote", "sync.remote: /var/runs/p2b\n", ""},
		{"export and lint", "export:\n  auto: true\n  path: issues.jsonl\nlint:\n  task: strict\n", ""},
		{"global off", "global: false\n", ""},
		{"embedded mode", "dolt:\n  mode: embedded\n", ""},
		{"flow mapping of harmless keys", "export: {auto: true}\nlabels: [a, b]\n", ""},
		{"nested dolt host", "dolt:\n  debug: false\n  host: 10.0.0.2\n", "dolt.host"},
		{"flow dolt host", "dolt: {host: 10.0.0.2, port: 3307}\n", "!"},
		{"flow dolt port", "dolt: {port: 3307}\n", "!"},
		{"flow sync", "sync: {remote: x}\n", "!"},
		{"dotted dolt port", "dolt.port: 3307\n", "dolt.port"},
		{"server mode", "dolt:\n  mode: server\n", "dolt.mode"},
		{"quoted keys", "\"dolt\":\n  'user': root\n", "dolt.user"},
		{"socket under a comment", "# server\ndolt:\n  # local socket\n  socket: /run/dolt.sock\n", "dolt.socket"},
	} {
		if err := os.WriteFile(config, []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		moved := wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "moved")}
		err := s.SnapshotPreflight(moved)
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v, want it carried", tc.name, err)
		}
		if tc.want == "!" {
			if err == nil || !strings.Contains(err.Error(), "cannot check") {
				t.Errorf("%s: %v, want a refusal of YAML it cannot check", tc.name, err)
			}
			continue
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), "config.yaml sets "+tc.want+",")) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.want)
		}
	}
}

// A fork's restored tracker has no remote: its Dolt remote and its
// sync.remote are gone, each is reported as an omission, and the source
// keeps both (bd config review round 2).
func TestRestoredTrackerHasNoRemote(t *testing.T) {
	dir := tempTracker(t)
	remote := "file://" + filepath.Join(t.TempDir(), "nowhere")
	cmd := exec.Command("bd", "dolt", "remote", "add", "origin", remote)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd dolt remote add: %v: %s", err, out)
	}
	config := filepath.Join(dir, ".beads", "config.yaml")
	if err := os.WriteFile(config, []byte("sync:\n  remote: \"git+https://example.invalid/r.git\"\nexport:\n  auto: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := snapshotter(t)
	req := wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "checkpoint")}
	meta, err := s.Snapshot(req)
	if err != nil {
		t.Fatal(err)
	}
	fork := t.TempDir()
	var omitted []string
	if err := s.Restore(wf.RestoreRequest{Workspace: fork, Directory: req.Directory, Omit: func(w string) { omitted = append(omitted, w) }}, meta); err != nil {
		t.Fatal(err)
	}
	remotes := func(ws string) string {
		cmd := exec.Command("bd", "dolt", "remote", "list")
		cmd.Dir = ws
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if got := remotes(fork); strings.Contains(got, remote) {
		t.Errorf("the fork's tracker kept its Dolt remote:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(fork, ".beads", "config.yaml")); string(got) != "sync:\n  remote: \"\"\nexport:\n  auto: false\n" {
		t.Errorf("the fork's config.yaml = %q", got)
	}
	if got := strings.Join(omitted, "\n"); !strings.Contains(got, "remote origin ("+remote+")") || !strings.Contains(got, "config.yaml sync.remote") {
		t.Errorf("omissions:\n%s", got)
	}
	if got := remotes(dir); !strings.Contains(got, remote) {
		t.Errorf("the source lost its Dolt remote:\n%s", got)
	}
	if got, _ := os.ReadFile(config); !strings.Contains(string(got), "example.invalid") {
		t.Errorf("the source's config.yaml changed: %q", got)
	}
}

// A capture's preflight starts no bd in the candidate tracker: its version
// probe runs in an empty directory, sandboxed, with PATH and HOME only (bd
// config review round 2).
func TestPreflightStartsNoBdInTheTracker(t *testing.T) {
	dir := tempTracker(t)
	logDir := t.TempDir()
	log := filepath.Join(logDir, "calls")
	fake := filepath.Join(logDir, "bd")
	script := "#!/bin/sh\necho \"$(pwd) $* db=${BEADS_DB:-}\" >> " + log + "\necho 'bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", logDir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	s := bd.Runtime(bd.Config{Bin: fake}).(wf.WorldSnapshotter)
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "c")}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	for _, l := range lines {
		cwd, rest, _ := strings.Cut(l, " ")
		if strings.HasPrefix(cwd, dir) || rest != "--sandbox version db=" {
			t.Errorf("a bd call in preflight: %q", l)
		}
	}
	if len(lines) != 1 {
		t.Errorf("preflight ran bd %d times: %q", len(lines), calls)
	}
}

// A .beads/.env that sets a storage override is refused: bd loads it at
// startup (bd config review round 2).
func TestSnapshotRefusesAStorageOverrideInTheTrackersEnvFile(t *testing.T) {
	dir := tempTracker(t)
	s := snapshotter(t)
	req := wf.SnapshotRequest{Workspace: dir, Directory: filepath.Join(t.TempDir(), "c")}
	for body, want := range map[string]string{"BEADS_DB=/elsewhere/beads.db\n": "BEADS_DB", "export BD_DOLT_HOST: 10.0.0.2\n": "BD_DOLT_HOST", "BD_ACTOR=me\n": ""} {
		if err := os.WriteFile(filepath.Join(dir, ".beads", ".env"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := s.SnapshotPreflight(req)
		if want == "" && err != nil {
			t.Errorf("%q: %v", body, err)
		}
		if want != "" && (err == nil || !strings.Contains(err.Error(), ".beads/.env sets "+want)) {
			t.Errorf("%q: %v, want a refusal naming %s", body, err, want)
		}
	}
}

// Metadata from a newer engine is refused by name, not as invalid.
func TestSnapshotRestoreRefusesNewerMetadata(t *testing.T) {
	s := snapshotter(t)
	err := s.Restore(wf.RestoreRequest{Workspace: t.TempDir()}, json.RawMessage(`{"version":2,"tracker":"embedded"}`))
	if !errors.Is(err, wf.ErrNewerFormat) || !strings.Contains(err.Error(), "format 2; this engine reads up to 1") {
		t.Fatalf("newer metadata: %v", err)
	}
}
