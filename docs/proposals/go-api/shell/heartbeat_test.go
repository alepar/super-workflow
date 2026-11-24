package shell

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// foreignNS is a process as a later command in another PID namespace sees it:
// the recorded pid and pgid name nothing there (escalation-and-recovery G2).
var foreignNS = wf.DetachedProcess{Pid: 1 << 22, Pgid: 1 << 22, StartTicks: 1}

// Under a simulated PID namespace (the pid cannot be probed, the files are
// shared), a live runner reads as running by its heartbeat, completes by its
// result file, and a runner that stopped beating reads as dead.
func TestHeartbeatTracksLivenessWithoutThePid(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })

	r, p, req := startSh(t, "sleep 0.6; echo done")
	waitAlive(t, req)
	otherNamespace(t, req)
	if rep, err := r.Status(foreignNS, req); err != nil || rep.Status != wf.DetachedRunning {
		t.Fatalf("live runner, invisible pid: %+v, %v; want running by heartbeat", rep, err)
	}
	if res := waitStatus(t, r, foreignNS, req, wf.DetachedPresent); res.Stdout != "done\n" {
		t.Fatalf("result %+v, want stdout done", res)
	}
	if _, err := os.Stat(alivePath(req.ResultPath)); !os.IsNotExist(err) {
		t.Errorf("heartbeat file left after the result (stat err %v)", err)
	}
	_ = p
}

// A heartbeat that went stale with no result is dead, even when the recorded
// pid is unreadable, so the engine settles it as not applied with no human.
func TestStaleHeartbeatIsDead(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })

	r, p, req := startSh(t, "sleep 30")
	waitAlive(t, req)
	otherNamespace(t, req)
	if rep, err := r.Status(foreignNS, req); err != nil || rep.Status != wf.DetachedRunning {
		t.Fatalf("fresh heartbeat across namespaces: %+v, %v; want running", rep, err)
	}
	if err := syscall.Kill(-p.Pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Minute)
	// The killed runner may have touched once more before dying.
	time.Sleep(2 * heartbeatEvery)
	if err := os.Chtimes(alivePath(req.ResultPath), stale, stale); err != nil {
		t.Fatal(err)
	}
	if rep, err := r.Status(foreignNS, req); err != nil || rep.Status != wf.DetachedDead {
		t.Fatalf("stale heartbeat, no result: %+v, %v; want dead", rep, err)
	}
}

// A launch with no heartbeat file (a runner from before G2) is judged by its
// pid as before.
func TestNoHeartbeatFallsBackToThePid(t *testing.T) {
	r, p, req := startSh(t, "sleep 30")
	waitAlive(t, req)
	if err := os.Remove(alivePath(req.ResultPath)); err != nil {
		t.Fatal(err)
	}
	// The runner re-creates the file on its next beat; stop it first.
	if err := syscall.Kill(-p.Pgid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-p.Pgid, syscall.SIGCONT) })
	_ = os.Remove(alivePath(req.ResultPath))
	if rep, err := r.Status(p, req); err != nil || rep.Status != wf.DetachedRunning {
		t.Fatalf("no heartbeat, visible live pid: %+v, %v; want running", rep, err)
	}
	if rep, err := r.Status(foreignNS, req); err != nil || rep.Status != wf.DetachedDead {
		t.Fatalf("no heartbeat, invisible pid: %+v, %v; want dead (the legacy judgment)", rep, err)
	}
}

func waitAlive(t *testing.T, req wf.DetachedRequest) {
	t.Helper()
	for i := 0; i < 250; i++ {
		if _, err := os.Stat(alivePath(req.ResultPath)); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the runner never wrote its heartbeat")
}

// A leader verified alive by its start ticks is running however stale its
// heartbeat: a suspended host must not get its runner terminated.
func TestLiveLeaderWinsOverAStaleHeartbeat(t *testing.T) {
	r, p, req := startSh(t, "sleep 30")
	waitAlive(t, req)
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(alivePath(req.ResultPath), stale, stale); err != nil {
		t.Fatal(err)
	}
	if rep, err := r.Status(p, req); err != nil || rep.Status != wf.DetachedRunning {
		t.Fatalf("stale heartbeat, live verified leader: %+v, %v; want running", rep, err)
	}
}

// otherNamespace rewrites the launch's proc record as a runner in another PID
// namespace would have written it: the reader's pid probe then means nothing.
func otherNamespace(t *testing.T, req wf.DetachedRequest) {
	t.Helper()
	b, err := os.ReadFile(req.ProcPath)
	if err != nil {
		t.Fatal(err)
	}
	var rec procRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.PidNS == "" {
		t.Fatal("the proc record names no PID namespace")
	}
	rec.PidNS = "pid:[1]"
	if err := writeJSON(req.ProcPath, rec); err != nil {
		t.Fatal(err)
	}
}

// In the runner's own namespace a killed runner is dead at once, without
// waiting out its heartbeat.
func TestSameNamespaceDeathIsImmediate(t *testing.T) {
	r, p, req := startSh(t, "sleep 30")
	waitAlive(t, req)
	if err := syscall.Kill(-p.Pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitReport(t, r, p, req, wf.DetachedDead)
}

// foreignRunner is a later command in another PID namespace: every launch's
// record names another namespace and every pid it reads names nothing.
type foreignRunner struct{ wf.EffectRunner }

func (f foreignRunner) Status(p wf.DetachedProcess, r wf.DetachedRequest) (wf.DetachedReport, error) {
	if b, err := os.ReadFile(r.ProcPath); err == nil {
		var rec procRecord
		if json.Unmarshal(b, &rec) == nil && rec.PidNS != "pid:[1]" {
			rec.PidNS = "pid:[1]"
			_ = writeJSON(r.ProcPath, rec)
		}
	}
	return f.EffectRunner.Status(foreignNS, r)
}

// The plan's G test: under a simulated PID namespace (pids unreadable, files
// shared) a detached suite completes and settles from its result, with no
// re-launch and no human gate.
func TestDetachedSuiteSettlesAcrossPidNamespaces(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sh", "-c", "sleep 1; echo done")
	e := engineOver(t, stateDir, d, foreignRunner{DetachedRunner(testBin(t))})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	killGroupsOf(t, stateDir, run)
	var st wf.RunStatus
	for i := 0; i < 500; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		if st, err = e.Status(run); err != nil {
			t.Fatal(err)
		}
		if st.Phase == wf.RunEnded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Phase != wf.RunEnded || st.Terminal != "cOK" {
		t.Fatalf("status = %s/%s, want ended/cOK", st.Phase, st.Terminal)
	}
	if n := len(kinds(t, stateDir, run, wf.JournalEffectStarted)); n != 1 {
		t.Errorf("effect_started records = %d, want 1: a live runner across namespaces is never re-launched", n)
	}
}

// A runner from before G2 (no heartbeat file and no namespace in its record)
// is judged by its pid alone.
func TestLegacyRecordIsJudgedByPid(t *testing.T) {
	r, p, req := startSh(t, "sleep 30")
	waitAlive(t, req)
	if err := syscall.Kill(-p.Pgid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-p.Pgid, syscall.SIGCONT) })
	_ = os.Remove(alivePath(req.ResultPath))
	b, err := os.ReadFile(req.ProcPath)
	if err != nil {
		t.Fatal(err)
	}
	var rec procRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	rec.PidNS = ""
	if err := writeJSON(req.ProcPath, rec); err != nil {
		t.Fatal(err)
	}
	if rep, err := r.Status(p, req); err != nil || rep.Status != wf.DetachedRunning {
		t.Fatalf("legacy record, live pid: %+v, %v; want running", rep, err)
	}
}

// Across namespaces dead-live is never reported: the engine would signal a
// group that is not this launch's.
func TestForeignDeathIsNeverDeadLive(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })
	r, p, req := startSh(t, "sleep 30")
	waitAlive(t, req)
	otherNamespace(t, req)
	if err := syscall.Kill(-p.Pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * heartbeatEvery)
	stale := time.Now().Add(-time.Minute)
	_ = os.Chtimes(alivePath(req.ResultPath), stale, stale)
	// A live local group under the recorded pgid (here: this test's own
	// process group) must not make it dead-live.
	local := wf.DetachedProcess{Pid: os.Getpid(), Pgid: syscall.Getpgrp()}
	if rep, err := r.Status(local, req); err != nil || rep.Status != wf.DetachedDead {
		t.Fatalf("foreign stale runner over a live local group: %+v, %v; want dead", rep, err)
	}
}

// an earlier revision: a runner in another PID namespace cannot be signalled, so
// Terminate leaves it a stop request, which it answers on its next heartbeat
// by stopping its own group. GroupEmpty there follows the heartbeat: not
// empty while it is fresh, empty once it is gone.
func TestForeignRunnerStopsOnRequest(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })

	// The command traps SIGTERM: only the SIGKILL after the grace stops it.
	r, p, req := startSh(t, "trap '' TERM; sleep 30 & wait; sleep 30")
	waitAlive(t, req)
	otherNamespace(t, req)
	if empty, err := r.GroupEmpty(foreignNS, req); err != nil || empty {
		t.Fatalf("GroupEmpty of a beating foreign runner = %v, %v, want not empty", empty, err)
	}
	if err := r.Terminate(foreignNS, req); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stopPath(req.ResultPath)); err != nil {
		t.Fatalf("no stop request written: %v", err)
	}
	// Terminate waited for the runner's answer: the heartbeat is gone.
	if present, fresh := heartbeat(req.ResultPath); present && fresh {
		t.Errorf("Terminate returned while the foreign runner still beats")
	}
	// The runner stops its own group within a beat plus the grace.
	deadline := time.Now().Add(terminateGrace + 3*time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-p.Pgid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-p.Pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the runner's group is still alive after its stop request (kill 0: %v)", err)
	}
	stale := time.Now().Add(-time.Minute)
	_ = os.Chtimes(alivePath(req.ResultPath), stale, stale)
	if empty, err := r.GroupEmpty(foreignNS, req); err != nil || !empty {
		t.Fatalf("GroupEmpty after the foreign runner stopped = %v, %v, want empty", empty, err)
	}
}
