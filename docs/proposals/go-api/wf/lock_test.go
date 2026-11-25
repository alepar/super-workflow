package wf

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

type (
	lkAsk    struct{}
	lkDone   struct{}
	lkGaveUp struct{}
	lkAnswer struct{ OK bool }
)

func lkDef() *Def {
	d := New("lock-cli")
	a := NewAgent[lkAsk, lkAnswer](d)
	done := NewTerminal[lkDone](d, Success)
	gaveUp := NewTerminal[lkGaveUp](d, Stopped)
	d.OnFailure(Blocked, gaveUp)
	a.Spec(PromptText("Say hello."), Retry[lkAsk](Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(lkAnswer, Ctx) Edge[lkAsk] { return next }, next)
	return d
}

func TestHeldLockExitsSixNamingHolder(t *testing.T) {
	old := fileLockWait
	fileLockWait = 200 * time.Millisecond
	t.Cleanup(func() { fileLockWait = old })
	noHarnessEnv(t) // a harness's tool limit would set the wait instead

	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	dir := filepath.Join(t.TempDir(), "state")
	call := func(args ...string) (int, string) {
		var o, e bytes.Buffer
		code := CLI(cfg, args, &o, &e)
		return code, e.String()
	}
	var o, e bytes.Buffer
	if code := CLI(cfg, []string{"run", "lock-cli", "--state-dir", dir, "--workspace", t.TempDir(), "--run-id", "lock-cli-1"}, &o, &e); code != 0 {
		t.Fatalf("run exited %d: %s", code, e.String())
	}
	run := strings.TrimSpace(o.String())

	// Another process holds the lock and writes nothing (flock(1)): the
	// record names run, its last writer, which is not the holder.
	holder, unlock := flockProcess(t, filepath.Join(dir, "runs", run, "lock"))
	start := time.Now()
	code, stderr := call("next", run, "all", "--state-dir", dir, "--json")
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("next took %v with the lock held, want within the bounded retry", took)
	}
	if code != exitBusy {
		t.Fatalf("held lock: exit %d, want %d; stderr: %s", code, exitBusy, stderr)
	}
	if want := fmt.Sprintf("next: the run is busy, held by pid %d (last written by run, pid %d, since ", holder, os.Getpid()); !strings.Contains(stderr, want) {
		t.Errorf("stderr %q lacks %q", stderr, want)
	}

	unlock()
	if code, stderr := call("next", run, "all", "--state-dir", dir, "--json"); code != 0 {
		t.Errorf("after release: exit %d: %s", code, stderr)
	}
}

func TestLockRecordsHolder(t *testing.T) {
	dir := t.TempDir()
	first := FileJournal(dir).(*fileJournal)
	unlock, err := first.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	// The record is written by the first append, not by taking the lock.
	if data, _ := os.ReadFile(filepath.Join(dir, "runs", "r", "lock")); len(data) != 0 {
		t.Errorf("taking the lock wrote %q before any append", data)
	}
	if err := first.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "runs", "r", "lock"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`"pid":%d`, os.Getpid()); !strings.Contains(string(data), want) {
		t.Errorf("lock file %q lacks %s", data, want)
	}
	if _, ok := first.lastHolder("r"); ok {
		t.Error("first locker has a previous holder")
	}
	unlock()

	second := FileJournal(dir).(*fileJournal)
	unlock, err = second.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	h, ok := second.lastHolder("r")
	if !ok || h.Pid != os.Getpid() || h.Since.IsZero() {
		t.Errorf("lastHolder = %+v, %v; want this pid with a start time", h, ok)
	}
}

// A verb's lock waits end together at its process's deadline, the caller's
// tool limit less a margin, so a busy run is reported (exit 9)
// before the harness cuts the call off; the per-acquisition wait does not
// stretch past it (FANOUT-2 run B).
func TestLockWaitEndsAtDeadline(t *testing.T) {
	dir := t.TempDir()
	unlock, err := FileJournal(dir).Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	j := FileJournal(dir)
	setLockDeadline(j, time.Now().Add(300*time.Millisecond))
	start := time.Now()
	if _, err := j.Lock("r"); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("held lock: %v", err)
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 2*time.Second {
		t.Errorf("first wait took %v, want about the 300ms to the deadline", took)
	}
	start = time.Now()
	if _, err := j.Lock("r"); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("held lock past the deadline: %v", err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("a wait past the deadline took %v, want one try", took)
	}
}

func TestCLILockDeadlineFollowsToolLimit(t *testing.T) {
	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	dir := filepath.Join(t.TempDir(), "state")
	var o, e bytes.Buffer
	if code := CLI(cfg, []string{"run", "lock-cli", "--state-dir", dir, "--workspace", t.TempDir(), "--run-id", "lock-cli-2"}, &o, &e); code != 0 {
		t.Fatalf("run exited %d: %s", code, e.String())
	}
	run := strings.TrimSpace(o.String())
	unlock, err := FileJournal(dir).Lock(RunID(run))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// Claude Code's Bash cut off at 3 s: the verb gives up at 1.5 s.
	noHarnessEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s")
	t.Setenv("BASH_DEFAULT_TIMEOUT_MS", "3000")
	start := time.Now()
	o.Reset()
	e.Reset()
	// retry writes the run: it waits for the lock (status reads lock-free).
	code := CLI(cfg, []string{"retry", run, "--state-dir", dir}, &o, &e)
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("retry took %v with the lock held, want under the 3s tool limit", took)
	}
	// A busy run is not a stop: nothing was changed, and the same command
	// is run again.
	if code != exitBusy || !strings.Contains(e.String(), "retry: the run is busy, held by ") || !strings.Contains(e.String(), BusyRunTail) || strings.Contains(e.String(), "do not change any files") {
		t.Errorf("held lock: exit %d: %s", code, e.String())
	}
}

// harnessEnvVars are the variables a harness's shell sets that the engine
// reads to detect or classify a caller, or to size a call: every registered
// adapter's markers (harness.Detector), every ANTIGRAVITY_* variable in the
// environment (agy's records root is ANTIGRAVITY_APP_DATA_DIR), and the
// non-marker ones the engine reads beside them. Derived from the registry,
// so a new adapter's markers are cleared without editing a list.
func harnessEnvVars() []string {
	vars := []string{"CODEX_SESSION_ID", "BASH_DEFAULT_TIMEOUT_MS", "TERMINAL_TIMEOUT", "HERMES_DELEGATED_CHILD_CONTEXT", "HERMES_KANBAN_TASK", "OPENCODE_PARENT_SESSION_ID", "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS"}
	for _, n := range harness.Names() {
		if d, ok := harness.For(n).(harness.Detector); ok {
			vars = append(vars, d.Markers()...)
		}
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ANTIGRAVITY_") {
			vars = append(vars, k)
		}
	}
	return vars
}

// noHarnessEnv clears the variables that name the test process's own agent
// harness, so a test's CLI call runs as from a person's shell.
func noHarnessEnv(t *testing.T) {
	t.Helper()
	for _, k := range harnessEnvVars() {
		t.Setenv(k, "")
	}
}

// A journal value reads the lines appended since its last read, by itself
// or by another process, and reads the whole file again when the file is
// not the one it read: replaced, shortened or rewritten in place.
func TestJournalCacheFollowsTheFile(t *testing.T) {
	dir := t.TempDir()
	reader, writer := FileJournal(dir), FileJournal(dir)
	appendOne := func(j Journal, note string) {
		t.Helper()
		unlock, err := j.Lock("r")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Roots: &PathRoots{Workspace: "/ws"}, Data: []byte(`{"Text":"` + note + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []JournalRecord {
		t.Helper()
		unlock, err := reader.Lock("r")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		recs, err := reader.Records("r")
		if err != nil {
			t.Fatal(err)
		}
		return recs
	}
	appendOne(writer, "a")
	appendOne(writer, "b")
	if got := read(); len(got) != 2 {
		t.Fatalf("first read: %d records", len(got))
	}
	appendOne(writer, "c")
	appendOne(reader, "d")
	got := read()
	if len(got) != 4 || string(got[2].Data) != `{"Text":"c"}` || got[3].Seq != 4 {
		t.Fatalf("after appends: %+v", got)
	}
	got[0].Seq = 99 // a caller's copy, not the cache
	got[0].Data[2] = 'X'
	got[0].Roots.Workspace = "/elsewhere"
	if again := read(); again[0].Seq != 1 || string(again[0].Data) != `{"Text":"a"}` || again[0].Roots.Workspace != "/ws" {
		t.Fatal("caller mutated the cache")
	}
	// Far more than any edge window: a rewrite of record 1 is still seen.
	for i := 0; i < 200; i++ {
		appendOne(writer, strings.Repeat("p", 100))
	}
	if n := len(read()); n != 204 {
		t.Fatalf("after many appends: %d records", n)
	}
	big := filepath.Join(dir, "runs", "r", journalFile)
	whole, _ := os.ReadFile(big)
	if err := os.WriteFile(big, bytes.Replace(whole, []byte(`"Text":"a"`), []byte(`"Text":"q"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(); string(got[0].Data) != `{"Text":"q"}` {
		t.Fatalf("rewrite of record 1 in a large journal not seen: %s", got[0].Data)
	}
	// Truncated and regrown to its old length with its old tail bytes: the
	// middle changed, and the read must say so.
	lines204 := bytes.SplitAfter(whole, []byte("\n"))
	regrown := bytes.Join(lines204[:2], nil)
	regrown = append(regrown, bytes.Replace(bytes.Join(lines204[2:], nil), []byte(`"Text":"c"`), []byte(`"Text":"k"`), 1)...)
	if err := os.WriteFile(big, regrown, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(); string(got[2].Data) != `{"Text":"k"}` {
		t.Fatalf("regrown journal served the old middle: %s", got[2].Data)
	}
	if err := os.WriteFile(big, whole[:len(bytes.Join(lines204[:4], nil))], 0o644); err != nil {
		t.Fatal(err)
	}
	// Rewritten in place, same length, different bytes.
	path := filepath.Join(dir, "runs", "r", journalFile)
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(b, []byte(`"a"`), []byte(`"z"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 4 || string(got[0].Data) != `{"Text":"z"}` {
		t.Fatalf("after a rewrite: %+v", got[0])
	}
	// Shortened to its first two lines.
	lines := bytes.SplitAfter(b, []byte("\n"))
	if err := os.WriteFile(path, bytes.Join(lines[:2], nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 2 {
		t.Fatalf("after shortening: %d records", len(got))
	}
	// A torn final line past the cached end is dropped by a read, which
	// leaves the file as it is; the next append cuts it off.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"Seq":3,"Kind":21`)
	f.Close()
	if got := read(); len(got) != 2 {
		t.Fatalf("torn tail: %d records", len(got))
	}
	if fi, _ := os.Stat(path); fi.Size() != int64(len(bytes.Join(lines[:2], nil)))+int64(len(`{"Seq":3,"Kind":21`)) {
		t.Fatalf("a read changed the file: %d bytes", fi.Size())
	}
	appendOne(writer, "after-torn")
	if got := read(); len(got) != 3 {
		t.Fatalf("after an append past a torn tail: %d records", len(got))
	}
}

// lockedRun starts a lock-cli run and holds its lock until the test ends.
func lockedRun(t *testing.T, cfg MainConfig, id string) (dir, run string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "state")
	var o, e bytes.Buffer
	if code := CLI(cfg, []string{"run", "lock-cli", "--state-dir", dir, "--workspace", t.TempDir(), "--run-id", id}, &o, &e); code != 0 {
		t.Fatalf("run exited %d: %s", code, e.String())
	}
	run = strings.TrimSpace(o.String())
	unlock, err := FileJournal(dir).Lock(RunID(run))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	return dir, run
}

// The blocking next under a held lock answers within its tool limit, its
// preflight's lock wait included: the loop's nothing-yet line, not a kill.
// An explicit --timeout bounds it the same way where no harness limit is
// known.
func TestNextUnderContentionAnswersWithinItsBudget(t *testing.T) {
	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	for _, c := range []struct {
		name  string
		env   map[string]string
		args  []string
		limit time.Duration
	}{
		{"claude, 3 s Bash", map[string]string{"CLAUDE_CODE_SESSION_ID": "s", "BASH_DEFAULT_TIMEOUT_MS": "3000"}, nil, 3 * time.Second},
		{"no harness, --timeout shorter than a lock wait", nil, []string{"--timeout", "300ms"}, 2 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			noHarnessEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			dir, run := lockedRun(t, cfg, "lock-next")
			var o, e bytes.Buffer
			start := time.Now()
			code := CLI(cfg, append([]string{"next", run, "--state-dir", dir}, c.args...), &o, &e)
			if took := time.Since(start); took > c.limit {
				t.Errorf("next took %v under a held lock, want under %v", took, c.limit)
			}
			if code != exitOK || !strings.Contains(o.String(), "nothing yet after") {
				t.Errorf("next under a held lock: exit %d, %q %q", code, o.String(), e.String())
			}
		})
	}
}

// The journal view and a run's resume discovery wait within the same budget.
func TestJournalAndRunDiscoveryWaitWithinTheBudget(t *testing.T) {
	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	noHarnessEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s")
	t.Setenv("BASH_DEFAULT_TIMEOUT_MS", "3000")
	dir, run := lockedRun(t, cfg, "lock-views")
	// The journal view reads the committed records lock-free: it answers at
	// once under a held lock.
	var jo, je bytes.Buffer
	start := time.Now()
	if code := CLI(cfg, []string{"journal", run, "--state-dir", dir}, &jo, &je); code != 0 || time.Since(start) > time.Second || !strings.Contains(jo.String(), "run_started") {
		t.Errorf("journal under a held lock: exit %d after %v: %q %q", code, time.Since(start), jo.String(), je.String())
	}
	for _, args := range [][]string{
		{"run", "lock-cli", "--state-dir", dir, "--run-id", run},
	} {
		var o, e bytes.Buffer
		start := time.Now()
		code := CLI(cfg, args, &o, &e)
		if took := time.Since(start); took > 2500*time.Millisecond {
			t.Errorf("%v took %v under a held lock, want under the 3 s tool limit", args[0], took)
		}
		if code != exitBusy || !strings.Contains(e.String(), args[0]+": the run is busy, held by ") || !strings.Contains(e.String(), BusyRunTail) {
			t.Errorf("%v under a held lock: exit %d, %q", args[0], code, e.String())
		}
	}
}

// A next whose preflight passed and whose run is then locked to the end of
// its budget, through the rendering, still answers nothing-yet in time; a
// bind of --until-unit that waits for the lock leaves next only the rest of
// the budget; under contention a bad argument is still refused.
func TestNextContentionAfterPreflight(t *testing.T) {
	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	noHarnessEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s")
	t.Setenv("BASH_DEFAULT_TIMEOUT_MS", "3000") // budget 1.5 s
	call := func(dir string, args ...string) (int, string, time.Duration) {
		var o, e bytes.Buffer
		start := time.Now()
		code := CLI(cfg, append(args, "--state-dir", dir), &o, &e)
		return code, o.String() + e.String(), time.Since(start)
	}
	newRun := func(id string) (string, string) {
		dir := filepath.Join(t.TempDir(), "state")
		var o, e bytes.Buffer
		if code := CLI(cfg, []string{"run", "lock-cli", "--state-dir", dir, "--workspace", t.TempDir(), "--run-id", id}, &o, &e); code != 0 {
			t.Fatalf("run exited %d: %s", code, e.String())
		}
		return dir, strings.TrimSpace(o.String())
	}
	hold := func(dir, run string, release time.Duration) {
		afterPreflight = func(cmd string) {
			if cmd != "next" {
				return
			}
			afterPreflight = nil
			unlock, err := FileJournal(dir).Lock(RunID(run))
			if err != nil {
				t.Error(err)
				return
			}
			if release > 0 {
				time.AfterFunc(release, unlock)
			} else {
				t.Cleanup(unlock)
			}
		}
		t.Cleanup(func() { afterPreflight = nil })
	}

	dir, run := newRun("after-preflight")
	hold(dir, run, 0)
	if code, out, took := call(dir, "next", run); code != exitOK || !strings.Contains(out, "nothing yet after") || took > 2500*time.Millisecond {
		t.Errorf("locked after preflight: exit %d in %v: %q", code, took, out)
	}

	// The bound unit is already handed out and claimed, and quiet: next has
	// nothing to answer but nothing-yet, at the end of what the bind left.
	dir, run = newRun("late-bind")
	if code, out, _ := call(dir, "next", run, "all", "--json"); code != exitOK {
		t.Fatalf("hand-out: %d %s", code, out)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "") // a person's shell claims it
	t.Setenv("BASH_DEFAULT_TIMEOUT_MS", "")
	if code, out, _ := call(dir, "dev", "started", run, "lkAsk#1", "--session", "s", "--agent", "kid"); code != exitOK {
		t.Fatalf("started: %d %s", code, out)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s")
	t.Setenv("BASH_DEFAULT_TIMEOUT_MS", "3000")
	hold(dir, run, time.Second)
	if code, out, took := call(dir, "next", run, "--until-unit", "lkAsk#1"); code != exitOK || !strings.Contains(out, "nothing yet after") || took < time.Second || took > 1900*time.Millisecond {
		t.Errorf("bind after a 1 s wait: exit %d in %v, want nothing-yet within the 1.5 s budget: %q", code, took, out)
	}

	dir, run = newRun("bad-args")
	unlock, err := FileJournal(dir).Lock(RunID(run))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, args := range [][]string{{"next", run, "bogus"}, {"next", run, "--timeout=-1s"}, {"next", run, "1", "--json", "--timeout=500ms"}, {"next", run, "1", "--json", "--timeout=10m"}} {
		if code, out, _ := call(dir, args...); code != exitUsage {
			t.Errorf("%v under contention: exit %d, %q; want a usage refusal", args[1:], code, out)
		}
	}
}

// Two run-data states with the same paths, sizes, modes and mtimes but
// different bytes digest apart: a reuse never names a capture of other bytes.
func TestRunDataDigestSeesSameStampDifferentBytes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "records", "f")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	before, _ := runDataDigest(dir)
	if err := os.WriteFile(p, []byte("cccc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after, _ := runDataDigest(dir); after == before {
		t.Error("same stamp, different bytes: digests equal")
	}
}

// The lock record names the verb and the unit or session it ran for, and the
// holder line gives a pid a person on this host can use: a holder in another
// pid namespace (Codex runs its commands as pid 2 in one, an earlier revision) is shown
// with its host pid when this process can see it.
func TestLockHolderNamesVerbUnitAndHostPid(t *testing.T) {
	dir := t.TempDir()
	j := FileJournal(dir)
	setLockOwner(j, lockOwner{Verb: "done", Unit: "Code/Round/x/Implement#1"}, nil, nil)
	unlock, err := j.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	waiter := FileJournal(dir)
	setLockDeadline(waiter, time.Now().Add(100*time.Millisecond))
	_, err = waiter.Lock("r")
	unlock()
	if want := fmt.Sprintf("run locked: held by pid %d (last written by done for unit Code/Round/x/Implement#1, pid %d, since ", os.Getpid(), os.Getpid()); err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("busy lock: %v, want prefix %q", err, want)
	}

	// A session, when the verb names no unit.
	if got := (LockHolder{Pid: 7, Verb: "next", Session: "s1"}).describe(); !strings.HasPrefix(got, "next for session s1, pid 7, since ") {
		t.Errorf("session holder: %q", got)
	}
	// Another namespace this process cannot map.
	if got := (LockHolder{Pid: 2, Verb: "prompt", PidNS: 1}).describe(); !strings.Contains(got, "pid 2 in a sandbox's pid namespace (its host pid is not visible from here)") {
		t.Errorf("unmapped namespace: %q", got)
	}
	// Another host: its namespace and pid mean nothing here, so they are
	// never mapped against this host's processes.
	if got := (LockHolder{Pid: 2, Host: "elsewhere.invalid", PidNS: pidNamespace("self") + 1}).describe(); !strings.Contains(got, "pid 2 on host elsewhere.invalid") || strings.Contains(got, "sandbox") {
		t.Errorf("other host: %q", got)
	}

	// Long names are cut so the encoded record stays within what readHolder
	// reads, JSON escaping included: an ampersand is six bytes encoded.
	long := strings.Repeat("ü", 600)
	setLockOwner(j, lockOwner{Verb: "done", Unit: long, Session: strings.Repeat("&", 200)}, nil, nil)
	unlock, err = j.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	unlock()
	f, err := os.Open(filepath.Join(dir, "runs", "r", "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if raw, _ := os.ReadFile(filepath.Join(dir, "runs", "r", "lock")); len(raw) > holderRecordMax {
		t.Errorf("the encoded holder record is %d bytes, over %d", len(raw), holderRecordMax)
	}
	h, ok := readHolder(f)
	if !ok || h.Verb != "done" || !strings.HasSuffix(h.Unit, "…") || !utf8.ValidString(h.Unit) || !strings.HasPrefix(h.Session, "&&&") {
		t.Errorf("long holder record: ok=%v verb=%q unit=%d bytes session=%d bytes", ok, h.Verb, len(h.Unit), len(h.Session))
	}
}

// A process in its own pid namespace is pid 1 there; the holder line maps it
// back to its pid on this host.
func TestLockHolderMapsASandboxPidToTheHost(t *testing.T) {
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	cmd := exec.Command("unshare", "-Urpf", "--mount-proc", "sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start a pid namespace:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	// unshare forks sleep into the new namespace; find it as the child whose
	// namespace differs from ours.
	var ns uint64
	var host int
	for start := time.Now(); time.Since(start) < 3*time.Second && ns == 0; time.Sleep(20 * time.Millisecond) {
		kids, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", cmd.Process.Pid, cmd.Process.Pid))
		for _, k := range strings.Fields(string(kids)) {
			if n := pidNamespace(k); n != 0 && n != pidNamespace("self") {
				ns = n
				host, _ = strconv.Atoi(k)
			}
		}
	}
	if ns == 0 {
		t.Skip("the namespaced child never appeared")
	}
	if got := hostPid(ns, 1); got != host {
		t.Fatalf("hostPid(ns, 1) = %d, want %d", got, host)
	}
	got := (LockHolder{Pid: 1, Verb: "prompt", Unit: "u", PidNS: ns}).describe()
	if want := fmt.Sprintf("prompt for unit u, pid 1 in a sandbox (host pid %d), since ", host); !strings.HasPrefix(got, want) {
		t.Errorf("describe = %q, want prefix %q", got, want)
	}
}

// A busy report from a call that wrote two runs (a rewind writes its fork,
// not its source) names each run's records and the status command of the
// last run it wrote.
func TestBusyAfterRecordingNamesEachRunItWrote(t *testing.T) {
	var er bytes.Buffer
	c := &cli{stderr: &er, verbLine: func(run RunID, verb string, args ...string) string {
		return "workflow " + verb + " " + string(run)
	}}
	c.committed = []committedRecord{{Run: "src", Kind: JournalNote}, {Run: "src.rw1", Kind: JournalRunStarted}, {Run: "src.rw1", Kind: JournalRewound}}
	if code := c.busy("dev rewind", fmt.Errorf("%w: held by pid 7", ErrRunLocked)); code != exitInfra {
		t.Errorf("exit %d, want %d", code, exitInfra)
	}
	for _, want := range []string{"note for run src; and run_started, rewound for run src.rw1, so it did not finish", "see where run src.rw1 stands", "workflow status src.rw1"} {
		if !strings.Contains(er.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, er.String())
		}
	}
}

// flockProcess holds an exclusive flock on path from another process,
// flock(1), until the returned func ends it; pid is that process.
func flockProcess(t *testing.T, path string) (pid int, unlock func()) {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock(1)")
	}
	cmd := exec.Command("flock", "--exclusive", path, "sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // sleep inherits the lock: end the group
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	unlock = func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }
	t.Cleanup(unlock)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(10 * time.Millisecond) {
		if p, ok := flockHolder(f); ok {
			return p, unlock
		}
	}
	t.Fatal("flock(1) never took the lock")
	return 0, nil
}

// The holder line names the record only when the kernel shows its writer
// holding the lock; a lock nobody's record names is attributed honestly.
func TestHeldByAttributesOnlyTheProvenHolder(t *testing.T) {
	dir := t.TempDir()
	j := FileJournal(dir)
	setLockOwner(j, lockOwner{Verb: "done", Unit: "u"}, nil, nil)
	unlock, err := j.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runs", "r", "lock")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got, want := heldBy(f), fmt.Sprintf("pid %d (last written by done for unit u, pid %d, since ", os.Getpid(), os.Getpid()); !strings.HasPrefix(got, want) {
		t.Errorf("the writer holding the lock: %q, want prefix %q", got, want)
	}
	unlock()
	pid, release := flockProcess(t, path)
	if got, want := heldBy(f), fmt.Sprintf("pid %d (last written by done for unit u, pid %d, since ", pid, os.Getpid()); !strings.HasPrefix(got, want) {
		t.Errorf("another holder: %q, want prefix %q", got, want)
	}
	release()

	// One process, two acquisitions: "first" locks, appends
	// and lets go; "second" locks and appends nothing. A matching pid proves
	// the process, not the acquisition: the line never names "first" as the
	// holder, only as the last writer.
	first := FileJournal(dir)
	setLockOwner(first, lockOwner{Verb: "first"}, nil, nil)
	unlockFirst, err := first.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	unlockFirst()
	second := FileJournal(dir)
	setLockOwner(second, lockOwner{Verb: "second"}, nil, nil)
	unlockSecond, err := second.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	defer unlockSecond()
	got := heldBy(f)
	if want := fmt.Sprintf("pid %d (last written by first, pid %d, since ", os.Getpid(), os.Getpid()); !strings.HasPrefix(got, want) {
		t.Errorf("second's hold: %q, want prefix %q", got, want)
	}
}

// The CLI's journal peek reads the same committed records as Records: a
// malformed final line is a torn write, and a bad line before the last
// leaves nothing to peek at.
func TestPeekRecordsSharesTheCommittedPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	good := "{\"Seq\":1,\"Kind\":21,\"Run\":\"r\"}\n{\"Seq\":2,\"Kind\":21,\"Run\":\"r\"}\n"
	for name, c := range map[string]struct {
		content string
		want    int
	}{
		"malformed last line": {good + "garbage\n", 2},
		"torn last line":      {good + `{"Seq":3`, 2},
		"bad line before":     {"garbage\n" + good, 0},
	} {
		if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := len(peekRecords(path)); got != c.want {
			t.Errorf("%s: peekRecords = %d records, want %d", name, got, c.want)
		}
	}
}

// One journal value, two Locks of a run: the first lets the lock go to the
// second and cannot take it back in time. Its release must not let go of
// the second's lock: a third caller still finds the run held.
func TestYieldReleasesOnlyItsOwnLease(t *testing.T) {
	dir := t.TempDir()
	j := FileJournal(dir).(*fileJournal)
	unlockA, err := j.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	// The run's first append makes its turnstile gate.
	if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	gotB := make(chan func())
	go func() {
		unlockB, err := j.Lock("r")
		if err != nil {
			t.Error(err)
		}
		gotB <- unlockB
	}()
	for start := time.Now(); !j.othersWaiting("r"); time.Sleep(5 * time.Millisecond) {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the second Lock never waited")
		}
	}
	j.until = time.Now().Add(200 * time.Millisecond) // A's take-back gives up; B's wait began without it
	if err := j.yieldLock("r"); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("A took the lock back while B held it: %v", err)
	}
	unlockB := <-gotB
	unlockA()
	third := FileJournal(dir)
	setLockDeadline(third, time.Now().Add(200*time.Millisecond))
	if _, err := third.Lock("r"); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("a third caller took the run while B held it: %v", err)
	}
	unlockB()
	unlockC, err := FileJournal(dir).Lock("r")
	if err != nil {
		t.Fatalf("after B let go: %v", err)
	}
	unlockC()
}
