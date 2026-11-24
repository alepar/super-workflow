package wf_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The transcripts here are copies of harness/testdata/ended fixtures,
// which are built from the documented pi 1.5.2 format, not recorded. The
// engine reads their content; only their mtime is injected.

const settle = 15 * time.Second // endSettle

// liveMarker is an injected ChildLive: it records each call's child id and
// answers what the test set.
type liveMarker struct {
	mu    sync.Mutex
	live  bool
	err   error
	calls []string
}

func (m *liveMarker) set(live bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live, m.err = live, err
}

func (m *liveMarker) ask(_, child string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, child)
	return m.live, m.err
}

func (m *liveMarker) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func fixtureCopy(t *testing.T, fixture string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "ended", fixture))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "wf-kid.jsonl")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// settleRun hands out the one unit to a pi coordinator and claims it with a
// pi transcript, with the engine on clock.
type settleRun struct {
	e    *wf.Engine
	j    wf.Journal
	run  wf.RunID
	unit wf.Instruction
	co   wf.Coordinator
	live *liveMarker
	mt   *transcripts
	path string
	t    *testing.T
}

func newSettleRun(t *testing.T, harnessName, fixture string, clock func() time.Time, mt time.Time) *settleRun {
	t.Helper()
	s := &settleRun{t: t, live: &liveMarker{}, mt: newTranscripts(), path: fixtureCopy(t, fixture)}
	d := clAgentDef("end-settle")
	opts := lsOpts()
	opts.Clock = clock
	opts.TranscriptMtime = s.mt.mtime
	opts.ChildLive = s.live.ask
	opts.Defs = []*wf.Def{d}
	s.j = wf.MemJournal()
	s.e = wf.NewEngine(s.j, wf.NewAdapters(), opts).(*wf.Engine)
	run, err := s.e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.run, s.co = run, wf.Coordinator{Harness: harnessName, Session: "coord"}
	ins, err := s.e.HandOutAs(run, 1, s.co)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOutAs = %+v, %v", ins, err)
	}
	s.unit = ins[0]
	s.mt.set(s.path, mt)
	if err := s.e.Started(run, s.unit.Name, wf.Claim{Harness: harnessName, Session: "coord", Agent: "wf-kid", Transcript: s.path}); err != nil {
		t.Fatal(err)
	}
	return s
}

// pass drives once and returns the recovery offers it carries.
func (s *settleRun) pass() []wf.Instruction {
	s.t.Helper()
	ins, err := s.e.HandOutAs(s.run, 1, s.co)
	if err != nil {
		s.t.Fatal(err)
	}
	var out []wf.Instruction
	for _, in := range ins {
		if in.Kind == wf.InstructionRecovery {
			out = append(out, in)
		}
	}
	return out
}

func (s *settleRun) endSettleDue() (time.Time, bool) {
	s.t.Helper()
	st, err := s.e.Status(s.run)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, d := range st.Due {
		if d.Kind == "end settle" {
			return d.Due, true
		}
	}
	return time.Time{}, false
}

func (s *settleRun) records() []wf.JournalRecord {
	s.t.Helper()
	unlock, err := s.j.Lock(s.run)
	if err != nil {
		s.t.Fatal(err)
	}
	defer unlock()
	recs, err := s.j.Records(s.run)
	if err != nil {
		s.t.Fatal(err)
	}
	return recs
}

func (s *settleRun) journaled() int { return len(s.records()) }

func TestEndSettleWakesABlockingNext(t *testing.T) {
	mt := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	var started time.Time
	var mu sync.Mutex
	base := mt.Add(settle - 50*time.Millisecond)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if started.IsZero() {
			return base
		}
		return base.Add(time.Since(started))
	}
	s := newSettleRun(t, "pi", "pi-final-error.jsonl", clock, mt)
	mu.Lock()
	started = time.Now()
	mu.Unlock()
	before := s.journaled()
	res, err := s.e.NextWait(s.run, 1, s.co, 5*time.Second)
	if err != nil || res.Timeout {
		t.Fatalf("NextWait = %+v, %v", res, err)
	}
	var rec *wf.Instruction
	for i := range res.Instructions {
		if res.Instructions[i].Kind == wf.InstructionRecovery {
			rec = &res.Instructions[i]
		}
	}
	if rec == nil || rec.Name != s.unit.Name {
		t.Fatalf("next = %+v, want the recovery offer of %s", res.Instructions, s.unit.Name)
	}
	if got := rec.Recovery.LastMessage; got != "error: No API key found for openrouter." {
		t.Errorf("recovery last message = %q, want the error named", got)
	}
	if calls := s.live.asked(); len(calls) != 1 || calls[0] != "wf-kid" {
		t.Errorf("ChildLive asked %v, want once with wf-kid", calls)
	}
	// The only journal growth is the recovery offer itself: the settle wake
	// journaled nothing before it.
	if got := s.journaled() - before; got != 1 {
		t.Errorf("%d records journaled during the wait, want only recovery_offered", got)
	}
}

func TestEndSettleWaitsForTheLiveMarker(t *testing.T) {
	mt := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	now := mt.Add(time.Second)
	s := newSettleRun(t, "pi", "pi-final-error.jsonl", func() time.Time { return now }, mt)

	if got := s.pass(); len(got) != 0 {
		t.Fatalf("offered before the settle: %+v", got)
	}
	if due, ok := s.endSettleDue(); !ok || !due.Equal(mt.Add(settle)) {
		t.Fatalf("end settle due = %v, %v; want %v", due, ok, mt.Add(settle))
	}
	if len(s.live.asked()) != 0 {
		t.Fatalf("ChildLive asked before the settle was due: %v", s.live.asked())
	}

	now = mt.Add(settle)
	s.live.set(true, nil)
	if got := s.pass(); len(got) != 0 {
		t.Fatalf("offered with a live marker: %+v", got)
	}
	if due, ok := s.endSettleDue(); !ok || !due.Equal(mt.Add(2*settle)) {
		t.Fatalf("a live marker re-arms: due = %v, %v; want %v", due, ok, mt.Add(2*settle))
	}

	now = mt.Add(2 * settle)
	s.live.set(false, errors.New("cannot list /proc"))
	if got := s.pass(); len(got) != 0 {
		t.Fatalf("offered after a failed scan: %+v", got)
	}
	if due, ok := s.endSettleDue(); !ok || !due.Equal(mt.Add(3*settle)) {
		t.Fatalf("a failed scan re-arms: due = %v, %v; want %v", due, ok, mt.Add(3*settle))
	}

	now = mt.Add(3 * settle)
	s.live.set(false, nil)
	got := s.pass()
	if len(got) != 1 || got[0].Name != s.unit.Name {
		t.Fatalf("no marker live at the due time: %+v, want the recovery offer", got)
	}
	if _, ok := s.endSettleDue(); ok {
		t.Error("an ended child keeps an end settle deadline")
	}
	if again := s.pass(); len(again) != 1 {
		t.Errorf("the offer is journaled once and re-offered, not raised again: %+v", again)
	}
	n := 0
	for _, r := range s.records() {
		if r.Kind == wf.JournalRecoveryOffered {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d recovery_offered records, want 1", n)
	}
}

func TestEndSettleCancelledByAWrite(t *testing.T) {
	mt := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	now := mt.Add(time.Second)
	s := newSettleRun(t, "pi", "pi-final-error.jsonl", func() time.Time { return now }, mt)
	if got := s.pass(); len(got) != 0 {
		t.Fatalf("offered before the settle: %+v", got)
	}
	s.mt.set(s.path, mt.Add(5*time.Second)) // the child wrote again before the due time
	now = mt.Add(settle)
	if got := s.pass(); len(got) != 0 {
		t.Fatalf("offered although the transcript was written inside the settle: %+v", got)
	}
	if due, ok := s.endSettleDue(); !ok || !due.Equal(mt.Add(5*time.Second+settle)) {
		t.Fatalf("the settle re-arms from the new mtime: due = %v, %v; want %v", due, ok, mt.Add(5*time.Second+settle))
	}
	if calls := s.live.asked(); len(calls) != 0 {
		t.Errorf("ChildLive asked %v before the re-armed settle", calls)
	}
}

func TestEndSettleNotForOtherHarnesses(t *testing.T) {
	// A claude transcript that ended with a hand-back is offered on the first
	// pass, as before: claude does not implement SettlingEnd.
	mt := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	now := mt.Add(time.Second)
	s := newSettleRun(t, "claude", "claude-handback.jsonl", func() time.Time { return now }, mt)
	if got := s.pass(); len(got) != 1 || got[0].Name != s.unit.Name {
		t.Fatalf("a claude hand-back must be offered on the first pass: %+v", got)
	}
	if _, ok := s.endSettleDue(); ok {
		t.Error("claude has no end settle")
	}
	if calls := s.live.asked(); len(calls) != 0 {
		t.Errorf("ChildLive asked for a harness without SettlingEnd: %v", calls)
	}
}
