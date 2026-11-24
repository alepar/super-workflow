package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// diedSnapshotRun starts a snapshot run whose workspace holds an installed
// binary the snapshot does not capture (as git leaves an ignored path out),
// and drives it until its state dies.
func diedSnapshotRun(t *testing.T, reg *wf.Adapters) (ws, state string, run wf.RunID) {
	t.Helper()
	ws, state = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".superpowers", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".superpowers", "bin", "super"), []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := snapshotDef()
	opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
	e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	var token string
	for _, i := range ins {
		if i.Kind == wf.InstructionAgent {
			token = i.Token
		}
	}
	if token == "" {
		t.Fatal("no agent token")
	}
	if _, err = e.Submit(run, wf.Submission{Token: token, Output: json.RawMessage(`"done"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	return ws, state, run
}

// A rewound fork of a run whose workspace had the client's installed binary
// finds it after restore: the running binary is installed at the declared
// path, since the snapshot (git) leaves that ignored path out.
func TestRewindIntoInstallsTheRunningBinary(t *testing.T) {
	reg := wf.NewAdapters()
	_ = reg.Register(&snapshotRuntime{})
	ws, state, run := diedSnapshotRun(t, reg)
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: snapshotDef()}}, Binary: "super", InstalledBinary: ".superpowers/bin/super"}
	out := filepath.Join(t.TempDir(), "rewind")
	code, stdout, er := cliCall(cfg, "wf", "dev", "rewind", "--to", "died", "--out", out, "--state-dir", state, "--workspace", ws, "--run-id", string(run))
	if code != 0 {
		t.Fatalf("rewind %d %s %s", code, stdout, er)
	}
	installed := filepath.Join(out, "workspace", ".superpowers", "bin", "super")
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("fork has no installed binary: %v (stderr %s)", err, er)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed binary not executable: %v", info.Mode())
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(exe)
	got, _ := os.ReadFile(installed)
	if !bytes.Equal(got, want) {
		t.Fatal("installed binary is not the running binary")
	}
	if !strings.Contains(er, "installed the invoking build "+exe+" as "+installed) {
		t.Fatalf("restore output does not name the install: %s", er)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".superpowers", "bin", "super")); string(b) != "old binary" {
		t.Fatal("source workspace's binary changed")
	}
}

// layoutRuntime restores the snapshot, then lays out what the restored world
// holds besides (tracked symlinks, a tracked directory or file).
type layoutRuntime struct {
	snapshotRuntime
	layout func(ws string) error
}

func (r *layoutRuntime) Restore(q wf.RestoreRequest, raw json.RawMessage) error {
	if err := r.snapshotRuntime.Restore(q, raw); err != nil {
		return err
	}
	return r.layout(q.Workspace)
}

// rewindWith rewinds a died run whose restored world layout lays out, with
// the client's install path rel; it returns the fork dir, exit code and stderr.
func rewindWith(t *testing.T, rel string, layout func(ws string) error) (string, int, string) {
	t.Helper()
	reg := wf.NewAdapters()
	_ = reg.Register(&layoutRuntime{layout: layout})
	ws, state, run := diedSnapshotRun(t, reg)
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: snapshotDef()}}, Binary: "super", InstalledBinary: rel}
	out := filepath.Join(t.TempDir(), "rewind")
	code, _, er := cliCall(cfg, "wf", "dev", "rewind", "--to", "died", "--out", out, "--state-dir", state, "--workspace", ws, "--run-id", string(run))
	return out, code, er
}

func none(string) error { return nil }

// A restored world that already holds the path as an executable regular file
// keeps it; a client that declares no installed binary gets none.
func TestForkKeepsARestoredInstalledBinary(t *testing.T) {
	out, code, er := rewindWith(t, "tool", func(ws string) error {
		return os.WriteFile(filepath.Join(ws, "tool"), []byte("tracked tool"), 0o755)
	})
	if code != 0 || strings.Contains(er, "installed ") {
		t.Fatalf("rewind %d %s: want the restored tool kept", code, er)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "workspace", "tool")); string(b) != "tracked tool" {
		t.Fatalf("restored tool replaced: %q", b)
	}
	out, code, er = rewindWith(t, "", none)
	if code != 0 {
		t.Fatalf("rewind %d %s", code, er)
	}
	if _, err := os.Stat(filepath.Join(out, "workspace", ".superpowers")); !os.IsNotExist(err) {
		t.Fatalf("undeclared install appeared: %v", err)
	}
}

// The install is confined to the fork's workspace and refuses, naming it,
// a path that is not a clean relative file path, a restored parent symlink
// that leads out of the workspace, a final symlink (dangling or not), a
// directory, or a file that is not executable. Nothing is written outside.
func TestForkInstallRefusesWhatIsNotAnInstallableFile(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "elsewhere"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		rel    string
		layout func(ws string) error
		want   string
	}{
		"unclean path":    {"a/../.superpowers/bin/super", none, "a/../.superpowers/bin/super"},
		"escaping path":   {"../super", none, "../super"},
		"absolute path":   {"/tmp/super", none, "/tmp/super"},
		"escaping parent": {".superpowers/bin/super", func(ws string) error { return os.Symlink(outside, filepath.Join(ws, ".superpowers")) }, ".superpowers/bin/super"},
		"final symlink": {".superpowers/bin/super", func(ws string) error {
			_ = os.MkdirAll(filepath.Join(ws, ".superpowers", "bin"), 0o755)
			return os.Symlink(filepath.Join(outside, "elsewhere"), filepath.Join(ws, ".superpowers", "bin", "super"))
		}, ".superpowers/bin/super"},
		"dangling symlink": {".superpowers/bin/super", func(ws string) error {
			_ = os.MkdirAll(filepath.Join(ws, ".superpowers", "bin"), 0o755)
			return os.Symlink(filepath.Join(outside, "missing"), filepath.Join(ws, ".superpowers", "bin", "super"))
		}, ".superpowers/bin/super"},
		"directory": {".superpowers/bin/super", func(ws string) error {
			return os.MkdirAll(filepath.Join(ws, ".superpowers", "bin", "super"), 0o755)
		}, ".superpowers/bin/super"},
		"not executable": {"data", none, "data"},
	} {
		t.Run(name, func(t *testing.T) {
			_, code, er := rewindWith(t, tc.rel, tc.layout)
			if code == 0 || !strings.Contains(er, tc.want) {
				t.Fatalf("rewind %d %s: want a refusal naming %s", code, er, tc.want)
			}
			if _, err := os.Stat(filepath.Join(outside, "bin")); !os.IsNotExist(err) {
				t.Fatal("the install wrote outside the workspace")
			}
			if b, _ := os.ReadFile(filepath.Join(outside, "elsewhere")); string(b) != "x" {
				t.Fatal("the install replaced a file outside the workspace")
			}
		})
	}
}

// The output says the fork got the invoking build, to resume with --adopt,
// and names a definition the build hashes differently from the run's.
func TestForkInstallSaysItIsTheInvokingBuild(t *testing.T) {
	reg := wf.NewAdapters()
	_ = reg.Register(&snapshotRuntime{})
	ws, state, run := diedSnapshotRun(t, reg)
	changed := snapshotDefPrompt("work, changed")
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: changed}}, Binary: "super", InstalledBinary: ".superpowers/bin/super"}
	out := filepath.Join(t.TempDir(), "rewind")
	code, _, er := cliCall(cfg, "wf", "dev", "rewind", "--to", "died", "--out", out, "--state-dir", state, "--workspace", ws, "--run-id", string(run))
	if code != 0 {
		t.Fatalf("rewind %d %s", code, er)
	}
	for _, want := range []string{"invoking build", "not the run's own binary", "next --adopt", snapshotDef().Hash(), changed.Hash()} {
		if !strings.Contains(er, want) {
			t.Fatalf("output %q does not say %q", er, want)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(out, "workspace", ".superpowers", "bin"))
	if len(entries) != 1 {
		t.Fatalf("install left %v in the bin directory, want only the binary", entries)
	}
}

// A run adopted from one definition onto another and forked after the
// adoption reports the hash it was adopted onto, not the one it started at:
// a build of that definition reports no mismatch, a build of the original
// one does. A fork that installs nothing reports no hashes.
func TestForkInstallHashesTheAdoptedRun(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	_ = reg.Register(&snapshotRuntime{})
	a, b := snapshotDef(), snapshotDefPrompt("work, adopted")
	engine := func(d *wf.Def, adopt bool, install string) *wf.Engine {
		return wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, Adopt: adopt, InstalledBinary: install,
			RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	}
	run, err := engine(a, false, "").Run(a, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine(a, false, "").Next(run, ""); err != nil {
		t.Fatal(err)
	}
	adopted := engine(b, true, "")
	ins, err := adopted.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	var token string
	for _, i := range ins {
		if i.Kind == wf.InstructionAgent {
			token = i.Token
		}
	}
	if token == "" {
		t.Fatalf("no open agent instruction after adoption: %+v", ins)
	}
	if _, err = adopted.Submit(run, wf.Submission{Token: token, Output: json.RawMessage(`"done"`)}); err != nil {
		t.Fatal(err)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var migrated bool
	var cut int64
	for _, r := range recs {
		migrated = migrated || r.Kind == wf.JournalMigrated
		if migrated && r.Kind == wf.JournalWorldSnapshot {
			cut = r.Seq
		}
	}
	if cut == 0 {
		t.Fatal("no checkpoint after the adoption")
	}
	fork, err := engine(b, false, ".superpowers/bin/super").ForkSnapshot(run, cut, filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatal(err)
	}
	if fork.Installed == "" || fork.RunHash != fork.BuildHash || fork.RunHash == a.Hash() {
		t.Fatalf("fork under the adopted build = %+v: want the adopted hash, no mismatch", fork)
	}
	fork, err = engine(a, false, ".superpowers/bin/super").ForkSnapshot(run, cut, filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatal(err)
	}
	if fork.RunHash == fork.BuildHash || fork.BuildHash != a.Hash() {
		t.Fatalf("fork under the original build = %+v: want the mismatch shown", fork)
	}
	fork, err = engine(b, false, "").ForkSnapshot(run, cut, filepath.Join(t.TempDir(), "none"))
	if err != nil || fork.RunHash != "" || fork.BuildHash != "" {
		t.Fatalf("fork with no install = %+v %v: want no hashes", fork, err)
	}
}

// includeRecorder records the files each capture is asked to carry.
type includeRecorder struct {
	snapshotRuntime
	include [][]string
}

func (r *includeRecorder) SnapshotPreflight(q wf.SnapshotRequest) error {
	r.include = append(r.include, q.Include)
	return r.snapshotRuntime.SnapshotPreflight(q)
}

func (r *includeRecorder) Snapshot(q wf.SnapshotRequest) (json.RawMessage, error) {
	r.include = append(r.include, q.Include)
	return r.snapshotRuntime.Snapshot(q)
}

// The client's declared snapshot files (EngineOptions.SnapshotFiles, from
// MainConfig.SnapshotFiles) reach every world capture as its Include, so an
// adapter carries a record git ignores (super install's manifest).
func TestSnapshotFilesReachEveryCapture(t *testing.T) {
	rec := &includeRecorder{}
	reg := wf.NewAdapters()
	_ = reg.Register(rec)
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	d := snapshotDef()
	files := []string{".superpowers/install-manifest.json"}
	opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, SnapshotFiles: files, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
	e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.include) == 0 {
		t.Fatal("no capture was taken")
	}
	for i, got := range rec.include {
		if len(got) != 1 || got[0] != files[0] {
			t.Fatalf("capture call %d was asked to carry %v, want %v", i, got, files)
		}
	}
}
