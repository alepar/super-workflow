package wftest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type crashSnapshotAdapter struct{}

func (crashSnapshotAdapter) Ops() []string { return []string{"snapshotworld.read"} }
func (crashSnapshotAdapter) Read(string, wf.ObservationDesc) (json.RawMessage, error) {
	return json.RawMessage(`null`), nil
}
func (crashSnapshotAdapter) Apply(string, wf.ExternalEffect) (json.RawMessage, error) {
	return nil, nil
}
func (crashSnapshotAdapter) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	return wf.EffectDone, nil
}
func (crashSnapshotAdapter) SnapshotPreflight(wf.SnapshotRequest) error { return nil }
func (crashSnapshotAdapter) Snapshot(req wf.SnapshotRequest) (json.RawMessage, error) {
	data, err := os.ReadFile(filepath.Join(req.Workspace, "world"))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), os.WriteFile(filepath.Join(req.Directory, "world"), data, 0600)
}
func (crashSnapshotAdapter) Restore(req wf.RestoreRequest, _ json.RawMessage) error {
	data, err := os.ReadFile(filepath.Join(req.Directory, "world"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(req.Workspace, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(req.Workspace, "world"), data, 0600)
}

func snapshotCrashRun(t *testing.T, crash bool) *drv {
	t.Helper()
	state, workspace := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "world"), []byte("durable historical world"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &drv{t: t, crash: crash, cj: wftest.NewCrashJournal(wf.FileJournal(state)), reg: wf.NewAdapters(), def: crSimpleDef(), now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	if err := h.reg.Register(crashSnapshotAdapter{}); err != nil {
		t.Fatal(err)
	}
	h.opts = wf.EngineOptions{Defs: []*wf.Def{h.def}, RunID: "snapshot-crash-run", Clock: func() time.Time { return h.now }, Workspace: workspace, StateRoot: state, Snapshots: true, RunDir: func(id wf.RunID) string { return filepath.Join(state, "runs", string(id)) }}
	h.e = wf.NewEngine(h.cj, h.reg, h.opts)
	var err error
	h.id, err = h.e.Run(h.def, nil, nil)
	h.must(err)
	return h
}

func snapshotCrashPoint(h *drv) {
	h.at(wf.JournalWorldSnapshot, func() { h.next() })
	// Inspect the original published record immediately after restart, before a
	// later capture could conceal loss of the checkpoint that survived the crash.
	var saved wf.JournalRecord
	for _, record := range h.records() {
		if record.Kind == wf.JournalWorldSnapshot {
			saved = record
			break
		}
	}
	if saved.Seq == 0 {
		h.t.Fatal("world checkpoint was never durably appended")
	}
	var data wf.WorldSnapshotData
	h.must(json.Unmarshal(saved.Data, &data))
	if data.Unavailable != "" || data.Directory == "" || data.Version != 2 || data.Tree == "" {
		h.t.Fatalf("expected a restorable store checkpoint: %+v", data)
	}
	// The checkpoint the durable record names survived the crash whole: a
	// fork from it validates its tree (every object, the meta, and the
	// manifest against this record's adapters), writes it out and restores
	// the historical world.
	h.must(os.WriteFile(filepath.Join(h.opts.Workspace, "world"), []byte("a later world"), 0600))
	fork, err := h.e.(*wf.Engine).ForkSnapshot(h.id, saved.Seq, filepath.Join(h.t.TempDir(), "fork"))
	h.must(err)
	bytes, err := os.ReadFile(filepath.Join(fork.Workspace, "world"))
	h.must(err)
	if string(bytes) != "durable historical world" {
		h.t.Fatalf("historical world lost after restart: %q", bytes)
	}
	h.must(os.WriteFile(filepath.Join(h.opts.Workspace, "world"), []byte("durable historical world"), 0600))
	h.finish(`{"OK":true}`)
}

func init() {
	crashPoints[wf.JournalWorldSnapshot] = scenario(wf.JournalWorldSnapshot, snapshotCrashRun, snapshotCrashPoint)
}
