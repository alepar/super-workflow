package wf_test

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func recordKinds(h *harness, run wf.RunID) []wf.JournalKind {
	var out []wf.JournalKind
	for _, r := range h.records(run) {
		out = append(out, r.Kind)
	}
	return out
}

func TestWaveNextClaimsUnderSessionWave(t *testing.T) {
	d := lsDef("al-a", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run := h.run(d, nil)
	h.handOut(run, 0) // drive to the open instruction, so Next journals only its own records
	before := len(h.records(run))

	ins := h.next(run)
	if len(ins) != 1 || ins[0].Token == "" || ins[0].Prompt == "" || ins[0].Name == "" {
		t.Fatalf("Next = %+v, want one agent instruction with token, prompt and name", ins)
	}
	got := recordKinds(h, run)[before:]
	want := []wf.JournalKind{wf.JournalHandedOut, wf.JournalAttemptStarted, wf.JournalPromptFetched}
	if !slices.Equal(got, want) {
		t.Fatalf("records written by Next = %v, want handed_out, attempt_started, prompt_fetched", got)
	}
	started := attemptStarted(t, h, run)
	if len(started) != 1 || started[0].Name != ins[0].Name || started[0].Session != "wave" || started[0].Transcript != "" {
		t.Fatalf("attempt_started = %+v, want the unit %s claimed under session wave with no transcript", started, ins[0].Name)
	}
}

func TestWaveUnitsAreOrphansOfWave(t *testing.T) {
	d := lsDef("al-b", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("Next = %+v, want one unit", ins)
	}

	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Session: "sess-other"}, false); err != nil {
		t.Fatal(err)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("died --session sess-other lost %d units, want 0", n)
	}
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Session: "wave"}, false); err != nil {
		t.Fatal(err)
	}
	lost := lostRecords(t, h, run)
	if len(lost) != 1 || lost[0].Name != ins[0].Name {
		t.Fatalf("lost = %+v, want the wave unit %s", lost, ins[0].Name)
	}
	res, err := h.e.Submit(run, wf.Submission{Token: ins[0].Token, Output: []byte(`{"Answer":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != wf.Rejected {
		t.Fatalf("Submit with the lost unit's token = %+v, want Rejected", res)
	}
	if again := h.next(run); len(again) != 1 || again[0].Token == ins[0].Token || again[0].Name == ins[0].Name {
		t.Fatalf("Next after the loss = %+v, want a fresh unit under a new token", again)
	}
}

func TestWaveUnitNeverStalls(t *testing.T) {
	d := lsDef("al-c", true)
	var stats atomic.Int32
	opts := lsOpts()
	opts.StallDefault = time.Minute
	opts.TranscriptMtime = func(string) (time.Time, error) { stats.Add(1); return time.Unix(1, 0), nil }
	h := newHarnessOpts(t, d, nil, opts)
	run := h.run(d, nil)
	ins := h.next(run)

	h.now = h.now.Add(10 * time.Hour)
	if again := h.next(run); len(again) != 1 || again[0].Token != ins[0].Token {
		t.Fatalf("Next 10h later = %+v, want the same unit still running", again)
	}
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 0 {
		t.Fatalf("a wave unit stalled %d times, want 0", n)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("a wave unit was lost %d times, want 0", n)
	}
	if n := stats.Load(); n != 0 {
		t.Fatalf("TranscriptMtime called %d times for a unit with no transcript, want 0", n)
	}
}

func TestWaveNextIdempotent(t *testing.T) {
	d := lsDef("al-d", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run := h.run(d, nil)
	first := h.next(run)
	n := len(h.records(run))
	second := h.next(run)
	if len(h.records(run)) != n {
		t.Fatalf("the second Next journaled %d records, want 0", len(h.records(run))-n)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Token != second[0].Token || first[0].Name != second[0].Name {
		t.Fatalf("Next twice = %+v then %+v, want the same unit and token", first, second)
	}
}
