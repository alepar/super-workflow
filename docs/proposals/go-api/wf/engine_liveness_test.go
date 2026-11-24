package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// transcripts is a fake TranscriptMtime over a map the test edits.
type transcripts struct {
	mu   sync.Mutex
	at   map[string]time.Time
	errs map[string]error
}

func newTranscripts() *transcripts {
	return &transcripts{at: map[string]time.Time{}, errs: map[string]error{}}
}

func (m *transcripts) set(path string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at[path] = at
}

func (m *transcripts) mtime(path string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errs[path]; err != nil {
		return time.Time{}, err
	}
	if at, ok := m.at[path]; ok {
		return at, nil
	}
	return time.Time{}, fs.ErrNotExist
}

func lvOpts(m *transcripts, stallDefault time.Duration) wf.EngineOptions {
	o := lsOpts()
	o.StallDefault = stallDefault
	o.TranscriptMtime = m.mtime
	return o
}

// lvRunning hands out the one unit, claims it as agent "a" (transcript
// /t/a.jsonl, written at the start) and fetches its prompt.
func lvRunning(t *testing.T, h *harness, m *transcripts, run wf.RunID, ins wf.Instruction) {
	t.Helper()
	m.set("/t/a.jsonl", h.now)
	if err := h.e.Started(run, ins.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
}

func heartbeats(t *testing.T, h *harness, run wf.RunID) []wf.HeartbeatData {
	t.Helper()
	var out []wf.HeartbeatData
	for _, r := range h.kind(run, wf.JournalHeartbeat) {
		var d wf.HeartbeatData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// askedThenDied checks that a passed lease asked the coordinator about name
// (one stalled heartbeat, no loss), answers heartbeat died and returns the
// losses after the answer.
func askedThenDied(t *testing.T, h *harness, run wf.RunID, name string) []wf.AttemptLostData {
	t.Helper()
	if lost := lostRecords(t, h, run); len(lost) != 0 {
		t.Fatalf("lease expiry lost %+v, want the coordinator asked", lost)
	}
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats = %d, want the one question: %+v", n, heartbeats(t, h, run))
	}
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: name}, false); err != nil {
		t.Fatalf("heartbeat died %s: %v", name, err)
	}
	return lostRecords(t, h, run)
}

func countStatus(hbs []wf.HeartbeatData, status string) int {
	n := 0
	for _, hb := range hbs {
		if hb.Status == status {
			n++
		}
	}
	return n
}

// A prompt fetch is the spawn signal: a fetched unit has no spawn deadline,
// however the claim was made. Without a Map lease or a transcript it lives by
// the implicit lease, which a coordinator heartbeat renews.
func TestPromptFetchedUnitHasNoSpawnDeadline(t *testing.T) {
	for _, promptFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("promptFirst=", promptFirst), func(t *testing.T) {
			d := lsDef("prompt-spawn", true)
			h := newHarnessOpts(t, d, nil, lsOpts())
			run, ins := handOneUnit(t, h, d)
			started := h.now
			if !promptFirst {
				if err := h.e.Started(run, ins.Name, wf.Claim{Agent: "a"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.e.Prompt(run, ins.Name); err != nil {
				t.Fatal(err)
			}
			h = resumeFrom(t, h, d, run, len(h.records(run)))
			h.now = started.Add(2 * time.Minute)
			if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, true); err != nil {
				t.Fatal(err)
			}
			h.now = started.Add(4 * time.Minute) // past a spawn window from the claim
			h.handOut(run, 0)
			if n := len(lostRecords(t, h, run)); n != 0 {
				t.Fatalf("a fetched unit was lost past the spawn window: %d", n)
			}
		})
	}
}

// A child that dies right after its prompt fetch, with no transcript and no
// Map lease, is not unbounded: when the implicit lease (FetchedLease from its
// last sign of life) runs out the coordinator is asked, and a died answer
// loses it and hands its unit out again.
func TestFetchedUnitWithoutTranscriptIsTimedOut(t *testing.T) {
	d := lsDef("prompt-dead", true)
	opts := lsOpts()
	h := newHarnessOpts(t, d, nil, opts)
	run, ins := handOneUnit(t, h, d)
	fetched := h.now
	if _, err := h.e.PromptClaim(run, ins.Name, claimToken(t, ins)); err != nil {
		t.Fatal(err)
	}
	h.now = fetched.Add(opts.SpawnWindow + time.Minute) // a drive past the spawn window keeps it
	h.handOut(run, 0)
	h.now = fetched.Add(opts.FetchedLease - time.Nanosecond)
	h.handOut(run, 0)
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("lost before the implicit lease ran out: %d", n)
	}
	h.now = fetched.Add(opts.FetchedLease)
	if again := h.handOut(run, 1); len(again) != 0 {
		t.Fatalf("handed out %+v before the coordinator answered", again)
	}
	lost := askedThenDied(t, h, run, ins.Name)
	if len(lost) != 1 || lost[0] != (wf.AttemptLostData{Name: ins.Name, Cause: wf.LostCauseDied}) {
		t.Fatalf("losses = %+v, want %s lost on the died answer", lost, ins.Name)
	}
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("replacement = %+v, want attempt 2", again)
	}
}

// A started that arrives after the implicit lease of a prompt claim ran out,
// with no drive in between, renews and attaches: the unit is not lost, and its
// done is accepted.
func TestLateStartedRenewsFetchedUnit(t *testing.T) {
	for _, transcript := range []bool{false, true} {
		t.Run(fmt.Sprint("transcript=", transcript), func(t *testing.T) {
			d := lsDef("prompt-late-started", true)
			m := newTranscripts()
			opts := lvOpts(m, 2*time.Hour)
			h := newHarnessOpts(t, d, nil, opts)
			run, ins := handOneUnit(t, h, d)
			fetched := h.now
			if _, err := h.e.PromptClaim(run, ins.Name, claimToken(t, ins)); err != nil {
				t.Fatal(err)
			}
			h.now = fetched.Add(opts.FetchedLease + time.Minute)
			cl := wf.Claim{Agent: "a"}
			if transcript {
				cl = claim("a")
				m.set(cl.Transcript, h.now)
			}
			if err := h.e.Started(run, ins.Name, cl); err != nil {
				t.Fatalf("late started: %v", err)
			}
			h.now = h.now.Add(2 * time.Minute) // inside the renewed lease
			if again := h.handOut(run, 1); len(again) != 0 {
				t.Fatalf("the late-started unit was handed out again: %+v", again)
			}
			if got := lostRecords(t, h, run); len(got) != 0 {
				t.Fatalf("losses = %+v, want none", got)
			}
			if _, err := h.e.Done(run, ins.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
				t.Fatalf("done: %v", err)
			}
		})
	}
}

// A unit claimed by started and never fetched keeps the spawn window.
func TestStartedOnlyUnitKeepsSpawnDeadline(t *testing.T) {
	d := lsDef("started-spawn", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, ins := handOneUnit(t, h, d)
	started := h.now
	if err := h.e.Started(run, ins.Name, wf.Claim{Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	h.now = started.Add(3*time.Minute - time.Nanosecond)
	h.handOut(run, 0)
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("lost before the spawn deadline: %d", n)
	}
	h.now = started.Add(3 * time.Minute)
	h.handOut(run, 0)
	lost := lostRecords(t, h, run)
	if len(lost) != 1 || lost[0].Name != ins.Name || lost[0].Cause != wf.LostCauseSpawn {
		t.Fatalf("loss at the spawn deadline = %+v", lost)
	}
}

// Attaching a transcript must not hide a spawn deadline that already passed.
func TestStartedTranscriptAttachmentSpawnBoundary(t *testing.T) {
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			d := lsDef("attach-spawn", true)
			h := newHarnessOpts(t, d, nil, lvOpts(newTranscripts(), 0))
			run, ins := handOneUnit(t, h, d)
			started := h.now
			if err := h.e.Started(run, ins.Name, wf.Claim{}); err != nil {
				t.Fatal(err)
			}
			h.now = started.Add(time.Minute)
			if err := h.e.Started(run, ins.Name, wf.Claim{Agent: "a"}); err != nil {
				t.Fatal(err)
			}
			h.now = started.Add(3*time.Minute + offset)
			cl := wf.Claim{Agent: "a", Transcript: "/t/a.jsonl"}
			err := h.e.Started(run, ins.Name, cl)
			if offset < 0 {
				if err != nil {
					t.Fatal(err)
				}
				st := h.status(run)
				if len(st.Units) != 1 || st.Units[0].Elapsed != 3*time.Minute-time.Nanosecond {
					t.Fatalf("attachment reset original timing: %+v", st.Units)
				}
				h = resumeFrom(t, h, d, run, len(h.records(run)))
				h.now = started.Add(3 * time.Minute)
				if err := h.e.Started(run, ins.Name, cl); err != nil {
					t.Fatalf("replayed timely attachment: %v", err)
				}
				// The prompt fetch is the spawn signal and proof of life: even at
				// the spawn window's end, before any drive lost the unit, it is
				// accepted and ends the window (the transcript then bounds it).
				if _, err := h.e.Prompt(run, ins.Name); err != nil {
					t.Fatal(err)
				}
				h.handOut(run, 0)
				if got := lostRecords(t, h, run); len(got) != 0 {
					t.Fatalf("timely transcript attachment lost attempt: %+v", got)
				}
				return
			}
			// Check the loss before any drive can repair it on our behalf.
			if got := lostRecords(t, h, run); len(got) != 1 || got[0] != (wf.AttemptLostData{Name: ins.Name, Cause: wf.LostCauseSpawn}) {
				t.Fatalf("late attachment losses = %+v, want original attempt lost to spawn", got)
			}
			if !errors.Is(err, wf.ErrRejected) {
				t.Fatalf("late attachment = %v, want ErrRejected", err)
			}
			if got := attemptStarted(t, h, run); len(got) != 2 || got[1].Transcript != "" {
				t.Fatalf("late attachment journaled metadata: %+v", got)
			}
			if got := h.kind(run, wf.JournalAttemptStarted)[0]; !got.At.Equal(started) || got.Attempt != ins.Attempt {
				t.Fatalf("original attempt timing/identity changed: %+v", got)
			}
			h = resumeFrom(t, h, d, run, len(h.records(run)))
			if err := h.e.Started(run, ins.Name, cl); !errors.Is(err, wf.ErrRejected) {
				t.Fatalf("replayed expired attachment = %v, want ErrRejected", err)
			}
			again := h.handOut(run, 1)
			if len(again) != 1 || again[0].Attempt != 2 || again[0].Name == ins.Name {
				t.Fatalf("replacement = %+v, want a new attempt 2", again)
			}
			if got := lostRecords(t, h, run); len(got) != 1 {
				t.Fatalf("replay lost attempt again: %+v", got)
			}
			for _, due := range h.status(run).Due {
				if due.Kind == "spawn" {
					t.Fatalf("retired attempt retained spawn deadline: %+v", due)
				}
			}
		})
	}
}

// Compatible metadata must not rescue an unmonitored attempt past its lease,
// even when the spawn window has not yet expired (or expires later as well).
func TestStartedTranscriptAttachmentLeaseBoundary(t *testing.T) {
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Second, 3 * time.Minute} {
		for _, transcript := range []string{"", "/t/a.jsonl"} {
			t.Run(fmt.Sprintf("%s/transcript=%t", offset, transcript != ""), func(t *testing.T) {
				kid := wf.New("kid")
				child := wf.Typed[lItem, lRes](kid)
				work := wf.NewAgent[lWork, lOut](kid)
				kdone := wf.NewTerminal[lKDon](kid, wf.Success)
				work.Spec(wf.PromptText("Work."), wf.Retry(wf.Const(2), work.To(kdone, "gave up")))
				next := work.To(kdone, "done")
				work.Route(func(lOut, wf.Ctx) wf.Edge[lWork] { return next }, next)
				d := wf.New("attach-lease")
				src := wf.FromObservation[lItem](wf.Observe[[]lItem]("x", "items", nil))
				wf.NewMap[lFan, lItem, string, lRes](d, src, func(i lItem) string { return i.ID }, child).Lease(time.Minute)
				wf.NewTerminal[lEnd](d, wf.Success)
				opts := lvOpts(newTranscripts(), 0)
				h := newHarnessOpts(t, d, newWorld(), opts)
				run := h.run(d, nil)
				spawn := wf.JournalRecord{
					Kind: wf.JournalChildSpawned, Run: run, DefHash: h.records(run)[0].DefHash, At: h.now,
					Data: mustJSON(t, wf.ChildSpawnedData{ID: "kid-1", Def: "kid", State: "lFan"}),
				}
				unlock, err := h.j.Lock(run)
				if err != nil {
					t.Fatal(err)
				}
				err = h.j.Append(spawn)
				unlock()
				if err != nil {
					t.Fatal(err)
				}
				ins := h.handOut(run, 1)
				if len(ins) != 1 {
					t.Fatalf("HandOut = %+v, want one child", ins)
				}
				original := ins[0]
				started := h.now
				if _, err := h.e.Prompt(run, original.Name); err != nil {
					t.Fatal(err)
				}
				replay := func() {
					opts.Clock = func() time.Time { return h.now }
					opts.Defs = []*wf.Def{d}
					h.e = wf.NewEngine(h.j, h.reg, opts)
				}
				replay()
				h.now = started.Add(time.Minute + offset)
				cl := wf.Claim{Agent: "a", Transcript: transcript}
				err = h.e.Started(run, original.Name, cl)
				if offset < 0 {
					if err != nil {
						t.Fatal(err)
					}
					got, late := attemptStarted(t, h, run), metadataAttached(t, h, run)
					if len(got) != 1 || !got[0].Expires.Equal(started.Add(time.Minute)) || len(late) != 1 || late[0].Agent != "a" || late[0].Transcript != transcript || !late[0].Late {
						t.Fatalf("timely attachment changed lease or metadata: %+v %+v", got, late)
					}
					st := h.status(run)
					if len(st.Units) != 1 || st.Units[0].Elapsed != time.Minute-time.Nanosecond {
						t.Fatalf("attachment reset original timing: %+v", st.Units)
					}
					replay()
					if err := h.e.Started(run, original.Name, cl); err != nil {
						t.Fatalf("replayed compatible attachment: %v", err)
					}
					h.now = started.Add(time.Minute)
					if transcript != "" {
						if err := h.e.Started(run, original.Name, wf.Claim{Session: "s"}); err != nil {
							t.Fatalf("metadata for watched running attempt at lease expiry: %v", err)
						}
					}
					h.handOut(run, 0)
					lost := lostRecords(t, h, run)
					if transcript != "" {
						if len(lost) != 0 || countStatus(heartbeats(t, h, run), "stalled") != 1 {
							t.Fatalf("watched lease must stall, losses=%+v heartbeats=%+v", lost, heartbeats(t, h, run))
						}
						if err := h.e.Started(run, original.Name, wf.Claim{Harness: "h"}); err != nil {
							t.Fatalf("metadata for watched stalled attempt: %v", err)
						}
					} else if len(lost) != 0 || countStatus(heartbeats(t, h, run), "stalled") != 1 {
						t.Fatalf("unmonitored fetched lease must ask, losses=%+v heartbeats=%+v", lost, heartbeats(t, h, run))
					}
					return
				}
				if got := lostRecords(t, h, run); len(got) != 1 || got[0] != (wf.AttemptLostData{Name: original.Name, Cause: wf.LostCauseLease}) {
					t.Fatalf("late attachment losses = %+v, want original attempt lost to lease", got)
				}
				if !errors.Is(err, wf.ErrRejected) {
					t.Fatalf("late attachment = %v, want ErrRejected", err)
				}
				if got := attemptStarted(t, h, run); len(got) != 1 || got[0].Agent != "" || got[0].Transcript != "" || !got[0].Expires.Equal(started.Add(time.Minute)) {
					t.Fatalf("late attachment journaled metadata or changed lease: %+v", got)
				}
				if got := h.kind(run, wf.JournalAttemptStarted)[0]; !got.At.Equal(started) || got.Attempt != original.Attempt {
					t.Fatalf("original timing/identity changed: %+v", got)
				}
				replay()
				if err := h.e.Started(run, original.Name, cl); !errors.Is(err, wf.ErrRejected) {
					t.Fatalf("replayed expired attachment = %v, want ErrRejected", err)
				}
				again := h.handOut(run, 1)
				if len(again) != 1 || again[0].Attempt != 2 || again[0].Name == original.Name {
					t.Fatalf("replacement = %+v, want distinct attempt 2", again)
				}
				if err := h.e.Started(run, again[0].Name, wf.Claim{Agent: "b", Transcript: "/t/b.jsonl"}); err != nil {
					t.Fatal(err)
				}
				replay()
				if err := h.e.Started(run, original.Name, cl); !errors.Is(err, wf.ErrRejected) {
					t.Fatalf("retired name reached replacement: %v", err)
				}
				got := attemptStarted(t, h, run)
				if len(got) != 2 || got[1].Name != again[0].Name || got[1].Agent != "b" || !got[1].Expires.Equal(h.now.Add(time.Minute)) || len(lostRecords(t, h, run)) != 1 {
					t.Fatalf("replacement metadata/loss changed after replay: %+v", got)
				}
			})
		}
	}
}

func TestStallReportedNotReissued(t *testing.T) {
	d := lsDef("lv-a", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)

	h.now = h.now.Add(19 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 0 {
		t.Fatalf("stalled heartbeats inside the threshold = %d, want 0", n)
	}
	h.now = h.now.Add(61 * time.Second) // 20m1s after the start
	h.handOut(run, 0)
	h.handOut(run, 0) // a second drive must not report it again

	hbs := heartbeats(t, h, run)
	if len(hbs) != 1 || hbs[0] != (wf.HeartbeatData{Name: ins.Name, Status: "stalled", By: "engine"}) {
		t.Fatalf("heartbeats = %+v, want exactly one stalled for %s", hbs, ins.Name)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("a stall journaled %d losses, want 0", n)
	}
	if again := h.handOut(run, 1); len(again) != 0 {
		t.Fatalf("HandOut after a stall = %+v, want nothing: the unit is not reissued", again)
	}
	if err := h.e.Started(run, ins.Name, claim("b")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("a second agent claiming the stalled unit = %v, want ErrRejected", err)
	}
}

func TestStallFromOldMtime(t *testing.T) {
	d := lsDef("lv-b", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run, first := handOneUnit(t, h, d)
	if err := h.e.Started(run, first.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(2 * time.Minute) // the class learns a 2m attempt
	if _, err := h.e.Failed(run, first.Name, "no"); err != nil {
		t.Fatal(err)
	}

	again := h.handOut(run, 1)
	if len(again) != 1 {
		t.Fatalf("HandOut after the failure = %+v, want the reissue", again)
	}
	started := h.now
	lvRunning(t, h, m, run, again[0])

	h.now = started.Add(5 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 0 {
		t.Fatalf("stalled at 5m with a 6m threshold: %d heartbeats", n)
	}
	m.set("/t/a.jsonl", started) // the transcript is 7m old at the next drive
	h.now = started.Add(7 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats at 7m with a 6m threshold = %d, want 1", n)
	}
}

func TestTranscriptWriteRenewsLease(t *testing.T) {
	h, m, run, _ := lvLeased(t, 0)
	// The lease is 10m: a write at 8m, seen at 9m, renews it to 18m.
	start := h.now
	m.set("/t/a.jsonl", start.Add(8*time.Minute))
	h.now = start.Add(9 * time.Minute)
	h.handOut(run, 0)
	hbs := heartbeats(t, h, run)
	if len(hbs) != 1 || hbs[0].Status != "active" || hbs[0].By != "transcript" || !hbs[0].Expires.Equal(start.Add(18*time.Minute)) {
		t.Fatalf("heartbeats = %+v, want one transcript active with the lease renewed to start+18m", hbs)
	}
	h.now = start.Add(11 * time.Minute) // past the original lease
	h.handOut(run, 0)
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("losses past the original lease = %d, want 0: the write renewed it", n)
	}
	h.now = start.Add(20 * time.Minute) // past the renewed lease
	h.handOut(run, 0)
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("losses past the renewed lease = %+v, want none: a transcript-backed unit is stalled instead", got)
	}
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats past the renewed lease = %d, want 1", n)
	}
}

func TestTranscriptWriteClearsStall(t *testing.T) {
	d := lsDef("lv-clear", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)
	start := h.now

	h.now = start.Add(21 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats = %d, want 1", n)
	}
	m.set("/t/a.jsonl", start.Add(22*time.Minute))
	h.now = start.Add(23 * time.Minute)
	h.handOut(run, 0)
	hbs := heartbeats(t, h, run)
	if last := hbs[len(hbs)-1]; last.Status != "active" || last.By != "transcript" {
		t.Fatalf("last heartbeat = %+v, want a transcript active", last)
	}
	// The threshold restarts at the write (22m), not the sample (23m): no stall
	// before 22m + 20m.
	h.now = start.Add(41 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats 19m after the write = %d, want still 1", n)
	}
	h.now = start.Add(43 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 2 {
		t.Fatalf("stalled heartbeats 21m after the write = %d, want 2", n)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("stalls and clears journaled %d losses, want 0", n)
	}
}

// lvLeased is a Map child's unit under a 10m lease with a huge lost cap, claimed
// and fetched at the harness's start time.
func lvLeased(t *testing.T, stallDefault time.Duration) (*harness, *transcripts, wf.RunID, wf.Instruction) {
	t.Helper()
	h, m, run, ins := lvLeasedHandedOut(t, stallDefault)
	lvRunning(t, h, m, run, ins)
	return h, m, run, ins
}

// lvLeasedHandedOut is lvLeased's Map child unit, handed out and not claimed.
func lvLeasedHandedOut(t *testing.T, stallDefault time.Duration) (*harness, *transcripts, wf.RunID, wf.Instruction) {
	t.Helper()
	kid := wf.New("kid")
	child := wf.Typed[lItem, lRes](kid)
	work := wf.NewAgent[lWork, lOut](kid)
	kdone := wf.NewTerminal[lKDon](kid, wf.Success)
	work.Spec(wf.PromptText("Work."), wf.Retry(wf.Const(2), work.To(kdone, "gave up")))
	next := work.To(kdone, "done")
	work.Route(func(lOut, wf.Ctx) wf.Edge[lWork] { return next }, next)

	d := wf.New("lv-lease")
	src := wf.FromObservation[lItem](wf.Observe[[]lItem]("x", "items", nil))
	wf.NewMap[lFan, lItem, string, lRes](d, src, func(i lItem) string { return i.ID }, child).Lease(10 * time.Minute)
	wf.NewTerminal[lEnd](d, wf.Success)

	m := newTranscripts()
	h := newHarnessOpts(t, d, newWorld(), lvOpts(m, stallDefault))
	run := h.run(d, nil)
	spawn := wf.JournalRecord{
		Kind: wf.JournalChildSpawned, Run: run, DefHash: h.records(run)[0].DefHash, At: h.now,
		Data: mustJSON(t, wf.ChildSpawnedData{ID: "kid-1", Def: "kid", State: "lFan"}),
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(spawn)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	ins := h.handOut(run, 1)
	if len(ins) != 1 {
		t.Fatalf("HandOut = %+v, want the child's unit", ins)
	}
	return h, m, run, ins[0]
}

// The lease starts at the claim (here a started), never at the hand-out, and
// a coordinator heartbeat or a transcript write renews it. A later started
// keeps a Map lease as it is.
func TestLeaseStartsAtClaimAndHeartbeatRenewsIt(t *testing.T) {
	h, m, run, ins := lvLeasedHandedOut(t, 0)
	start := h.now
	h.now = start.Add(90 * time.Second) // the unit sits handed for a while
	h.handOut(run, 0)
	claimAt := h.now
	m.set("/t/a.jsonl", claimAt)
	if err := h.e.Started(run, ins.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if got := attemptStarted(t, h, run); len(got) != 1 || !got[0].Expires.Equal(claimAt.Add(10*time.Minute)) {
		t.Fatalf("attempt_started = %+v, want the lease from the claim time (%v) + 10m", got, claimAt)
	}
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
	h.now = claimAt.Add(5 * time.Minute)
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, true); err != nil {
		t.Fatal(err)
	}
	hbs := heartbeats(t, h, run)
	if last := hbs[len(hbs)-1]; last.By != "coordinator" || !last.Expires.Equal(claimAt.Add(15*time.Minute)) {
		t.Fatalf("heartbeat = %+v, want a coordinator renewal to claim+15m", last)
	}
	h.now = claimAt.Add(12 * time.Minute) // past the original lease, inside the renewed one
	h.handOut(run, 0)
	if n := len(lostRecords(t, h, run)); n != 0 || countStatus(heartbeats(t, h, run), "stalled") != 0 {
		t.Fatalf("losses = %d, stalled = %d past the original lease: the heartbeat did not renew it", n, countStatus(heartbeats(t, h, run), "stalled"))
	}
}

// A wf started that arrives after the SpawnWindow of a prompt claim attaches:
// the fetched unit lives by its lease, so it is neither lost nor re-issued and
// its done is accepted.
func TestLateStartedAfterPromptClaimAttaches(t *testing.T) {
	h, m, run, ins := lvLeasedHandedOut(t, 0)
	start := h.now
	m.set("/t/a.jsonl", start)
	if _, err := h.e.PromptClaim(run, ins.Name, claimToken(t, ins)); err != nil {
		t.Fatal(err)
	}
	h.now = start.Add(4 * time.Minute) // beyond SpawnWindow (3m), inside the 10m lease
	h.handOut(run, 0)
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("a prompt-fetched unit was lost at the spawn window: %d", n)
	}
	if err := h.e.Started(run, ins.Name, claim("a")); err != nil {
		t.Fatalf("late started: %v", err)
	}
	if again := h.handOut(run, 1); len(again) != 0 {
		t.Fatalf("HandOut after the late started = %+v, want nothing re-issued", again)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("losses after the late started = %d, want 0", n)
	}
	res, err := h.e.Done(run, ins.Name, wf.Submission{Output: json.RawMessage(`{}`)})
	if err != nil || res.Class != wf.Accepted {
		t.Fatalf("done = %+v, %v, want accepted", res, err)
	}
}

func TestNoTranscriptNeverStalls(t *testing.T) {
	d := lsDef("lv-c", true)
	m := newTranscripts()
	opts := lvOpts(m, 0)
	opts.SpawnWindow = 11 * time.Hour  // Isolate stall behavior while still inside the spawn window
	opts.FetchedLease = 11 * time.Hour // and the fetched-unit lease.
	h := newHarnessOpts(t, d, nil, opts)
	run, ins := handOneUnit(t, h, d)
	noPath := wf.Claim{Harness: "claude", Session: "sess-1", Agent: "a"}
	if err := h.e.Started(run, ins.Name, noPath); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Prompt(run, ins.Name); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Hour)
	h.handOut(run, 0)
	if hbs := heartbeats(t, h, run); len(hbs) != 0 {
		t.Fatalf("heartbeats for a claim with no transcript = %+v, want none", hbs)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("an unleased unit with no transcript was lost: %v", lostRecords(t, h, run))
	}
}

func TestUnstatableTranscriptNeverStalls(t *testing.T) {
	h, m, run, _ := lvLeased(t, time.Minute) // a 1m stall threshold: a stall would show well before the lease
	m.errs["/t/a.jsonl"] = fs.ErrPermission
	h.now = h.now.Add(9 * time.Minute)
	h.handOut(run, 0)
	if hbs := heartbeats(t, h, run); len(hbs) != 0 {
		t.Fatalf("heartbeats for an unstatable transcript = %+v, want none", hbs)
	}
	// Past the stall threshold, still nothing; the lease (10m, Map-claimed)
	// running out is what asks the coordinator.
	h.now = h.now.Add(2 * time.Minute)
	h.handOut(run, 0)
	hbs := heartbeats(t, h, run)
	if len(hbs) != 1 || hbs[0].Status != "stalled" || hbs[0].By != "engine" {
		t.Fatalf("heartbeats = %+v, want the lease's one question", hbs)
	}
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("losses = %+v, want none before the answer", got)
	}
}

func TestHeartbeatActiveRestartsThreshold(t *testing.T) {
	d := lsDef("lv-d", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)
	start := h.now

	h.now = start.Add(21 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats = %d, want 1", n)
	}
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, true); err != nil {
		t.Fatal(err)
	}
	hbs := heartbeats(t, h, run)
	if last := hbs[len(hbs)-1]; last.Status != "active" || last.By != "coordinator" {
		t.Fatalf("last heartbeat = %+v, want a coordinator active", last)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("heartbeat active journaled %d losses, want 0", n)
	}

	h.now = start.Add(40 * time.Minute) // 19m after the answer
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats 19m after active = %d, want still 1", n)
	}
	h.now = start.Add(42 * time.Minute) // 21m after the answer
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 2 {
		t.Fatalf("stalled heartbeats 21m after active = %d, want 2", n)
	}
	if res, err := h.e.Done(run, ins.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatalf("Done on the revived unit = %+v, %v", res, err)
	}
}

func TestHeartbeatDiedReissues(t *testing.T) {
	d := lsDef("lv-e", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)

	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: "no-such-unit"}, false); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Heartbeat on an unknown name = %v, want ErrRejected", err)
	}
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, false); err != nil {
		t.Fatal(err)
	}
	got := lostRecords(t, h, run)
	if len(got) != 1 || got[0] != (wf.AttemptLostData{Name: ins.Name, Cause: wf.LostCauseDied}) {
		t.Fatalf("attempt_lost = %+v, want one died for %s", got, ins.Name)
	}
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, true); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Heartbeat on the retired name = %v, want ErrRejected", err)
	}
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Attempt != 2 || again[0].Name == ins.Name {
		t.Fatalf("HandOut after died = %+v, want a new unit at attempt 2", again)
	}
	// No retry was spent: Retry(Const(2)) still allows a refused output and
	// another reissue.
	if err := h.e.Started(run, again[0].Name, claim("c")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Failed(run, again[0].Name, "no"); err != nil {
		t.Fatalf("Failed after the loss: %v", err)
	}
	if st := h.status(run); st.Phase != wf.RunRunning {
		t.Fatalf("status %+v: died spent a retry, the first Failed exhausted Const(2)", st)
	}
}

func TestHeartbeatDiedOnLateVoterIsNoop(t *testing.T) {
	d := fanDef("lv-late", chItems(1, 2, 3), 3, wf.Quorum(1))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	seats := h.handOut(run, 3)
	byChild := map[string]string{}
	for _, s := range seats {
		byChild[s.Child] = s.Name
	}
	one, three := byChild["chFan[1]"], byChild["chFan[3]"]
	if len(seats) != 3 || one == "" || three == "" {
		t.Fatalf("hand-out %+v lacks seats 1 and 3", seats)
	}
	for i, n := range []string{one, three} {
		if err := h.e.Started(run, n, claim([]string{"a1", "a3"}[i])); err != nil {
			t.Fatal(err)
		}
		if _, err := h.e.Prompt(run, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.e.Done(run, one, jsonOut(t, `{"OK":true}`)); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")

	before, statusBefore := len(h.records(run)), h.status(run)
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: three}, false); err != nil {
		t.Fatalf("Heartbeat died on a late voter = %v, want nil", err)
	}
	if after := h.records(run); len(after) != before {
		t.Fatalf("records %d -> %d: died on a settled child journaled %+v", before, len(after), after[before:])
	}
	if got := h.status(run); !reflect.DeepEqual(got, statusBefore) {
		t.Fatalf("status changed:\n got %+v\nwant %+v", got, statusBefore)
	}
	if _, err := h.e.Done(run, three, jsonOut(t, `{"OK":true}`)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.kind(run, wf.JournalLateVote)); n != 1 {
		t.Fatalf("late_vote records = %d, want 1: the no-op retired the seat", n)
	}
}

func TestHeartbeatDiedSessionOnlyThatSession(t *testing.T) {
	d := fanDef("lv-fan", chItems(1, 2), 2, wf.AllSettled())
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run := h.run(d, nil)
	ins := h.handOut(run, 2)
	if len(ins) != 2 {
		t.Fatalf("HandOut(2) = %+v, want two units", ins)
	}
	cx := wf.Claim{Harness: "claude", Session: "X", Agent: "ax", Transcript: "/t/x.jsonl"}
	cy := wf.Claim{Harness: "claude", Session: "Y", Agent: "ay", Transcript: "/t/y.jsonl"}
	if err := h.e.Started(run, ins[0].Name, cx); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Started(run, ins[1].Name, cy); err != nil {
		t.Fatal(err)
	}

	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Session: "nobody"}, false); err != nil {
		t.Fatalf("died for a session with no units = %v, want nil", err)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("a session with no units lost %d units", n)
	}
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Session: "X"}, true); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("alive for a session = %v, want ErrRejected", err)
	}
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Session: "X"}, false); err != nil {
		t.Fatal(err)
	}
	got := lostRecords(t, h, run)
	if len(got) != 1 || got[0] != (wf.AttemptLostData{Name: ins[0].Name, Cause: wf.LostCauseOrphan}) {
		t.Fatalf("attempt_lost = %+v, want only %s lost as an orphan", got, ins[0].Name)
	}
	if _, err := h.e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{}`)}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("late Done from the orphan = %v, want ErrRejected", err)
	}
	if _, err := h.e.Prompt(run, ins[1].Name); err != nil {
		t.Fatalf("Y's unit must stay claimed: %v", err)
	}
}

func TestStallWakesWait(t *testing.T) {
	d := lsDef("lv-wait", true)
	m := newTranscripts()
	opts := wf.EngineOptions{StallDefault: 150 * time.Millisecond, TranscriptMtime: m.mtime}
	w := newWaiter(t, d, opts)
	run := w.start(d)
	ins, err := w.e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOut = %+v, %v", ins, err)
	}
	m.set("/t/a.jsonl", time.Now().Add(-time.Hour))
	if err := w.e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.Prompt(run, ins[0].Name); err != nil {
		t.Fatal(err)
	}

	begin := time.Now()
	ev := oneEvent(t, w.wait(run, -1, 5*time.Second), wf.EventStall)
	if ev.Name != ins[0].Name {
		t.Fatalf("event = %+v, want a stall of %s", ev, ins[0].Name)
	}
	if took := time.Since(begin); took < 100*time.Millisecond {
		t.Fatalf("Wait returned after %v, before the threshold passed", took)
	}
}

// A transcript written on every drive costs at most four active heartbeats per
// stall window: a write is journaled only once a quarter window has passed
// since the last sign of life.
func TestBusyTranscriptDebounced(t *testing.T) {
	d := lsDef("lv-busy", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 4*time.Minute))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)

	for i := 0; i < 48; i++ { // 4 minutes in 5s steps
		h.now = h.now.Add(5 * time.Second)
		m.set("/t/a.jsonl", h.now)
		h.handOut(run, 0)
	}
	hbs := heartbeats(t, h, run)
	if n := countStatus(hbs, "active"); n < 1 || n > 4 {
		t.Fatalf("active heartbeats over one 4m window = %d, want 1 to 4", n)
	}
	if n := countStatus(hbs, "stalled"); n != 0 {
		t.Fatalf("a continuously written transcript stalled %d times", n)
	}
	for _, hb := range hbs {
		if hb.By != "transcript" {
			t.Fatalf("heartbeat %+v, want only transcript heartbeats", hb)
		}
	}
}

// A write inside the debounce margin is not journaled at once, but when the
// stall falls due drive samples first, journals it, and the stall is postponed.
func TestDebouncedWriteJournaledWhenStallDue(t *testing.T) {
	const T = 10 * time.Minute
	d := lsDef("lv-due", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, T))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)
	start := h.now

	m.set("/t/a.jsonl", start.Add(T/5))
	h.now = start.Add(3 * T / 10)
	h.handOut(run, 0)
	if hbs := heartbeats(t, h, run); len(hbs) != 0 {
		t.Fatalf("heartbeats at 0.3T = %+v, want none: the write at 0.2T is inside the margin", hbs)
	}
	h.now = start.Add(T)
	h.handOut(run, 0)
	hbs := heartbeats(t, h, run)
	if len(hbs) != 1 || hbs[0].Status != "active" || hbs[0].By != "transcript" {
		t.Fatalf("heartbeats at 1.0T = %+v, want one transcript active and no stall", hbs)
	}
	h.now = start.Add(11 * T / 10)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 0 {
		t.Fatalf("stalled heartbeats at 1.1T = %d, want 0: the write postponed the stall", n)
	}
}

// A write held inside the margin is journaled with its own mtime, so the stall
// is measured from the write, not from the sample that journaled it.
func TestDebouncedWriteMeasuresStallFromWrite(t *testing.T) {
	const T = 10 * time.Minute
	d := lsDef("lv-from-write", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, T))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)
	start := h.now

	m.set("/t/a.jsonl", start.Add(T/5))
	h.now = start.Add(3 * T / 10)
	h.handOut(run, 0)
	if hbs := heartbeats(t, h, run); len(hbs) != 0 {
		t.Fatalf("heartbeats at 0.3T = %+v, want none", hbs)
	}
	h.now = start.Add(T)
	h.handOut(run, 0)
	hbs := heartbeats(t, h, run)
	if len(hbs) != 1 || hbs[0].By != "transcript" || !hbs[0].Written.Equal(start.Add(T/5)) {
		t.Fatalf("heartbeats at 1.0T = %+v, want one transcript heartbeat with Written = start+0.2T", hbs)
	}
	h.now = start.Add(5 * T / 4) // past 0.2T + T
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats at 1.25T = %d, want 1: measured from the write at 0.2T", n)
	}
}

// A heartbeat record from before Written existed still folds, and its stall is
// measured from the record's own At.
func TestOldHeartbeatWithoutWrittenFolds(t *testing.T) {
	h, _, run, ins := lvLeased(t, 4*time.Minute)
	start := h.now
	at := start.Add(2 * time.Minute)
	old := wf.JournalRecord{
		Kind: wf.JournalHeartbeat, Run: run, DefHash: h.records(run)[0].DefHash, At: at,
		Data: json.RawMessage(`{"Name":"` + ins.Name + `","Status":"active","By":"transcript"}`),
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(old)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.now = start.Add(5 * time.Minute)
	h.status(run) // the old record folds
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 0 {
		t.Fatalf("stalled heartbeats at 5m = %d, want 0: the 4m threshold restarted at the record (2m)", n)
	}
	h.now = start.Add(7 * time.Minute) // past the record At + 4m, before the 10m lease
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats at 7m = %d, want 1", n)
	}
}

// The same for a lease shorter than the stall threshold: it is the window, and
// a write inside its margin renews the lease when it falls due.
func TestDebouncedWriteRenewsLeaseWhenDue(t *testing.T) {
	h, m, run, _ := lvLeased(t, 0)
	start := h.now // the lease is 10m
	m.set("/t/a.jsonl", start.Add(2*time.Minute))
	h.now = start.Add(4 * time.Minute)
	h.handOut(run, 0)
	if hbs := heartbeats(t, h, run); len(hbs) != 0 {
		t.Fatalf("heartbeats at 4m = %+v, want none: the write at 2m is inside the 2.5m margin", hbs)
	}
	h.now = start.Add(10 * time.Minute) // the lease falls due
	h.handOut(run, 0)
	hbs := heartbeats(t, h, run)
	if len(hbs) != 1 || hbs[0].Status != "active" || hbs[0].By != "transcript" || !hbs[0].Expires.Equal(start.Add(12*time.Minute)) {
		t.Fatalf("heartbeats at the lease expiry = %+v, want one transcript active renewed to start+12m", hbs)
	}
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("losses at the lease expiry = %d, want 0: the write renewed it", n)
	}
	h.now = start.Add(12 * time.Minute) // the lease measured from the write at 2m
	h.handOut(run, 0)
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("losses at write+lease = %+v, want none: a transcript-backed unit is stalled instead", got)
	}
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats at write+lease = %d, want 1", n)
	}
}

// leaseStalled drives lvLeased's unit past its 10m lease and returns the state
// right after the stall report.
func leaseStalled(t *testing.T) (*harness, *transcripts, wf.RunID, wf.Instruction) {
	t.Helper()
	h, m, run, ins := lvLeased(t, 0)
	h.now = h.now.Add(10*time.Minute + time.Second)
	h.handOut(run, 0)
	return h, m, run, ins
}

// A silent transcript is not evidence of death: at its lease expiry a
// transcript-backed running unit is reported stalled, never lost or reissued.
func TestLeaseExpiryStallsTranscriptUnit(t *testing.T) {
	h, _, run, ins := lvLeased(t, 0)
	start := h.now
	baseline := int64(len(h.records(run)))
	h.now = start.Add(10*time.Minute + time.Second)
	h.handOut(run, 0)
	h.now = start.Add(12 * time.Minute)
	h.handOut(run, 0) // a second and third drive journal nothing more
	h.now = start.Add(15 * time.Minute)
	h.handOut(run, 0)

	hbs := heartbeats(t, h, run)
	if len(hbs) != 1 || hbs[0] != (wf.HeartbeatData{Name: ins.Name, Status: "stalled", By: "engine"}) {
		t.Fatalf("heartbeats = %+v, want exactly one stalled for %s", hbs, ins.Name)
	}
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("a lease expiry of a transcript-backed unit lost %+v, want no loss", got)
	}
	if again := h.handOut(run, 1); len(again) != 0 {
		t.Fatalf("HandOut after the stall = %+v, want nothing: the unit is not reissued", again)
	}
	if err := h.e.Started(run, ins.Name, claim("b")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("a second agent claiming the stalled unit = %v, want ErrRejected", err)
	}
	st := h.status(run)
	if len(st.Units) != 1 || st.Units[0].State != "stalled" {
		t.Fatalf("Units = %+v, want the one unit stalled", st.Units)
	}
	for _, dt := range st.Due {
		if dt.Kind == "lease" {
			t.Fatalf("Due = %+v, want no lease deadline for a stalled transcript-backed unit", st.Due)
		}
	}
	res, err := h.e.Wait(run, baseline, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if evs := eventsOf(res, wf.EventStall); len(evs) != 1 || evs[0].Name != ins.Name {
		t.Fatalf("events = %+v, want exactly one stall of %s", res.Events, ins.Name)
	}
}

// Only heartbeat died loses the stalled unit; it reissues at attempt+1 and
// spends no retry.
func TestDiedAfterLeaseStallReissues(t *testing.T) {
	h, _, run, ins := leaseStalled(t)
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, false); err != nil {
		t.Fatal(err)
	}
	got := lostRecords(t, h, run)
	if len(got) != 1 || got[0] != (wf.AttemptLostData{Name: ins.Name, Cause: wf.LostCauseDied}) {
		t.Fatalf("attempt_lost = %+v, want one died for %s", got, ins.Name)
	}
	again := h.handOut(run, 1)
	if len(again) != 1 || again[0].Attempt != 2 || again[0].Name == ins.Name {
		t.Fatalf("HandOut after died = %+v, want a new unit at attempt 2", again)
	}
	// No retry was spent: had died spent one, the first Failed would exhaust
	// Retry(Const(2)) instead of reissuing at attempt 3.
	if err := h.e.Started(run, again[0].Name, claim("c")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Failed(run, again[0].Name, "no"); err != nil {
		t.Fatalf("Failed after the loss: %v", err)
	}
	if next := h.handOut(run, 1); len(next) != 1 || next[0].Attempt != 3 {
		t.Fatalf("HandOut after the first Failed = %+v, want attempt 3: died spent a retry", next)
	}
}

func TestDiedSessionAfterLeaseStallReissues(t *testing.T) {
	h, _, run, ins := leaseStalled(t)
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Session: "sess-1"}, false); err != nil {
		t.Fatal(err)
	}
	got := lostRecords(t, h, run)
	if len(got) != 1 || got[0] != (wf.AttemptLostData{Name: ins.Name, Cause: wf.LostCauseOrphan}) {
		t.Fatalf("attempt_lost = %+v, want one orphan for %s", got, ins.Name)
	}
	if again := h.handOut(run, 1); len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("HandOut after died --session = %+v, want a new unit at attempt 2", again)
	}
}

// A transcript write after the stall renews the lease and revives the unit; the
// next expiry stalls it again.
func TestWriteAfterLeaseStallRenews(t *testing.T) {
	h, m, run, _ := leaseStalled(t)
	start := h.now.Add(-(10*time.Minute + time.Second))
	m.set("/t/a.jsonl", start.Add(12*time.Minute))
	h.now = start.Add(12 * time.Minute)
	h.handOut(run, 0)
	hbs := heartbeats(t, h, run)
	last := hbs[len(hbs)-1]
	if len(hbs) != 2 || last.Status != "active" || last.By != "transcript" || !last.Expires.Equal(start.Add(22*time.Minute)) {
		t.Fatalf("heartbeats = %+v, want the stall then one transcript active renewed to start+22m", hbs)
	}
	if st := h.status(run); len(st.Units) != 1 || st.Units[0].State != "running" {
		t.Fatalf("Units = %+v, want the unit running again", st.Units)
	}
	h.now = start.Add(23 * time.Minute)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 2 {
		t.Fatalf("stalled heartbeats at the renewed expiry = %d, want 2", n)
	}
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("losses = %+v, want none", got)
	}
}
