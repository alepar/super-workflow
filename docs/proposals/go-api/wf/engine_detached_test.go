package wf_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	dRun  struct{}
	dOK   struct{}
	dFail struct{}
)

type dExit int

// detRunner is a scripted wf.EffectRunner that obeys the shell runner's group
// rule: per intent a leader (alive until set otherwise) and a member count.
// Status is Running while the leader lives, DeadLive while it is gone and
// members remain, Dead when both are gone; Terminate kills leader and members
// of a non-empty group unless the intent is stubborn; GroupEmpty is true with
// neither.
type detRunner struct {
	mu         sync.Mutex
	started    []wf.DetachedRequest
	pidIntent  map[int]int64
	procOf     map[string]wf.DetachedProcess
	hideProc   map[int64]bool
	present    map[int64]wf.RunResult
	leaderGone map[int64]bool
	members    map[int64]int
	stubborn   map[int64]bool
	reason     map[int64]string
	statusAsk  []wf.DetachedProcess
	terminated []int64
	startErr   error
	statusErr  error // every Status fails with it while set
}

func newDetRunner() *detRunner {
	return &detRunner{pidIntent: map[int]int64{}, procOf: map[string]wf.DetachedProcess{}, hideProc: map[int64]bool{}, present: map[int64]wf.RunResult{},
		leaderGone: map[int64]bool{}, members: map[int64]int{}, stubborn: map[int64]bool{}, reason: map[int64]string{}}
}

func intentOf(path string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), 10, 64)
	return n
}

func (r *detRunner) Start(req wf.DetachedRequest) (wf.DetachedProcess, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return wf.DetachedProcess{}, r.startErr
	}
	r.started = append(r.started, req)
	pid := 100 + len(r.started) - 1
	r.pidIntent[pid] = intentOf(req.ResultPath)
	p := wf.DetachedProcess{Pid: pid, Pgid: pid, StartTicks: uint64(5000 + pid)}
	r.procOf[req.ProcPath] = p
	return p, nil
}

func (r *detRunner) Launched(req wf.DetachedRequest) (wf.DetachedProcess, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.procOf[req.ProcPath]
	if !ok || r.hideProc[intentOf(req.ProcPath)] {
		return wf.DetachedProcess{}, false, nil
	}
	return p, true, nil
}

// Status follows the group rule for the intent of req. A zero process (no proc
// record) is asked about too, as the engine does.
func (r *detRunner) Status(p wf.DetachedProcess, req wf.DetachedRequest) (wf.DetachedReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusAsk = append(r.statusAsk, p)
	if r.statusErr != nil {
		return wf.DetachedReport{}, r.statusErr
	}
	i := intentOf(req.ResultPath)
	if res, ok := r.present[i]; ok {
		return wf.DetachedReport{Status: wf.DetachedPresent, Result: res}, nil
	}
	switch {
	case !r.leaderGone[i]:
		return wf.DetachedReport{Status: wf.DetachedRunning}, nil
	case r.members[i] > 0:
		return wf.DetachedReport{Status: wf.DetachedDeadLive, Reason: r.reason[i]}, nil
	}
	return wf.DetachedReport{Status: wf.DetachedDead, Reason: r.reason[i]}, nil
}

func (r *detRunner) Terminate(p wf.DetachedProcess, _ wf.DetachedRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.pidIntent[p.Pid]
	r.terminated = append(r.terminated, i)
	if (!r.leaderGone[i] || r.members[i] > 0) && !r.stubborn[i] {
		r.leaderGone[i], r.members[i] = true, 0
	}
	return nil
}

func (r *detRunner) GroupEmpty(p wf.DetachedProcess, _ wf.DetachedRequest) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.pidIntent[p.Pid]
	return r.leaderGone[i] && r.members[i] == 0, nil
}

// set scripts the launch of intent: Present gives a result file; Dead and
// DeadLive are a gone leader with no members or one live member.
func (r *detRunner) set(intent int64, st wf.DetachedStatus, rr wf.RunResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch st {
	case wf.DetachedPresent:
		r.present[intent] = rr
	case wf.DetachedDead:
		r.leaderGone[intent], r.members[intent] = true, 0
	case wf.DetachedDeadLive:
		r.leaderGone[intent], r.members[intent] = true, 1
	}
}

func (r *detRunner) setMembers(intent int64, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.members[intent] = n
}

// setStubborn makes Terminate leave the group of intent as it is.
func (r *detRunner) setStubborn(intent int64, v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stubborn[intent] = v
}

func (r *detRunner) setHideProc(intent int64, v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hideProc[intent] = v
}

func (r *detRunner) setReason(intent int64, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reason[intent] = why
}

func (r *detRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

func (r *detRunner) request(i int) wf.DetachedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started[i]
}

func (r *detRunner) terminations() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.terminated...)
}

// detDef is one keyed command run as make {topic}: exit 0 passes, 1, 124 and
// 125 fail. mod shapes the command (Detached, Idempotent, DoneWhen).
func detDef(mod func(*wf.Command[dRun])) *wf.Def {
	d := wf.New("detached")
	d.Param("topic")
	exits := wf.NewEnum(d, "d-exit", dExit(0), dExit(1), dExit(124), dExit(125))
	cmd := wf.NewCommand[dRun](d, "make", "{topic}")
	ok := wf.NewTerminal[dOK](d, wf.Success)
	fail := wf.NewTerminal[dFail](d, wf.Stopped)
	cmd.Key("build", "{topic}")
	mod(cmd)
	cmd.RouteCases(wf.Cases[dRun, wf.RunResult](wf.ExitAs[dExit], exits,
		wf.On(dExit(0), cmd.To(ok, "passes")),
		wf.On(dExit(1), cmd.To(fail, "fails")),
		wf.On(dExit(124), cmd.To(fail, "timed out")),
		wf.On(dExit(125), cmd.To(fail, "runner failed"))))
	return d
}

func idempotentDetached(max ...time.Duration) func(*wf.Command[dRun]) {
	return func(c *wf.Command[dRun]) { c.Idempotent().Detached(max...) }
}

func detOpts(t *testing.T, fr wf.EffectRunner) wf.EngineOptions {
	dir := t.TempDir()
	return wf.EngineOptions{EffectRunner: fr, RunDir: func(wf.RunID) string { return dir }}
}

// detStart runs the definition, launches its command and returns the harness,
// the run and the intent seq.
func detStart(t *testing.T, d *wf.Def, fr *detRunner, w *world) (*harness, wf.RunID, int64) {
	t.Helper()
	h := newHarnessOpts(t, d, w, detOpts(t, fr))
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	intents := h.kind(run, wf.JournalEffectIntent)
	if len(intents) != 1 {
		t.Fatalf("effect_intent records = %d, want 1", len(intents))
	}
	return h, run, intents[0].Seq
}

// intentsOf are the effect_intent payloads of the run in journal order, with
// their seqs.
func intentsOf(t *testing.T, h *harness, run wf.RunID) (seqs []int64, data []wf.EffectIntentData) {
	t.Helper()
	for _, r := range h.kind(run, wf.JournalEffectIntent) {
		var d wf.EffectIntentData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		seqs, data = append(seqs, r.Seq), append(data, d)
	}
	return seqs, data
}

// lastIntent is the seq of the newest effect_intent.
func lastIntent(t *testing.T, h *harness, run wf.RunID) int64 {
	t.Helper()
	seqs, _ := intentsOf(t, h, run)
	return seqs[len(seqs)-1]
}

// dropStarted is a journal that fails the append of the nth effect_started, the
// way a crash between Start and the journal write loses it.
type dropStarted struct {
	wf.Journal
	drop int
}

func (j *dropStarted) Append(r wf.JournalRecord) error {
	if r.Kind == wf.JournalEffectStarted && j.drop > 0 {
		j.drop--
		return errors.New("crash before effect_started")
	}
	return j.Journal.Append(r)
}

// withDroppedStarted makes h's next effect_started append fail.
func withDroppedStarted(t *testing.T, h *harness, fr *detRunner, d *wf.Def) *dropStarted {
	t.Helper()
	j := &dropStarted{Journal: h.j, drop: 1}
	opts := detOpts(t, fr)
	opts.Clock, opts.Defs = func() time.Time { return h.now }, []*wf.Def{d}
	h.e = wf.NewEngine(j, h.reg, opts)
	return j
}

func onlyInstruction(t *testing.T, ins []wf.Instruction, kind wf.InstructionKind) wf.Instruction {
	t.Helper()
	if len(ins) != 1 || ins[0].Kind != kind {
		t.Fatalf("instructions = %+v, want exactly one %q", ins, kind)
	}
	return ins[0]
}

func resultsOf(t *testing.T, h *harness, run wf.RunID) []wf.EffectResultData {
	t.Helper()
	var out []wf.EffectResultData
	for _, r := range h.kind(run, wf.JournalEffectResult) {
		var d wf.EffectResultData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func startedOf(t *testing.T, h *harness, run wf.RunID) []wf.EffectStartedData {
	t.Helper()
	var out []wf.EffectStartedData
	for _, r := range h.kind(run, wf.JournalEffectStarted) {
		var d wf.EffectStartedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestDetachedRunningGivesWait(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	for i := 0; i < 3; i++ {
		ins := onlyInstruction(t, h.next(run), wf.InstructionWait)
		if !strings.Contains(ins.Prompt, "pid 100") || ins.RetryAfterSeconds != 2 {
			t.Fatalf("instruction = %+v, want a wait naming pid 100, retry after 2s", ins)
		}
	}
	if got := fr.startCount(); got != 1 {
		t.Fatalf("launches = %d, want 1", got)
	}
	req := fr.request(0)
	if strings.Join(req.Argv, " ") != "make build" || !strings.HasSuffix(req.ResultPath, filepath.Join("effects", strconv.FormatInt(intent, 10)+".json")) {
		t.Fatalf("request = %+v, want argv [make build] and the result path of intent %d", req, intent)
	}
	if w.appliedCount() != 0 || len(resultsOf(t, h, run)) != 0 {
		t.Fatalf("applied %d inline, results %v: a detached effect runs only under the runner", w.appliedCount(), resultsOf(t, h, run))
	}
	if st := startedOf(t, h, run); len(st) != 1 || st[0].Intent != intent || st[0].Pid != 100 || st[0].Max != time.Minute {
		t.Fatalf("effect_started = %+v, want intent %d pid 100 max 1m", st, intent)
	}
}

func TestDetachedResultFileRecovered(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	fr.set(intent, wf.DetachedPresent, wf.RunResult{Exit: 0, Stdout: "built"})
	// A fresh engine over the same journal: the launching call is gone.
	opts := detOpts(t, fr)
	opts.Clock, opts.Defs = func() time.Time { return h.now }, []*wf.Def{detDef(idempotentDetached(time.Minute))}
	h.e = wf.NewEngine(h.j, h.reg, opts)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none", ins)
	}
	statusOf(t, h.status(run), wf.RunEnded, "dOK")
	res := resultsOf(t, h, run)
	if len(res) != 1 || !res[0].OK || res[0].Intent != intent || !strings.Contains(string(res[0].Result), `"Stdout":"built"`) {
		t.Fatalf("effect_result = %+v, want one result for intent %d carrying the file's RunResult", res, intent)
	}
	if fr.startCount() != 1 {
		t.Fatalf("launches = %d, want 1: recovery reads the file, it does not re-run", fr.startCount())
	}
}

func TestDetachedDeadGoesToRecovery(t *testing.T) {
	t.Run("a DoneWhen that says not done goes to manual verification naming the pid", func(t *testing.T) {
		d := detDef(func(c *wf.Command[dRun]) {
			wf.DoneWhen(c, wf.Observe[bool]("x", "done", nil), "the target exists")
			c.Detached(time.Minute)
		})
		fr, w := newDetRunner(), newWorld()
		w.set("x", "done", nil, false)
		h, run, intent := detStart(t, d, fr, w)
		fr.set(intent, wf.DetachedDead, wf.RunResult{})
		for i := 0; i < 3; i++ {
			ins := onlyInstruction(t, h.next(run), wf.InstructionManualVerification)
			if !strings.Contains(ins.Prompt, "detached runner pid 100 died with no result file") {
				t.Fatalf("prompt = %q, want the dead pid named", ins.Prompt)
			}
		}
		if fr.startCount() != 1 || w.appliedCount() != 0 {
			t.Fatalf("launches %d, inline applies %d: an uncertain effect is never re-run", fr.startCount(), w.appliedCount())
		}
	})

	t.Run("an idempotent command is re-run under a fresh intent once the group is empty", func(t *testing.T) {
		fr, w := newDetRunner(), newWorld()
		h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
		fr.set(intent, wf.DetachedDead, wf.RunResult{})
		ins := onlyInstruction(t, h.next(run), wf.InstructionWait)
		if !strings.Contains(ins.Prompt, "pid 101") {
			t.Fatalf("prompt = %q, want a wait for the re-launched pid 101", ins.Prompt)
		}
		seqs, data := intentsOf(t, h, run)
		if len(seqs) != 2 || data[1].Rerun == nil || data[1].Rerun.Of != intent || data[1].Rerun.Pid != 100 || data[1].Rerun.StartTicks != 5100 ||
			!strings.Contains(data[1].Rerun.Reason, "detached runner pid 100 died with no result file") || data[1].Max != time.Minute {
			t.Fatalf("effect_intent records = %v %+v, want a second intent re-running %d (pid 100, ticks 5100) with Max 1m", seqs, data, intent)
		}
		if st := startedOf(t, h, run); len(st) != 2 || st[0].Intent != intent || st[1].Intent != seqs[1] || st[1].Pid != 101 || st[1].StartTicks != 5101 {
			t.Fatalf("effect_started = %+v, want the second launch (pid 101, ticks 5101) against the new intent %d", st, seqs[1])
		}
		first, second := fr.request(0), fr.request(1)
		for ext, pair := range map[string][2]string{".json": {first.ResultPath, second.ResultPath}, ".proc": {first.ProcPath, second.ProcPath}, ".log": {first.LogPath, second.LogPath}} {
			if filepath.Base(pair[1]) != strconv.FormatInt(seqs[1], 10)+ext || pair[0] == pair[1] {
				t.Errorf("re-run %s path = %q (first %q), want effects/%d%s", ext, pair[1], pair[0], seqs[1], ext)
			}
		}
		// The new process dies too: it is re-run again, never waited for.
		fr.set(seqs[1], wf.DetachedDead, wf.RunResult{})
		h.next(run)
		if fr.startCount() != 3 {
			t.Fatalf("launches = %d, want 3: a dead runner is re-run, not waited for", fr.startCount())
		}
	})

	t.Run("a keyed command whose Check says not done is re-run", func(t *testing.T) {
		fr, w := newDetRunner(), newWorld()
		w.status["build/build"] = wf.EffectNotDone
		h, run, intent := detStart(t, detDef(func(c *wf.Command[dRun]) { c.Detached(time.Minute) }), fr, w)
		fr.set(intent, wf.DetachedDead, wf.RunResult{})
		onlyInstruction(t, h.next(run), wf.InstructionWait)
		if fr.startCount() != 2 || w.checks != 1 {
			t.Fatalf("launches %d, checks %d, want 2 and 1", fr.startCount(), w.checks)
		}
	})
}

// G2 x G4: a runner judged dead by its stale heartbeat, whose keyed command
// the runtime cannot vouch for, is a manual verification; settling it not
// applied re-issues the command once under a fresh detached launch, and the
// re-issue dying the same way and settled not applied again is Blocked, with
// no third launch.
func TestDeadHeartbeatNotAppliedReissuesOnceThenBlocked(t *testing.T) {
	const why = "heartbeat stale for 15s: runner presumed dead"
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(func(c *wf.Command[dRun]) { c.Detached(time.Minute) }), fr, w)
	fr.setReason(intent, why)
	fr.set(intent, wf.DetachedDead, wf.RunResult{})
	first := onlyInstruction(t, h.next(run), wf.InstructionManualVerification)
	if !strings.Contains(first.Prompt, why) {
		t.Fatalf("prompt = %q, want the heartbeat reason", first.Prompt)
	}
	if err := settleAs(h.e, run, first.Effect, false); err != nil {
		t.Fatal(err)
	}
	seqs, _ := intentsOf(t, h, run)
	if len(seqs) != 2 || fr.startCount() != 2 || filepath.Base(fr.request(1).ResultPath) != strconv.FormatInt(seqs[1], 10)+".json" {
		t.Fatalf("intents %v, launches %d, want the re-issue launched detached under its own intent", seqs, fr.startCount())
	}
	fr.setReason(seqs[1], why)
	fr.set(seqs[1], wf.DetachedDead, wf.RunResult{})
	second := onlyInstruction(t, h.next(run), wf.InstructionManualVerification)
	if second.Effect != seqs[1] {
		t.Fatalf("verification effect = %d, want the re-issue %d", second.Effect, seqs[1])
	}
	if err := settleAs(h.e, run, second.Effect, false); err != nil {
		t.Fatal(err)
	}
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %s, want blocked after the second not-applied", st.Phase)
	}
	if fr.startCount() != 2 || w.appliedCount() != 0 {
		t.Fatalf("launches %d, inline applies %d, want no third run", fr.startCount(), w.appliedCount())
	}
}

func TestDetachedMaxDurationKills(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	h.now = h.now.Add(59 * time.Second)
	onlyInstruction(t, h.next(run), wf.InstructionWait)
	if got := fr.terminations(); len(got) != 0 {
		t.Fatalf("terminated %v before the maximum duration", got)
	}
	h.now = h.now.Add(2 * time.Second)
	h.next(run)
	if got := fr.terminations(); len(got) != 1 || got[0] != intent {
		t.Fatalf("terminated = %v, want [%d] once", got, intent)
	}
	res := resultsOf(t, h, run)
	var rr wf.RunResult
	if len(res) != 1 || !res[0].OK || res[0].Outcome != "timed out" || json.Unmarshal(res[0].Result, &rr) != nil || rr.Exit != 124 || !strings.Contains(rr.Stderr, "timed out after 1m0s") {
		t.Fatalf("effect_result = %+v (%+v), want a timed-out exit-124 result", res, rr)
	}
	statusOf(t, h.status(run), wf.RunEnded, "dFail")
	if w.checks != 0 {
		t.Fatalf("checks = %d, want 0: a timeout never enters Check recovery", w.checks)
	}
}

func TestDetachedPresentBeatsTimeout(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	h.now = h.now.Add(time.Hour)
	fr.set(intent, wf.DetachedPresent, wf.RunResult{Exit: 0})
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "dOK")
	if got := fr.terminations(); len(got) != 0 {
		t.Fatalf("terminated %v: the result file wins over the timeout", got)
	}
	if res := resultsOf(t, h, run); len(res) != 1 || res[0].Outcome != "applied" {
		t.Fatalf("effect_result = %+v, want the file's result", res)
	}
}

func TestDetachedRerunWaitsForGroup(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	fr.set(intent, wf.DetachedDeadLive, wf.RunResult{})
	fr.setStubborn(intent, true)
	ins := onlyInstruction(t, h.next(run), wf.InstructionWait)
	if !strings.Contains(ins.Prompt, "process group 100") {
		t.Fatalf("prompt = %q, want a wait naming process group 100", ins.Prompt)
	}
	if got := fr.terminations(); len(got) != 1 || got[0] != intent {
		t.Fatalf("terminated = %v, want [%d] once", got, intent)
	}
	if fr.startCount() != 1 {
		t.Fatalf("launches = %d, want 1: a live group blocks the re-run", fr.startCount())
	}
	fr.setMembers(intent, 0)
	h.now = h.now.Add(10 * time.Second) // the re-run intent's start grace
	h.next(run)
	if fr.startCount() != 2 {
		t.Fatalf("launches = %d, want 2 once the group is empty", fr.startCount())
	}
	if res := resultsOf(t, h, run); len(res) != 0 {
		t.Fatalf("effect_result = %+v, want none: a dead launch never settles timed out", res)
	}
}

func TestDetachedRerunWaitBoundedByMax(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	fr.set(intent, wf.DetachedDeadLive, wf.RunResult{})
	fr.setStubborn(intent, true)
	onlyInstruction(t, h.next(run), wf.InstructionWait)
	h.now = h.now.Add(59 * time.Second) // the new intent's At is the dead decision's time
	onlyInstruction(t, h.next(run), wf.InstructionWait)
	if fr.startCount() != 1 {
		t.Fatalf("launches = %d, want 1 inside the new intent's maximum", fr.startCount())
	}
	h.now = h.now.Add(2 * time.Second)
	h.next(run)
	if fr.startCount() != 2 {
		t.Fatalf("launches = %d, want 2: past At + Max the re-run starts anyway", fr.startCount())
	}
}

func TestDetachedLostStartedUsesProcRecord(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, fr))
	withDroppedStarted(t, h, fr, d)
	run := h.run(d, map[string]string{"topic": "build"})
	if _, err := h.e.Next(run, ""); err == nil {
		t.Fatal("Next succeeded, want the lost effect_started append to fail it")
	}
	intent := lastIntent(t, h, run)
	if fr.startCount() != 1 || len(startedOf(t, h, run)) != 0 {
		t.Fatalf("launches %d, effect_started %d, want 1 and 0", fr.startCount(), len(startedOf(t, h, run)))
	}
	ins := onlyInstruction(t, h.next(run), wf.InstructionWait)
	if !strings.Contains(ins.Prompt, "pid 100") || fr.startCount() != 1 {
		t.Fatalf("prompt %q, launches %d, want a wait for pid 100 and no second Start while the leader lives", ins.Prompt, fr.startCount())
	}
	if st := startedOf(t, h, run); len(st) != 1 || st[0].Intent != intent || st[0].Pid != 100 || st[0].StartTicks != 5100 || st[0].Max != time.Minute {
		t.Fatalf("effect_started = %+v, want the proc record's launch journaled against intent %d", st, intent)
	}
	// The leader hangs: the maximum runs from the intent's At.
	h.now = h.now.Add(61 * time.Second)
	h.next(run)
	res := resultsOf(t, h, run)
	if got := fr.terminations(); len(got) != 1 || got[0] != intent || len(res) != 1 || res[0].Outcome != "timed out" {
		t.Fatalf("terminated %v, results %+v, want one termination and a timed-out result", got, res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "dFail")
}

func TestDetachedStartGrace(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, fr))
	withDroppedStarted(t, h, fr, d)
	run := h.run(d, map[string]string{"topic": "build"})
	if _, err := h.e.Next(run, ""); err == nil {
		t.Fatal("Next succeeded, want the lost effect_started append to fail it")
	}
	// The runner's proc write has not landed: no proc record, no result file.
	fr.setHideProc(lastIntent(t, h, run), true)
	h.now = h.now.Add(9 * time.Second)
	ins := onlyInstruction(t, h.next(run), wf.InstructionWait)
	if !strings.Contains(ins.Prompt, "may still be starting") || fr.startCount() != 1 {
		t.Fatalf("prompt %q, launches %d, want a start wait and no second Start inside the grace", ins.Prompt, fr.startCount())
	}
	h.now = h.now.Add(time.Second)
	h.next(run)
	if fr.startCount() != 2 {
		t.Fatalf("launches = %d, want 2 at At + 10s", fr.startCount())
	}
}

func TestDetachedStartGraceRerun(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h, run, intent := detStart(t, d, fr, w)
	withDroppedStarted(t, h, fr, d)
	fr.set(intent, wf.DetachedDead, wf.RunResult{})
	if _, err := h.e.Next(run, ""); err == nil {
		t.Fatal("Next succeeded, want the lost effect_started append of the re-run to fail it")
	}
	re := lastIntent(t, h, run)
	if re == intent || fr.startCount() != 2 {
		t.Fatalf("re-run intent %d (first %d), launches %d, want a new intent and 2 launches", re, intent, fr.startCount())
	}
	// The re-run's runner proc write has not landed: no proc record, no result file.
	fr.setHideProc(re, true)
	h.now = h.now.Add(9 * time.Second)
	ins := onlyInstruction(t, h.next(run), wf.InstructionWait)
	if !strings.Contains(ins.Prompt, "may still be starting") || fr.startCount() != 2 {
		t.Fatalf("prompt %q, launches %d, want a start wait and no third Start inside the re-run's grace", ins.Prompt, fr.startCount())
	}
	h.now = h.now.Add(time.Second)
	h.next(run)
	if fr.startCount() != 3 {
		t.Fatalf("launches = %d, want 3 at the re-run's At + 10s", fr.startCount())
	}
}

func TestDetachedThreeDeadLaunchesRunnerFailed(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	for i := 1; i <= 3; i++ {
		fr.setReason(intent, "boom "+strconv.Itoa(i))
		status := wf.DetachedDead
		if i == 3 {
			status = wf.DetachedDeadLive
		}
		fr.set(intent, status, wf.RunResult{})
		h.next(run)
		if i < 3 {
			intent = lastIntent(t, h, run)
		}
	}
	seqs, data := intentsOf(t, h, run)
	if len(seqs) != 3 || data[1].Rerun == nil || data[1].Rerun.Reason != "boom 1" || data[2].Rerun == nil || data[2].Rerun.Of != seqs[1] || data[2].Rerun.Reason != "boom 2" {
		t.Fatalf("effect_intent records = %v %+v, want three launches, each re-running the previous with its reason", seqs, data)
	}
	if fr.startCount() != 3 {
		t.Fatalf("launches = %d, want 3: no fourth run", fr.startCount())
	}
	res := resultsOf(t, h, run)
	var rr wf.RunResult
	if len(res) != 1 || !res[0].OK || res[0].Outcome != "runner failed" || res[0].Intent != seqs[2] || json.Unmarshal(res[0].Result, &rr) != nil || rr.Exit != 125 || rr.Stderr != "boom 3" {
		t.Fatalf("effect_result = %+v (%+v), want one runner-failed exit-125 result carrying the last reason", res, rr)
	}
	if got := fr.terminations(); len(got) != 1 || got[0] != seqs[2] {
		t.Fatalf("terminated = %v, want [%d]: the third launch's group is ended before the red result", got, seqs[2])
	}
	statusOf(t, h.status(run), wf.RunEnded, "dFail")
	if w.appliedCount() != 0 {
		t.Fatalf("applied %d inline, want 0", w.appliedCount())
	}
}

// legacyJournal rewrites what the engine appends into the records an engine
// from before the intent carried Max and the started record carried ticks wrote.
type legacyJournal struct {
	wf.Journal
	start func() time.Time
}

func (j legacyJournal) Append(r wf.JournalRecord) error {
	switch r.Kind {
	case wf.JournalEffectIntent:
		var d wf.EffectIntentData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			return err
		}
		d.Max = 0
		r.Data, _ = json.Marshal(d)
	case wf.JournalEffectStarted:
		var d wf.EffectStartedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			return err
		}
		d.StartTicks, d.Start = 0, j.start()
		r.Data, _ = json.Marshal(d)
	}
	return j.Journal.Append(r)
}

func TestDetachedLegacyStartedFolds(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarnessOpts(t, d, w, detOpts(t, fr))
	begin := h.now
	opts := detOpts(t, fr)
	opts.Clock, opts.Defs = func() time.Time { return h.now }, []*wf.Def{d}
	h.e = wf.NewEngine(legacyJournal{h.j, func() time.Time { return begin }}, h.reg, opts)
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	onlyInstruction(t, h.next(run), wf.InstructionWait)
	if st := startedOf(t, h, run); len(st) != 1 || st[0].StartTicks != 0 || !st[0].Start.Equal(begin) {
		t.Fatalf("effect_started = %+v, want a legacy record: wall start, no ticks", st)
	}
	last := fr.statusAsk[len(fr.statusAsk)-1]
	if last.StartTicks != 0 || !last.Start.Equal(begin) || last.Pgid != 100 {
		t.Fatalf("Status asked about %+v, want the legacy process: group 100, no ticks, the wall start", last)
	}
	// Judged by the group alone: a gone leader with an empty group is dead and
	// re-run; the maximum runs from the legacy record's wall start.
	h.now = h.now.Add(61 * time.Second)
	h.next(run)
	res := resultsOf(t, h, run)
	if len(res) != 1 || res[0].Outcome != "timed out" {
		t.Fatalf("effect_result = %+v, want timed out measured from the legacy Start", res)
	}
}

func TestDetachedStatusNamesGroupWait(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	begin := h.now
	st := h.status(run)
	if len(st.OpenIntents) != 1 || !st.OpenIntents[0].Detached || !st.OpenIntents[0].Due.Equal(begin.Add(time.Minute)) || st.OpenIntents[0].WaitGroup != 0 {
		t.Fatalf("open intents = %+v, want the running launch with max due %v", st.OpenIntents, begin.Add(time.Minute))
	}
	fr.set(intent, wf.DetachedDeadLive, wf.RunResult{})
	fr.setStubborn(intent, true)
	fr.setReason(intent, "runner: out of memory")
	h.now = h.now.Add(5 * time.Second)
	onlyInstruction(t, h.next(run), wf.InstructionWait)
	oi := h.status(run).OpenIntents
	rerun := lastIntent(t, h, run)
	if len(oi) != 1 || oi[0].Seq != rerun || oi[0].Detached || oi[0].WaitGroup != 100 || oi[0].RerunOf != intent || oi[0].Reason != "runner: out of memory" || !oi[0].Due.Equal(h.now.Add(time.Minute)) {
		t.Fatalf("open intents = %+v, want intent %d waiting on group 100 after dead launch %d, due %v", oi, rerun, intent, h.now.Add(time.Minute))
	}
	want := "open effect " + strconv.FormatInt(rerun, 10) + " shell.run detached: re-run waits for process group 100 to end (max due " +
		h.now.Add(time.Minute).UTC().Format(time.RFC3339) + "); dead launch " + strconv.FormatInt(intent, 10) + ": runner: out of memory\n"
	if view := wf.RenderStatus(h.status(run)); !strings.Contains(view, want) {
		t.Fatalf("status view = %q, want it to contain %q", view, want)
	}
}

func TestDetachedDeadLaunchWakesWait(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	baseline := int64(len(h.records(run)))
	fr.set(intent, wf.DetachedDead, wf.RunResult{})
	fr.setReason(intent, "runner: out of memory")
	res, err := h.e.Wait(run, baseline, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	evs := eventsOf(res, wf.EventEffect)
	want := "re-run after dead launch " + strconv.FormatInt(intent, 10) + ": runner: out of memory"
	if len(evs) != 1 || evs[0].Detail != want {
		t.Fatalf("events = %+v, want one effect done with detail %q", res.Events, want)
	}
}

func TestDetachedDefaultMax(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, _ := detStart(t, detDef(idempotentDetached()), fr, w)
	if st := startedOf(t, h, run); len(st) != 1 || st[0].Max != 60*time.Minute {
		t.Fatalf("effect_started = %+v, want Max 60m", st)
	}
	if v := wf.Inspect(detDef(idempotentDetached())).States[0].Detached; v != 60*time.Minute {
		t.Fatalf("StateView.Detached = %v, want 60m", v)
	}
}

func TestDetachedStartFailureIsUnavailable(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	fr.startErr = wf.ErrUnavailable
	d := detDef(idempotentDetached())
	h := newHarnessOpts(t, d, w, detOpts(t, fr))
	run := h.run(d, map[string]string{"topic": "x"})
	h.next(run)
	res := resultsOf(t, h, run)
	if len(res) != 1 || res[0].OK || res[0].Outcome != "unavailable" {
		t.Fatalf("effect_result = %+v, want an unavailable failure", res)
	}
}

func TestDetachedWakesWait(t *testing.T) {
	fr := newDetRunner()
	d := detDef(idempotentDetached(time.Minute))
	opts := detOpts(t, fr)
	opts.EffectPoll = 50 * time.Millisecond
	w := newWaiter(t, d, opts)
	run, err := w.e.Run(d, map[string]string{"topic": "build"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	intents := 0
	unlock, err := w.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := w.j.Records(run)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	var intent int64
	for _, r := range recs {
		if r.Kind == wf.JournalEffectIntent {
			intents++
			intent = r.Seq
		}
	}
	if intents != 1 {
		t.Fatalf("intents = %d, want 1", intents)
	}
	w.after(100*time.Millisecond, func() error {
		fr.set(intent, wf.DetachedPresent, wf.RunResult{Exit: 0})
		return nil
	})
	begin := time.Now()
	res := w.wait(run, 0, 10*time.Second)
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("Wait took %v, want within a poll of the file appearing", took)
	}
	if evs := eventsOf(res, wf.EventEffect); len(evs) != 1 || evs[0].Detail != "applied" {
		t.Fatalf("events = %+v, want one effect done", res.Events)
	}
}

type (
	gRun  struct{}
	gOK   struct{}
	gFail struct{}
	gExit int
)

// goldenDef is a definition with no .Detached(); goldenRender and goldenHash
// were captured from it before the detached path existed.
func goldenDef() *wf.Def {
	d := wf.New("detached")
	d.Param("topic")
	exits := wf.NewEnum(d, "g-exit", gExit(0), gExit(1))
	cmd := wf.NewCommand[gRun](d, "make", "{topic}")
	ok := wf.NewTerminal[gOK](d, wf.Success)
	fail := wf.NewTerminal[gFail](d, wf.Stopped)
	cmd.Key("build", "{topic}").Idempotent()
	cmd.RouteCases(wf.Cases[gRun, wf.RunResult](wf.ExitAs[gExit], exits,
		wf.On(gExit(0), cmd.To(ok, "passes")),
		wf.On(gExit(1), cmd.To(fail, "fails"))))
	return d
}

const goldenRender = `detached v0 — 3 states, start: gRun

▶ gRun               command make {topic} key=build/{topic} idempotent
      cases g-exit: 0 → passes | 1 → fails
      ├── passes ──▶ gOK
      └── fails ──▶ gFail
  gOK                terminal ■ success
  gFail              terminal ■ stopped
`

func TestUndecoratedCommandInlineUnchanged(t *testing.T) {
	d := goldenDef()
	if got := d.Render(); got != goldenRender {
		t.Fatalf("Render changed:\n%s\nwant:\n%s", got, goldenRender)
	}
	if got := d.Hash(); got != "sha256:a76d5857532921e7" {
		t.Fatalf("Hash = %s, want the golden sha256:a76d5857532921e7", got)
	}
	fr, w := newDetRunner(), newWorld()
	h := newHarnessOpts(t, d, w, detOpts(t, fr))
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "gOK")
	if fr.startCount() != 0 || w.appliedCount() != 1 {
		t.Fatalf("launches %d, inline applies %d, want 0 and 1", fr.startCount(), w.appliedCount())
	}
}

func TestDetachedRendersWhenSet(t *testing.T) {
	d := detDef(idempotentDetached(90 * time.Minute))
	if got := d.Render(); !strings.Contains(got, "idempotent detached(1h30m0s)") {
		t.Fatalf("Render = %s, want the detached(max) marker", got)
	}
}

func TestDetachedNilRunnerRunsInline(t *testing.T) {
	w := newWorld()
	d := detDef(idempotentDetached(time.Minute))
	h := newHarness(t, d, w)
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "dOK")
	if w.appliedCount() != 1 || len(h.kind(run, wf.JournalEffectStarted)) != 0 {
		t.Fatalf("inline applies %d, effect_started %d, want 1 and 0", w.appliedCount(), len(h.kind(run, wf.JournalEffectStarted)))
	}
}

// The runner's request carries an absolute Dir: a relative or empty command
// dir resolves against the run's workspace, as the inline runtime does.
func TestDetachedRequestDirIsAbsolute(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"", "/ws"}, {"sub/dir", "/ws/sub/dir"}, {"/abs", "/abs"}} {
		fr := newDetRunner()
		d := detDef(func(c *wf.Command[dRun]) {
			c.Idempotent().Detached()
			if tc.in != "" {
				c.In(tc.in)
			}
		})
		opts := detOpts(t, fr)
		opts.Workspace = "/ws"
		h := newHarnessOpts(t, d, newWorld(), opts)
		h.next(h.run(d, map[string]string{"topic": "build"}))
		if fr.startCount() != 1 || fr.request(0).Dir != tc.want {
			t.Errorf("In(%q): request %+v, want dir %q", tc.in, fr.request(0), tc.want)
		}
	}
}

func TestDetachedRequestCarriesProcAndLogPaths(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	req := fr.request(0)
	base := strconv.FormatInt(intent, 10)
	for ext, got := range map[string]string{".json": req.ResultPath, ".proc": req.ProcPath, ".log": req.LogPath} {
		if filepath.Base(got) != base+ext || filepath.Base(filepath.Dir(got)) != "effects" || filepath.Dir(got) != filepath.Dir(req.ResultPath) {
			t.Errorf("path %q, want effects/%s%s beside the result file", got, base, ext)
		}
	}
	if st := startedOf(t, h, run); len(st) != 1 || st[0].StartTicks != 5100 {
		t.Fatalf("effect_started = %+v, want the returned StartTicks 5100", st)
	}
}

func TestDetachedDeadReasonFromLog(t *testing.T) {
	d := detDef(func(c *wf.Command[dRun]) {
		wf.DoneWhen(c, wf.Observe[bool]("x", "done", nil), "the target exists")
		c.Detached(time.Minute)
	})
	fr, w := newDetRunner(), newWorld()
	w.set("x", "done", nil, false)
	h, run, intent := detStart(t, d, fr, w)
	fr.set(intent, wf.DetachedDead, wf.RunResult{})
	fr.setReason(intent, "boom")
	ins := onlyInstruction(t, h.next(run), wf.InstructionManualVerification)
	if !strings.Contains(ins.Prompt, "boom") {
		t.Fatalf("prompt = %q, want the runner's log reason boom", ins.Prompt)
	}
}
