package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func journals(t *testing.T) map[string]wf.Journal {
	return map[string]wf.Journal{"file": wf.FileJournal(t.TempDir()), "mem": wf.MemJournal()}
}

func lock(t *testing.T, j wf.Journal, run wf.RunID) func() {
	t.Helper()
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}

func TestJournalRoundTrip(t *testing.T) {
	for name, j := range journals(t) {
		t.Run(name, func(t *testing.T) {
			defer lock(t, j, "r1")()
			in := []wf.JournalRecord{
				{Kind: wf.JournalRunStarted, Run: "r1", DefHash: "sha256:x", Seq: 99},
				{Kind: wf.JournalSubmitted, Run: "r1", State: "a/B", Attempt: 2, Class: wf.Accepted, Data: json.RawMessage(`{"k":"v"}`)},
			}
			for _, r := range in {
				if err := j.Append(r); err != nil {
					t.Fatal(err)
				}
			}
			got, err := j.Records("r1")
			if err != nil {
				t.Fatal(err)
			}
			in[0].Seq, in[1].Seq = 1, 2 // the journal assigns Seq; the caller's is ignored
			if !reflect.DeepEqual(got, in) {
				t.Errorf("got %+v\nwant %+v", got, in)
			}
		})
	}
}

func TestJournalUnknownRunAndListing(t *testing.T) {
	for name, j := range journals(t) {
		t.Run(name, func(t *testing.T) {
			unlock := lock(t, j, "nope")
			if _, err := j.Records("nope"); !errors.Is(err, wf.ErrUnknownRun) {
				t.Errorf("Records of an unknown run = %v", err)
			}
			unlock()
			for _, run := range []wf.RunID{"b", "a"} {
				u := lock(t, j, run)
				if err := j.Append(wf.JournalRecord{Kind: wf.JournalRunStarted, Run: run}); err != nil {
					t.Fatal(err)
				}
				u()
			}
			got, err := j.Runs()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, []wf.RunID{"a", "b"}) {
				t.Errorf("Runs = %v (a locked run with no records is not listed)", got)
			}
		})
	}
}

func TestJournalRunsOfNothing(t *testing.T) {
	for name, j := range journals(t) {
		t.Run(name, func(t *testing.T) {
			if got, err := j.Runs(); err != nil || len(got) != 0 {
				t.Errorf("Runs = %v, %v", got, err)
			}
		})
	}
}

func TestJournalNeedsTheLock(t *testing.T) {
	for name, j := range journals(t) {
		t.Run(name, func(t *testing.T) {
			if err := j.Append(wf.JournalRecord{Run: "r"}); !errors.Is(err, wf.ErrJournalNotLocked) {
				t.Errorf("Append = %v", err)
			}
			if _, err := j.Records("r"); !errors.Is(err, wf.ErrJournalNotLocked) {
				t.Errorf("Records = %v", err)
			}
		})
	}
}

func TestJournalRejectsUnsafeRunID(t *testing.T) {
	for name, j := range journals(t) {
		t.Run(name, func(t *testing.T) {
			for _, run := range []wf.RunID{"", ".", "..", "../x", "a/b"} {
				if _, err := j.Lock(run); err == nil {
					t.Errorf("Lock(%q) succeeded", run)
				}
			}
		})
	}
}

// cycle is one read-fold-append: the record it appends names the Seq it read
// last, so a lock that did not span the cycle shows up as a wrong prev.
func cycle(j wf.Journal, run wf.RunID) error {
	unlock, err := j.Lock(run)
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := j.Records(run)
	if err != nil && !errors.Is(err, wf.ErrUnknownRun) {
		return err
	}
	data := strconv.Itoa(len(recs))
	return j.Append(wf.JournalRecord{Kind: wf.JournalNote, Run: run, Data: json.RawMessage(data)})
}

func checkCycles(t *testing.T, j wf.Journal, run wf.RunID, want int) {
	t.Helper()
	defer lock(t, j, run)()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != want {
		t.Fatalf("%d records, want %d", len(recs), want)
	}
	for i, r := range recs {
		if r.Seq != int64(i+1) || string(r.Data) != strconv.Itoa(i) {
			t.Fatalf("record %d: seq %d read %s: a cycle was interleaved", i, r.Seq, r.Data)
		}
	}
}

func TestCycleLockAcrossGoroutines(t *testing.T) {
	dir := t.TempDir()
	mem := wf.MemJournal()
	for name, pair := range map[string][2]wf.Journal{
		"two file values": {wf.FileJournal(dir), wf.FileJournal(dir)},
		"one mem value":   {mem, mem},
	} {
		t.Run(name, func(t *testing.T) {
			var wg sync.WaitGroup
			for _, j := range []wf.Journal{pair[0], pair[1], pair[0], pair[1]} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 25 {
						if err := cycle(j, "r"); err != nil {
							t.Error(err)
							return
						}
					}
				}()
			}
			wg.Wait()
			checkCycles(t, pair[0], "r", 100)
		})
	}
}

func TestParallelLockersSerialize(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j := wf.FileJournal(dir)
			unlock, err := j.Lock("r")
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			if err := j.Append(wf.JournalRecord{Run: "r", Kind: wf.JournalNote, Data: json.RawMessage(strconv.Itoa(i))}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	j := wf.FileJournal(dir)
	defer lock(t, j, "r")()
	recs, err := j.Records("r")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	for i, r := range recs {
		if r.Seq != int64(i+1) {
			t.Errorf("record %d has Seq %d", i, r.Seq)
		}
	}
}

const helperDirEnv = "WF_JOURNAL_HELPER_DIR"

// TestJournalHelperProcess is the child of TestCycleLockAcrossProcesses.
func TestJournalHelperProcess(t *testing.T) {
	dir := os.Getenv(helperDirEnv)
	if dir == "" {
		t.Skip("runs only as the child of TestCycleLockAcrossProcesses")
	}
	j := wf.FileJournal(dir)
	for range 50 {
		if err := cycle(j, "r"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCycleLockAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestJournalHelperProcess$")
			cmd.Env = append(os.Environ(), helperDirEnv+"="+dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("helper: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()
	checkCycles(t, wf.FileJournal(dir), "r", 100)
}

func journalPath(dir string, run wf.RunID) string {
	return filepath.Join(dir, "runs", string(run), "journal.jsonl")
}

// seeded writes n good records and returns the file journal and the path.
func seeded(t *testing.T, n int) (wf.Journal, string) {
	t.Helper()
	dir := t.TempDir()
	j := wf.FileJournal(dir)
	defer lock(t, j, "r")()
	for range n {
		if err := j.Append(wf.JournalRecord{Kind: wf.JournalNote, Run: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	return j, journalPath(dir, "r")
}

func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestFileLayoutIsJSONLines(t *testing.T) {
	_, path := seeded(t, 3)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		var r wf.JournalRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Errorf("line %q: %v", l, err)
		}
	}
}

func TestTornTailIsTruncated(t *testing.T) {
	for name, tail := range map[string]string{
		"no newline":     `{"Seq":3,"Kind":21,"Run":"r"}`,
		"partial":        `{"Seq":3,"Ki`,
		"does not parse": "garbage\n",
	} {
		t.Run(name, func(t *testing.T) {
			j, path := seeded(t, 2)
			good, _ := os.ReadFile(path)
			appendRaw(t, path, tail)
			defer lock(t, j, "r")()
			recs, err := j.Records("r")
			if err != nil || len(recs) != 2 {
				t.Fatalf("Records = %d, %v", len(recs), err)
			}
			// A read skips the torn tail and writes nothing; the first
			// append cuts it off.
			if b, _ := os.ReadFile(path); string(b) != string(good)+tail {
				t.Errorf("a read changed the file: %q", b)
			}
			if err := j.Append(wf.JournalRecord{Kind: wf.JournalNote, Run: "r"}); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(path); !strings.HasPrefix(string(b), string(good)) || strings.Count(string(b[len(good):]), "\n") != 1 || !json.Valid(bytes.TrimSpace(b[len(good):])) {
				t.Errorf("tail not truncated before the append: %q", b)
			}
			recs, err = j.Records("r")
			if err != nil || len(recs) != 3 || recs[2].Seq != 3 {
				t.Errorf("after the tail: %+v, %v", recs, err)
			}
		})
	}
}

func TestAppendFirstInACycleTruncatesTheTail(t *testing.T) {
	j, path := seeded(t, 2)
	appendRaw(t, path, `{"Seq":3,"Ki`)
	defer lock(t, j, "r")()
	if err := j.Append(wf.JournalRecord{Kind: wf.JournalNote, Run: "r"}); err != nil {
		t.Fatal(err)
	}
	recs, err := j.Records("r")
	if err != nil || len(recs) != 3 || recs[2].Seq != 3 {
		t.Errorf("Records = %+v, %v", recs, err)
	}
}

func TestBadNonFinalLineIsFatal(t *testing.T) {
	for name, middle := range map[string]string{
		"does not parse": "garbage\n",
		"seq gap":        `{"Seq":9,"Kind":21,"Run":"r"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			j, path := seeded(t, 1)
			appendRaw(t, path, middle+`{"Seq":2,"Kind":21,"Run":"r"}`+"\n")
			before, _ := os.ReadFile(path)
			defer lock(t, j, "r")()
			if _, err := j.Records("r"); !errors.Is(err, wf.ErrCorruptJournal) {
				t.Errorf("Records = %v, want corrupt", err)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Error("a corrupt journal was modified")
			}
		})
	}
}

func TestNewKindsRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	payloads := map[wf.JournalKind]any{
		wf.JournalHandedOut:        wf.HandedOutData{Name: "a#1", Token: "t", Session: "s", Expires: at},
		wf.JournalAttemptStarted:   wf.AttemptStartedData{Name: "a#1", Harness: "h", Session: "s", Agent: "ag", Transcript: "/t.jsonl", Expires: at},
		wf.JournalPromptFetched:    wf.PromptFetchedData{Name: "a#1"},
		wf.JournalHeartbeat:        wf.HeartbeatData{Name: "a#1", Status: "active", By: "ag", Expires: at},
		wf.JournalAttemptLost:      wf.AttemptLostData{Name: "a#1", Cause: wf.LostCauseCap},
		wf.JournalWithdrawn:        wf.WithdrawnData{Name: "a#1", Reason: "left"},
		wf.JournalLateVote:         wf.LateVoteData{Name: "a#1", Output: json.RawMessage(`{"v":1}`)},
		wf.JournalEffectStarted:    wf.EffectStartedData{Intent: 4, Pid: 10, Pgid: 11, Start: at},
		wf.JournalRetried:          wf.RetriedData{Child: "Fan[1]", Reason: "blocked: x"},
		wf.JournalUnitHeld:         wf.UnitHeldData{Name: "a#1", Cause: "blocked: x", Parked: true},
		wf.JournalMetadataAttached: wf.MetadataAttachedData{Name: "a#1", Agent: "ag", State: "done", Late: true},
	}
	for name, j := range journals(t) {
		t.Run(name, func(t *testing.T) {
			defer lock(t, j, "r1")()
			var in []wf.JournalRecord
			for k := wf.JournalHandedOut; k <= wf.JournalMetadataAttached; k++ {
				raw, err := json.Marshal(payloads[k])
				if err != nil {
					t.Fatal(err)
				}
				rec := wf.JournalRecord{Kind: k, Run: "r1", Instance: "i", State: "S", Attempt: 1, At: at, Data: raw}
				if err := j.Append(rec); err != nil {
					t.Fatal(err)
				}
				rec.Seq = int64(len(in) + 1)
				in = append(in, rec)
			}
			got, err := j.Records("r1")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, in) {
				t.Fatalf("records = %+v, want %+v", got, in)
			}
			for _, rec := range got {
				p := reflect.New(wf.JournalPayloads[rec.Kind])
				if err := json.Unmarshal(rec.Data, p.Interface()); err != nil {
					t.Fatalf("%s: %v", rec.Kind, err)
				}
				if !reflect.DeepEqual(p.Elem().Interface(), payloads[rec.Kind]) {
					t.Fatalf("%s payload = %+v, want %+v", rec.Kind, p.Elem().Interface(), payloads[rec.Kind])
				}
			}
		})
	}
}

func TestWatchSignalsOnAppend(t *testing.T) {
	// The file journal polls; shorten it. No test in the package runs in
	// parallel, so the package variable is not shared.
	const poll = 20 * time.Millisecond
	defer wf.SetFileWatchPoll(poll)()
	for name, j := range journals(t) {
		t.Run(name, func(t *testing.T) {
			stop := make(chan struct{})
			defer close(stop)
			ch := j.Watch("w1", stop)
			select {
			case <-ch:
				t.Fatal("signal before any append")
			case <-time.After(50 * time.Millisecond):
			}
			go func() {
				unlock, err := j.Lock("w1")
				if err != nil {
					t.Error(err)
					return
				}
				defer unlock()
				if err := j.Append(wf.JournalRecord{Kind: wf.JournalRunStarted, Run: "w1"}); err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
				t.Fatal("no signal within 2s of an append")
			}
			select {
			case <-ch:
				t.Fatal("a second signal for one append")
			case <-time.After(3 * poll):
			}
		})
	}
}

// Runs lists a run only when Records can read a first record from it: a run
// whose only line is torn is not listed.
func TestRunsSkipsARunWithATornFirstRecord(t *testing.T) {
	dir := t.TempDir()
	for run, content := range map[string]string{
		"torn":       `{"Seq":1,"Kind":21`,
		"tornline":   "{\"Seq\":1,\"Ki\n",
		"committed":  "{\"Seq\":1,\"Kind\":21,\"Run\":\"committed\"}\n{\"Seq\":2,\"Ki",
		"corruptmid": "garbage\n{\"Seq\":2,\"Kind\":21,\"Run\":\"corruptmid\"}\n",
		// Valid JSON that is no record: as torn as a cut line.
		"notrecord": "{\"Seq\":\"partial\"}\n",
	} {
		if err := os.MkdirAll(filepath.Join(dir, "runs", run), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "runs", run, "journal.jsonl"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := wf.FileJournal(dir).Runs()
	if err != nil {
		t.Fatal(err)
	}
	if want := []wf.RunID{"committed", "corruptmid"}; !slices.Equal(got, want) {
		t.Errorf("Runs = %v, want %v", got, want)
	}
}
