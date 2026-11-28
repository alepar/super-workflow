package wf_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	obRead    struct{}
	obGate    struct{}
	obDone    struct{}
	obStopped struct{}
	obFacts   struct {
		Len int
		Sum string
	}
)

// obDef reads the workspace's big.txt into small facts (its length and
// digest), then a gate refuses while *refuse holds.
func obDef(refuse *bool) *wf.Def {
	d := wf.New("observed-blob")
	big := shell.Observe(shell.Run("cat", "big.txt"))
	read := wf.NewCompute[obRead, obFacts](d, func(c wf.Ctx) (obFacts, error) {
		r, err := wf.Observed(c, big)
		sum := sha256.Sum256([]byte(r.Stdout))
		return obFacts{Len: len(r.Stdout), Sum: hex.EncodeToString(sum[:])}, err
	}).Observes(big)
	gate := wf.NewCompute[obGate, bool](d, func(wf.Ctx) (bool, error) { return *refuse, nil })
	done := wf.NewTerminal[obDone](d, wf.Success)
	stop := wf.NewTerminal[obStopped](d, wf.Stopped)
	read.Then(read.To(gate, "read"))
	pass, refused := gate.To(done, "sound"), gate.To(stop, "refused")
	gate.Route(func(bad bool, _ wf.Ctx) wf.Edge[obGate] {
		if bad {
			return refused
		}
		return pass
	}, pass, refused)
	return d
}

type obSetup struct {
	workspace, state string
	reg              *wf.Adapters
	opts             wf.EngineOptions
	e                *wf.Engine
	content          string
}

func newObSetup(t *testing.T, d *wf.Def, size int, snapshots bool) *obSetup {
	t.Helper()
	s := &obSetup{workspace: t.TempDir(), state: t.TempDir(), reg: wf.NewAdapters()}
	if err := os.WriteFile(filepath.Join(s.workspace, "data"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.content = strings.Repeat("line of a large diff\n", size/21+1)[:size]
	if err := os.WriteFile(filepath.Join(s.workspace, "big.txt"), []byte(s.content), 0o600); err != nil {
		t.Fatal(err)
	}
	rts := []wf.AdapterRuntime{shell.Runtime(shell.Config{})}
	if snapshots {
		rts = append(rts, &snapshotRuntime{})
	}
	for _, rt := range rts {
		if err := s.reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	s.opts = obOptions(d, s.workspace, s.state)
	s.opts.Snapshots = snapshots
	s.e = wf.NewEngine(wf.FileJournal(s.state), s.reg, s.opts).(*wf.Engine)
	return s
}

func obOptions(d *wf.Def, workspace, state string) wf.EngineOptions {
	return wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: workspace, StateRoot: state,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
}

// drive runs run until it ends and returns its terminal.
func obDrive(t *testing.T, e *wf.Engine, run wf.RunID) string {
	t.Helper()
	for range 20 {
		ins, err := e.Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(ins) == 1 && ins[0].Kind == wf.InstructionEnded {
			st, err := e.Status(run)
			if err != nil {
				t.Fatal(err)
			}
			return st.Terminal
		}
	}
	t.Fatal("run did not end")
	return ""
}

// obObserved is run's observed records of big.txt.
func obObserved(t *testing.T, state string, run wf.RunID) []wf.ObservedData {
	t.Helper()
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var out []wf.ObservedData
	for _, r := range recs {
		if r.Kind != wf.JournalObserved {
			continue
		}
		var od wf.ObservedData
		if err := json.Unmarshal(r.Data, &od); err != nil {
			t.Fatal(err)
		}
		out = append(out, od)
	}
	return out
}

func obFactsOf(t *testing.T, state string, run wf.RunID) obFacts {
	t.Helper()
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Kind == wf.JournalAccepted && r.State == "obRead" {
			var a wf.AcceptedData
			var f obFacts
			if json.Unmarshal(r.Data, &a) != nil || json.Unmarshal(a.Output, &f) != nil {
				t.Fatalf("obRead acceptance: %s", r.Data)
			}
			return f
		}
	}
	t.Fatal("obRead was not accepted")
	return obFacts{}
}

func (s *obSetup) want() obFacts {
	sum := sha256.Sum256([]byte(s.content))
	return obFacts{Len: len(s.content), Sum: hex.EncodeToString(sum[:])}
}

func journalSize(t *testing.T, state string, run wf.RunID) int64 {
	t.Helper()
	var n int64
	err := filepath.Walk(filepath.Join(state, "runs", string(run)), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && strings.HasPrefix(info.Name(), "journal") {
			n += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A value over ObservedInline is journaled by hash and size; the compute
// reading it sees the whole value, and the journal holds none of it.
func TestObservedBlobKeepsALargeReadOutOfTheJournal(t *testing.T) {
	refuse := false
	d := obDef(&refuse)
	s := newObSetup(t, d, 300<<10, false)
	run, err := s.e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := obDrive(t, s.e, run); got != "obDone" {
		t.Fatalf("ended at %q", got)
	}
	if got := obFactsOf(t, s.state, run); got != s.want() {
		t.Fatalf("the compute read %+v, want %+v", got, s.want())
	}
	reads := obObserved(t, s.state, run)
	if len(reads) == 0 {
		t.Fatal("no observed record")
	}
	for _, od := range reads {
		if od.Blob == nil || len(od.Value) != 0 {
			t.Fatalf("observed record holds its value inline: blob %+v, %d bytes", od.Blob, len(od.Value))
		}
		data, err := os.ReadFile(wf.ObservedBlobPath(s.opts.RunDir(run), od.Blob.SHA256))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != od.Blob.SHA256 || len(data) != od.Blob.Size {
			t.Fatal("blob does not match its name")
		}
		var rr wf.RunResult
		if json.Unmarshal(data, &rr) != nil || rr.Stdout != s.content {
			t.Fatal("blob is not the adapter's value")
		}
	}
	if n := journalSize(t, s.state, run); n > 32<<10 {
		t.Fatalf("journal is %d bytes for a %d-byte read", n, len(s.content))
	}
}

// A value within ObservedInline stays inline, as before.
func TestObservedBlobLeavesASmallReadInline(t *testing.T) {
	refuse := false
	d := obDef(&refuse)
	s := newObSetup(t, d, 1<<10, false)
	run, err := s.e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	obDrive(t, s.e, run)
	for _, od := range obObserved(t, s.state, run) {
		if od.Blob != nil || len(od.Value) == 0 {
			t.Fatalf("small read spilled: %+v", od.Blob)
		}
	}
	if _, err := os.Stat(filepath.Join(s.opts.RunDir(run), wf.ObservedBlobDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a run with no large read has a blob directory: %v", err)
	}
}

// Replay feeds a spilled read from the source run's blob; with the blob gone
// it stops at that read and names the blob.
func TestObservedBlobReplayReadsTheBlobAndNamesAMissingOne(t *testing.T) {
	refuse := true
	d := obDef(&refuse)
	s := newObSetup(t, d, 200<<10, false)
	run, err := s.e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := obDrive(t, s.e, run); got != "obStopped" {
		t.Fatalf("ended at %q", got)
	}
	recs, err := wf.ReadJournal(s.state, run)
	if err != nil {
		t.Fatal(err)
	}
	runDir := s.opts.RunDir(run)
	// The world moved on: replay must not read it.
	if err := os.WriteFile(filepath.Join(s.workspace, "big.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := wf.Replay(recs, d, runDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Differences) != 0 || rep.Stop.Kind != "ended" || rep.Died != "obGate" {
		t.Fatalf("replay with the blob:\n%s", rep)
	}
	sum := obObserved(t, s.state, run)[0].Blob.SHA256
	if err := os.Remove(wf.ObservedBlobPath(runDir, sum)); err != nil {
		t.Fatal(err)
	}
	if recs, err = wf.ReadJournal(s.state, run); err != nil {
		t.Fatal(err)
	}
	rep, err = wf.Replay(recs, d, runDir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stop.Kind != "read" || !strings.Contains(rep.Stop.Then, sum) || !strings.Contains(rep.Stop.Then, "absent") ||
		rep.Verdict != wf.VerdictInconclusive || !strings.Contains(rep.String(), "inconclusive: recorded read") || strings.Contains(rep.String(), "never made") {
		t.Fatalf("replay without the blob: %+v\n%s", rep.Stop, rep)
	}
}

// A rewind's fork carries the blobs its kept prefix names: a replay of the
// fork from its own run dir reads them.
func TestObservedBlobRewindCarriesTheBlobs(t *testing.T) {
	refuse := true
	d := obDef(&refuse)
	s := newObSetup(t, d, 200<<10, false)
	run, err := s.e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	obDrive(t, s.e, run)
	rep, err := s.e.Rewind(run, "died")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Data.State, "obGate") {
		t.Fatalf("rewound to %q, want obGate", rep.Data.State)
	}
	sum := obObserved(t, s.state, run)[0].Blob.SHA256
	if _, err := os.Stat(wf.ObservedBlobPath(s.opts.RunDir(rep.Run), sum)); err != nil {
		t.Fatalf("the fork has no copy of the kept read's blob: %v", err)
	}
	if err := os.RemoveAll(s.opts.RunDir(run)); err != nil {
		t.Fatal(err)
	}
	refuse = false
	if got := obDrive(t, s.e, rep.Run); got != "obDone" {
		t.Fatalf("fork ended at %q", got)
	}
	recs, err := wf.ReadJournal(s.state, rep.Run)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := wf.Replay(recs, d, s.opts.RunDir(rep.Run)); err != nil || r.Stop.Kind != "ended" || len(r.Differences) != 0 {
		t.Fatalf("replay of the fork: %v\n%s", err, r)
	}
}

// A snapshot fork copies the blobs its prefix names into its own state and
// is refused, naming the blob, when the source has lost one.
func TestObservedBlobSnapshotForkCarriesTheBlobs(t *testing.T) {
	refuse := true
	d := obDef(&refuse)
	s := newObSetup(t, d, 200<<10, true)
	run, err := s.e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	obDrive(t, s.e, run)
	recs, err := wf.ReadJournal(s.state, run)
	if err != nil {
		t.Fatal(err)
	}
	var at int64
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot && r.State == "obRead" {
			at = r.Seq // after the read, before the gate decided
		}
	}
	if at == 0 {
		t.Fatal("no checkpoint after obRead")
	}
	sum := obObserved(t, s.state, run)[0].Blob.SHA256
	fork, err := s.e.ForkSnapshot(run, at, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	forkDir := filepath.Join(fork.StateDir, "runs", string(run))
	if _, err := os.Stat(wf.ObservedBlobPath(forkDir, sum)); err != nil {
		t.Fatalf("the fork has no copy of the blob: %v", err)
	}
	forkRecs, err := wf.ReadJournal(fork.StateDir, run)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := wf.Replay(forkRecs, d, forkDir); err != nil || r.Stop.Kind == "read" {
		t.Fatalf("replay of the fork: %v\n%s", err, r)
	}
	refuse = false
	restored := wf.NewEngine(wf.FileJournal(fork.StateDir), s.reg, obOptions(d, fork.Workspace, fork.StateDir)).(*wf.Engine)
	if got := obDrive(t, restored, run); got != "obDone" {
		t.Fatalf("fork ended at %q", got)
	}

	if err := os.Remove(wf.ObservedBlobPath(s.opts.RunDir(run), sum)); err != nil {
		t.Fatal(err)
	}
	_, err = s.e.ForkSnapshot(run, at, filepath.Join(t.TempDir(), "lost"))
	if !errors.Is(err, wf.ErrObservedBlob) || !strings.Contains(err.Error(), sum) {
		t.Fatalf("fork over a lost blob: %v", err)
	}
}

// A large observed prompt input is journaled by hash: a fresh process renders
// the prompt from the blob, and the staleness baseline the blob digests to is
// the live read's, so an unchanged world accepts the submission.
func TestObservedBlobPromptInputRendersFromTheBlob(t *testing.T) {
	rt := newFakeRuntime()
	title := strings.Repeat("t", 100<<10)
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: title, Labels: []string{"bug"}})
	d := observedDef(true, renderTicket)
	state := t.TempDir()
	runDir := func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }
	h := newHarnessOpts(t, d, rt, wf.EngineOptions{RunDir: runDir})
	run := h.run(d, map[string]string{"id": "T-1"})
	ins := h.handOut(run, 1)
	reads := inputReads(t, h, run, "TICKET")
	if len(reads) != 1 {
		t.Fatalf("%d input reads", len(reads))
	}
	var od wf.ObservedData
	if err := json.Unmarshal(reads[0].Data, &od); err != nil || od.Blob == nil || len(od.Value) != 0 {
		t.Fatalf("input read not spilled: %s", reads[0].Data[:min(200, len(reads[0].Data))])
	}
	// The prompt file is rendered again, from the journal, by a fresh process.
	if err := os.RemoveAll(filepath.Join(runDir(run), "prompt")); err != nil {
		t.Fatal(err)
	}
	fresh := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, RunDir: runDir})
	text, err := fresh.Prompt(run, ins[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	file := ""
	for _, line := range strings.Split(text, "\n") {
		if l := strings.TrimSpace(line); strings.HasSuffix(l, "prompt.md") {
			file = l
		}
	}
	body, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(body), "Work on "+title+" [bug].") {
		t.Fatalf("fresh process prompt %q: %v (%d bytes)", file, err, len(body))
	}
	run2 := h.run(d, map[string]string{"id": "T-1"})
	in := h.next(run2)[0]
	if res := h.submit(run2, in, `{}`); res.Class != wf.Accepted {
		t.Fatalf("submit over an unchanged world = %+v, want accepted", res)
	}
}
