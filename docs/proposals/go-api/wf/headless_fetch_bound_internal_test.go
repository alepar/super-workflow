package wf

import (
	"os"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type (
	fbAsk    struct{}
	fbDone   struct{}
	fbGaveUp struct{}
	fbAnswer struct{ OK bool }
)

// parentPid is pid's parent, from /proc.
func parentPid(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b)[strings.LastIndexByte(string(b), ')')+1:])
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		t.Fatal(err)
	}
	return ppid
}

// The first-fetch bound holds whatever the supervisor's state: a headless
// child whose supervisor is killed before the child fetched its prompt reads
// as unknown (a stale heartbeat, no exit). Within the bound that is still a
// launch in progress; past it, with no fetch, the launch is dead, its unit is
// offered again, and the new offer's spawn starts a new child.
func TestHeadlessFetchBoundHoldsWhenTheSupervisorDies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the supervisor's parentage is read from /proc")
	}
	beat := headlessBeat
	headlessBeat = 50 * time.Millisecond
	t.Cleanup(func() { headlessBeat = beat })

	d := New("fetch-bound")
	a := NewAgent[fbAsk, fbAnswer](d)
	done := NewTerminal[fbDone](d, Success)
	gaveUp := NewTerminal[fbGaveUp](d, Stopped)
	d.OnFailure(Blocked, gaveUp)
	d.OnFailure(Unavailable, gaveUp)
	a.Spec(PromptText("Say hello."), Retry[fbAsk](Const(1), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(fbAnswer, Ctx) Edge[fbAsk] { return next }, next)
	a.Dispatch(OnFamily(FamilyNamed("claude")), ModelTier(TierLight))

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{
		Defs: []*Def{d}, RunID: "r1", Clock: func() time.Time { return now }, StateRoot: t.TempDir(), Harness: "codex",
		// A child that never fetches its prompt.
		Headless: &HeadlessConfig{Cmd: map[string][]string{"claude": {"sleep", "60"}}},
	}).(*Engine)
	id, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	co := Coordinator{Harness: "codex", Session: "s1"}
	claimOf := func(ins []Instruction) string {
		t.Helper()
		if len(ins) != 1 || ins[0].Spawn == "" {
			t.Fatalf("want one spawn offer, got %+v", ins)
		}
		return strings.Fields(ins[0].Spawn[strings.Index(ins[0].Spawn, "--claim ")+8:])[0]
	}
	ins, err := e.HandOutAs(id, 1, co)
	if err != nil {
		t.Fatal(err)
	}
	first := claimOf(ins)
	res, err := e.Spawn(id, first, co, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-res.Pid, syscall.SIGKILL) })

	// Kill the supervisor: the child runs on, unwatched, and the heartbeat
	// goes stale with no exit recorded.
	if err := syscall.Kill(parentPid(t, res.Pid), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h := HeadlessStartedData{Pid: res.Pid, Status: strings.TrimSuffix(res.ErrLog, ".stderr") + ".status"}
	for deadline := time.Now().Add(10 * time.Second); (osProcess{}).State(h) != HeadlessUnknown; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("state %d, want unknown once the supervisor is gone", (osProcess{}).State(h))
		}
	}

	now = now.Add(headlessFetchBound - 10*time.Second)
	if again, err := e.HandOutAs(id, 1, co); err != nil || len(again) != 0 {
		t.Fatalf("within the bound an unknown launch is still starting: %v %+v", err, again)
	}

	now = now.Add(time.Hour) // past the bound and the unstarted grace
	again, err := e.HandOutAs(id, 1, co)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || !again[0].NotStarted {
		t.Fatalf("past the bound with no fetch, want the unit offered again as not started: %+v", again)
	}
	second := claimOf(again)
	if second == first {
		t.Fatalf("the re-offer kept the dead launch's claim %s: the orphan could still fetch it", first)
	}
	res2, err := e.Spawn(id, second, co, os.Environ())
	if err != nil {
		t.Fatalf("the re-offer's spawn was refused: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-res2.Pid, syscall.SIGKILL) })
	if res2.Pid == res.Pid {
		t.Errorf("the re-offer did not start a new child")
	}
	// The orphan, still running, fetches first under its old claim: an
	// earlier offer's child may claim the unit. The new child's fetch is
	// then refused, so the unit never has two children working on it.
	name := again[0].Name
	if _, err := e.PromptFetchAs(id, name, first, harness.Caller{Harness: "claude", Session: "s1", Agent: "orphan"}); err != nil {
		t.Logf("the orphan's fetch under the old claim: %v", err)
	} else if _, err := e.PromptFetchAs(id, name, second, harness.Caller{Harness: "claude", Session: "s1", Agent: "new"}); err == nil {
		t.Errorf("both the orphan and the new child fetched the unit's prompt")
	}
}
