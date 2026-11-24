package wf_test

import (
	"encoding/json"
	bdadapter "github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type snapshotStart struct{}
type snapshotDone struct{}
type snapshotStop struct{}

func snapshotDef() *wf.Def { return snapshotDefPrompt("work") }

// snapshotDefPrompt is snapshotDef with another prompt: another hash.
func snapshotDefPrompt(prompt string) *wf.Def {
	d := wf.New("snapshot-test")
	a := wf.NewAgent[snapshotStart, string](d)
	done := wf.NewTerminal[snapshotDone](d, wf.Success)
	stop := wf.NewTerminal[snapshotStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText(prompt), wf.Retry[snapshotStart](wf.Const(1), a.To(done, "stop")))
	edge := a.To(done, "done")
	a.Route(func(string, wf.Ctx) wf.Edge[snapshotStart] { return edge }, edge)
	return d
}

type snapshotRuntime struct {
	fail     bool
	captures int
	// moveDuring has each capture change the world's data after reading it,
	// as a live child writing during the capture would.
	moveDuring bool
	// omit is what each restore reports it left out of the fork.
	omit []string
}

// SnapshotStamp is the world's data as it stands.
func (r *snapshotRuntime) SnapshotStamp(q wf.SnapshotRequest) (string, error) {
	b, err := os.ReadFile(filepath.Join(q.Workspace, "data"))
	return string(b), err
}

func (*snapshotRuntime) Ops() []string                                            { return []string{"world.read"} }
func (*snapshotRuntime) Read(string, wf.ObservationDesc) (json.RawMessage, error) { return nil, nil }
func (*snapshotRuntime) Apply(string, wf.ExternalEffect) (json.RawMessage, error) { return nil, nil }
func (*snapshotRuntime) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	return wf.EffectDone, nil
}
func (r *snapshotRuntime) SnapshotPreflight(wf.SnapshotRequest) error {
	if r.fail {
		return wf.ErrUnavailable
	}
	return nil
}
func (r *snapshotRuntime) Snapshot(q wf.SnapshotRequest) (json.RawMessage, error) {
	r.captures++
	b, e := os.ReadFile(filepath.Join(q.Workspace, "data"))
	if e != nil {
		return nil, e
	}
	if r.moveDuring {
		if err := os.WriteFile(filepath.Join(q.Workspace, "data"), append(b, '+'), 0600); err != nil {
			return nil, err
		}
	}
	return json.RawMessage(`{}`), os.WriteFile(filepath.Join(q.Directory, "data"), b, 0600)
}
func (r *snapshotRuntime) Restore(q wf.RestoreRequest, _ json.RawMessage) error {
	for _, w := range r.omit {
		q.Omit(w)
	}
	b, e := os.ReadFile(filepath.Join(q.Directory, "data"))
	if e != nil {
		return e
	}
	if e = os.MkdirAll(q.Workspace, 0700); e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(q.Workspace, "data"), b, 0600)
}

func TestSnapshotForkRestoresHistoricalWorldWithoutSource(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	if e := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); e != nil {
		t.Fatal(e)
	}
	rt := &snapshotRuntime{}
	reg := wf.NewAdapters()
	if e := reg.Register(rt); e != nil {
		t.Fatal(e)
	}
	d := snapshotDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(state, "runs", string(run), "out", "historical")
	if err = os.MkdirAll(filepath.Dir(artifact), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(artifact, []byte("historical"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, r := range records {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
			break
		}
	}
	if seq == 0 {
		t.Fatal("no checkpoint before offer")
	}
	if err = os.WriteFile(artifact, []byte("future"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "new", "deep", "fork")
	report, err := e.ForkSnapshot(run, seq, out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(report.Workspace, "data"))
	if err != nil || string(got) != "before" {
		t.Fatalf("restore=%q %v", got, err)
	}
	saved, err := os.ReadFile(filepath.Join(report.StateDir, "runs", string(run), "out", "historical"))
	if err != nil || string(saved) != "historical" {
		t.Fatalf("imported future artifact: %q %v", saved, err)
	}
	recs, err := wf.ReadJournal(report.StateDir, report.Run)
	if err != nil {
		t.Fatal(err)
	}
	var started wf.RunStartedData
	if err = json.Unmarshal(recs[0].Data, &started); err != nil {
		t.Fatal(err)
	}
	if started.Workspace != report.Workspace {
		t.Fatalf("fork workspace %s", started.Workspace)
	}
	if _, err = os.Stat(ws); !os.IsNotExist(err) {
		t.Fatal("source recreated")
	}
	if _, err = e.ForkSnapshot(run, seq, out); err == nil {
		t.Fatal("overwrote existing fork")
	}
}
func TestSnapshotPreflightDoesNotPublishPartialCheckpoint(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	rt := &snapshotRuntime{fail: true}
	reg := wf.NewAdapters()
	_ = reg.Register(rt)
	d := snapshotDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A refused preflight never refuses next: the checkpoint is journaled
	// unavailable with why.
	if _, err = e.Next(run, ""); err != nil {
		t.Fatalf("next refused on a failed capture: %v", err)
	}
	if rt.captures != 0 {
		t.Fatal("capture before preflight completed")
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var d0 wf.WorldSnapshotData
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot {
			if err := json.Unmarshal(r.Data, &d0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if d0.Unavailable != "capture failed: snapshot world preflight: adapter unavailable" || d0.Directory != "" {
		t.Fatalf("checkpoint: %+v", d0)
	}
}

func TestNativeSnapshotNestedStateForkResumes(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "Snapshot Test"}, {"config", "user.email", "snapshot@example.invalid"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup: %s %v", out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "data"}, {"commit", "-qm", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup: %s %v", out, err)
		}
	}
	state := filepath.Join(ws, ".state")
	reg := wf.NewAdapters()
	_ = reg.Register(gitadapter.Runtime(gitadapter.Config{}))
	_ = reg.Register(bdadapter.Runtime(bdadapter.Config{}))
	d := snapshotDef()
	opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
	engine := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	run, err := engine.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, r := range records {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
			break
		}
	}
	fork, err := engine.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	opts.Workspace = fork.Workspace
	opts.StateRoot = fork.StateDir
	opts.RunDir = func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }
	resumed := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, opts)
	ins, err := resumed.Next(fork.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) == 0 {
		t.Fatal("restored fork did not offer work")
	}
	if _, err := os.Stat(filepath.Join(fork.Workspace, ".state")); !os.IsNotExist(err) {
		t.Fatal("old state directory captured as workspace content")
	}
}

func TestSnapshotCLIFlagPersistsForFreshNext(t *testing.T) {
	strictCallers(t)
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "snapshot-parent")
	t.Setenv("CODEX_THREAD_ID", "snapshot-parent")
	t.Setenv("CODEX_HOME", t.TempDir())
	ws := t.TempDir()
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	_ = reg.Register(&snapshotRuntime{})
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: snapshotDef()}}, Binary: "super"}
	code, out, er := cliCall(cfg, "run", "--snapshots", "--workspace", ws, "--state-dir", state, "--run-id", "snap-cli")
	if code != 0 {
		t.Fatalf("run %d %s %s", code, out, er)
	}
	code, _, er = cliCall(cfg, "wf", "next", "1", "--json", "--state-dir", state)
	if code != 0 {
		t.Fatalf("next %d %s", code, er)
	}
	recs, err := wf.ReadJournal(state, "snap-cli")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range recs {
		found = found || r.Kind == wf.JournalWorldSnapshot
	}
	if !found {
		t.Fatal("fresh next lost snapshot setting")
	}
	code, _, er = cliCall(cfg, "wf", "dev", "rewind", "--to", "died", "--state-dir", state)
	if code == 0 || !strings.Contains(er, "--out") {
		t.Fatalf("legacy rewind allowed snapshot world: %d %s", code, er)
	}
}

func TestSnapshotRewindRestoresBeforeDyingState(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600)
	rt := &snapshotRuntime{}
	reg := wf.NewAdapters()
	_ = reg.Register(rt)
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
	_ = os.WriteFile(filepath.Join(ws, "data"), []byte("later"), 0600)
	fork, err := e.RewindInto(run, "died", filepath.Join(t.TempDir(), "rewind"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(fork.Workspace, "data"))
	if err != nil || string(got) != "before" {
		t.Fatalf("world %q %v", got, err)
	}
	opts.Workspace = fork.Workspace
	opts.StateRoot = fork.StateDir
	opts.RunDir = func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }
	again, err := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, opts).Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) == 0 || again[0].Kind != wf.InstructionAgent {
		t.Fatalf("rewind did not reoffer: %+v", again)
	}
	original, _ := os.ReadFile(filepath.Join(ws, "data"))
	if string(original) != "later" {
		t.Fatal("rewind mutated source")
	}
}

func TestSnapshotChoiceSurvivesSuccessor(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("world"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := snapshotDef()
	opts := wf.EngineOptions{Nonce: func() string { return "stable" }, Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
	e := wf.NewEngine(wf.FileJournal(state), reg, opts)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 {
		t.Fatalf("instructions: %v", ins)
	}
	if _, err = e.Submit(run, wf.Submission{Token: ins[0].Token, Child: ins[0].Child, Output: json.RawMessage(`"done"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	opts.Snapshots = false
	e = wf.NewEngine(wf.FileJournal(state), reg, opts)
	successor, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if successor == run {
		t.Fatal("no successor")
	}
	records, err := wf.ReadJournal(state, successor)
	if err != nil {
		t.Fatal(err)
	}
	var started wf.RunStartedData
	if err = json.Unmarshal(records[0].Data, &started); err != nil {
		t.Fatal(err)
	}
	if !started.Snapshots {
		t.Fatal("successor lost persisted snapshot choice")
	}
}

// A fork is a new run to drive (fanout sweep, bl-codex; an earlier revision): the source
// was started under Codex and stays driven from it, but its fork has no
// harness binding until its first coordinator, here a Hermes one, binds it.
func TestSnapshotForkHasNoHarnessBinding(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	if e := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); e != nil {
		t.Fatal(e)
	}
	rt := &snapshotRuntime{}
	reg := wf.NewAdapters()
	if e := reg.Register(rt); e != nil {
		t.Fatal(e)
	}
	d := snapshotDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, Harness: "codex", HarnessFromEnv: true, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if h, err := wf.Driver(e, run); err != nil || h != "codex" {
		t.Fatalf("source driver = %q, %v; want codex", h, err)
	}
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, r := range records {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
			break
		}
	}
	report, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	var recs []wf.JournalRecord
	fork := wf.NewEngine(wf.FileJournal(report.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: report.Workspace, StateRoot: report.StateDir, RunDir: func(r wf.RunID) string { return filepath.Join(report.StateDir, "runs", string(r)) }}).(*wf.Engine)
	if h, err := wf.Driver(fork, report.Run); err != nil || h != "" {
		t.Fatalf("fork driver = %q, %v; want none until its first coordinator", h, err)
	}
	offered, err := fork.HandOutAs(report.Run, 1, wf.Coordinator{Harness: "hermes", Session: "hermes-coord", Root: t.TempDir()})
	if err != nil || len(offered) != 1 || offered[0].Kind != wf.InstructionAgent {
		t.Fatalf("the fork's first hand-out = %+v, %v", offered, err)
	}
	if h, err := wf.Driver(fork, report.Run); err != nil || h != "hermes" {
		t.Fatalf("fork driver after a Hermes coordinator = %q, %v; want hermes", h, err)
	}
	// A process presenting Codex identity with the unit's valid token is
	// served on the Hermes-bound fork (fanout sweep fork 0017): the token
	// binds the call, and the journal notes the other harness.
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "HERMES_SESSION_ID", "HERMES_DELEGATED_CHILD_CONTEXT"} {
		t.Setenv(k, "")
	}
	t.Setenv("CODEX_SESSION_ID", "codex-sess")
	t.Setenv("CODEX_THREAD_ID", "codex-thread")
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: d}}, Binary: "super"}
	code, _, er := cliCall(cfg, "wf", "prompt", "--claim", claimToken(t, offered[0]), "--run-id", string(report.Run), "--state-dir", report.StateDir)
	if code != 0 {
		t.Fatalf("a Codex caller with the fork's valid token = exit %d %q; want served", code, er)
	}
	recs, err = wf.ReadJournal(report.StateDir, report.Run)
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	for _, r := range recs {
		if r.Kind == wf.JournalDegraded && strings.Contains(string(r.Data), string(wf.DegradedChildCaller)) {
			noted = true
		}
	}
	if !noted {
		t.Fatal("the Codex caller's fetch was not noted as a child caller mismatch")
	}
	if h, err := wf.Driver(e, run); err != nil || h != "codex" {
		t.Fatalf("the source's driver changed: %q, %v", h, err)
	}
}

// What a restore leaves out of a fork on purpose (a tracker's remote) is
// listed on the fork's rewound record, named by its adapter (bd config
// review round 2).
func TestSnapshotForkRecordsWhatItsRestoreOmitted(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	if e := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); e != nil {
		t.Fatal(e)
	}
	rt := &snapshotRuntime{omit: []string{"dolt t: remote origin (https://x)", "config.yaml sync.remote"}}
	reg := wf.NewAdapters()
	if e := reg.Register(rt); e != nil {
		t.Fatal(e)
	}
	d := snapshotDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, r := range records {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
			break
		}
	}
	report, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	forkRecs, err := wf.ReadJournal(report.StateDir, report.Run)
	if err != nil {
		t.Fatal(err)
	}
	var rewound wf.RewoundData
	for _, r := range forkRecs {
		if r.Kind == wf.JournalRewound {
			if err := json.Unmarshal(r.Data, &rewound); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := []string{"world: dolt t: remote origin (https://x)", "world: config.yaml sync.remote"}
	if strings.Join(rewound.Omitted, "|") != strings.Join(want, "|") {
		t.Errorf("rewound Omitted = %q, want %q", rewound.Omitted, want)
	}
}
