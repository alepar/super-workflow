package wf

import (
	"encoding/json"
	"errors"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fanoutFakeProcess struct {
	done chan error
	kill func()
}

func (p *fanoutFakeProcess) Wait() error { return <-p.done }
func (p *fanoutFakeProcess) Kill() error {
	if p.kill != nil {
		p.kill()
	}
	return nil
}

type fanoutTestStart struct{}
type fanoutTestEnd struct{}

func fanoutTestJob(t *testing.T) fanoutJob {
	t.Helper()
	d := New("fanout-runner")
	a := NewAgent[fanoutTestStart, string](d)
	end := NewTerminal[fanoutTestEnd](d, Success)
	edge := a.To(end, "done")
	a.Spec(PromptText("work"), Retry[fanoutTestStart](Const(1), edge))
	a.Route(func(string, Ctx) Edge[fanoutTestStart] { return edge }, edge)
	state, ws := t.TempDir(), t.TempDir()
	e := NewEngine(FileJournal(state), NewAdapters(), EngineOptions{Defs: []*Def{d}, StateRoot: state, Workspace: ws, RunID: "probe"}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fanoutJob{Engine: e, Fork: SnapshotFork{Run: run, StateDir: state, Workspace: ws}, Directory: t.TempDir(), Target: FanoutTarget{LocalState: "fanoutTestStart", State: "fanout-runner/fanoutTestStart"}}
}

func TestFanoutTemplateDoesNotInterpretShellSyntax(t *testing.T) {
	args, err := fanoutArgv(`["echo","{workspace}","$(touch bad); {run}","{prompt}"]`)
	if err != nil {
		t.Fatal(err)
	}
	got := expandFanoutArgv(args, SnapshotFork{Workspace: "a b", Run: "run"}, "u", "brief", "prompt {workspace}")
	want := []string{"echo", "a b", "$(touch bad); run", "prompt {workspace}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv %q", got)
	}
	for _, bad := range []string{"echo hi", `[]`, `[""]`, `["a\u0000b"]`} {
		if _, err := fanoutArgv(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestFanoutTimeoutStopsBeforeKilling(t *testing.T) {
	job := fanoutTestJob(t)
	killed := false
	p := &fanoutFakeProcess{done: make(chan error, 1)}
	p.kill = func() {
		if killed {
			return
		}
		killed = true
		records, err := ReadJournal(job.Fork.StateDir, job.Fork.Run)
		if err != nil {
			t.Error(err)
		}
		paused := false
		for _, r := range records {
			paused = paused || r.Kind == JournalPaused
		}
		if !paused {
			t.Error("killed before durable Stop")
		}
		p.done <- nil
	}
	got := runFanoutJob(job, 10*time.Millisecond, func(fanoutJob) (fanoutProcess, error) { return p, nil })
	if got.Outcome.Status != "timed out" || !killed || got.Error != "" {
		t.Fatalf("result %+v killed=%v", got, killed)
	}
}

func TestFanoutParallelBoundAndStableReportOrder(t *testing.T) {
	jobs := make([]fanoutJob, 5)
	for i := range jobs {
		jobs[i] = fanoutTestJob(t)
		jobs[i].Target.State = string(rune('A' + i))
	}
	var mu sync.Mutex
	active, max := 0, 0
	start := func(fanoutJob) (fanoutProcess, error) {
		mu.Lock()
		active++
		if active > max {
			max = active
		}
		mu.Unlock()
		p := &fanoutFakeProcess{done: make(chan error, 1)}
		go func() { time.Sleep(10 * time.Millisecond); mu.Lock(); active--; mu.Unlock(); p.done <- nil }()
		return p, nil
	}
	results := runFanoutJobs(jobs, 2, time.Second, start)
	if max != 2 {
		t.Fatalf("max active %d", max)
	}
	for i, r := range results {
		if r.Target.State != jobs[i].Target.State || r.Outcome.Status != "stalled" {
			t.Fatalf("result %d: %+v", i, r)
		}
	}
}

func TestFanoutProcessOutputDoesNotProveDone(t *testing.T) {
	job := fanoutTestJob(t)
	job.Command = []string{"sh", "-c", `printf '{"status":"done","tokens":123}'`}
	result := runFanoutJob(job, time.Second, startFanoutProcess)
	if result.Outcome.Status != "stalled" || result.ChildTokens != nil {
		t.Fatalf("trusted stdout: %+v", result)
	}
	log, err := os.ReadFile(filepath.Join(job.Directory, "coordinator.log"))
	if err != nil || !json.Valid(log) {
		t.Fatalf("log %q %v", log, err)
	}
}

func TestFanoutProcessStripsSourceRoutingEnvironment(t *testing.T) {
	job := fanoutTestJob(t)
	t.Setenv("GIT_DIR", "source/.git")
	t.Setenv("GIT_WORK_TREE", "source")
	t.Setenv("BEADS_DIR", "source/.beads")
	t.Setenv("DOLT_ROOT_PATH", "source")
	t.Setenv("HERDR_PANE_ID", "parent-seat")
	t.Setenv("CODEX_THREAD_ID", "parent-session")
	t.Setenv("CODEX_HOME", "profile")
	job.Command = []string{"sh", "-c", `printf '%s/%s/%s/%s/%s/%s/%s' "${GIT_DIR-unset}" "${GIT_WORK_TREE-unset}" "${BEADS_DIR-unset}" "${DOLT_ROOT_PATH-unset}" "${HERDR_PANE_ID-unset}" "${CODEX_THREAD_ID-unset}" "$CODEX_HOME"`}
	result := runFanoutJob(job, time.Second, startFanoutProcess)
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	data, err := os.ReadFile(filepath.Join(job.Directory, "coordinator.log"))
	if err != nil || string(data) != "unset/unset/unset/unset/unset/unset/profile" {
		t.Fatalf("environment %s %v", data, err)
	}
	if os.Getenv("GIT_DIR") != "source/.git" {
		t.Fatal("changed parent environment")
	}
}

type fanoutUnkillable struct{ done chan error }

func (p *fanoutUnkillable) Wait() error { return <-p.done }
func (p *fanoutUnkillable) Kill() error { return errors.New("kill denied") }
func TestFanoutKillFailureDoesNotHangBatch(t *testing.T) {
	job := fanoutTestJob(t)
	p := &fanoutUnkillable{done: make(chan error, 1)}
	defer func() { p.done <- nil }()
	start := time.Now()
	got := runFanoutJob(job, time.Millisecond, func(fanoutJob) (fanoutProcess, error) { return p, nil })
	if time.Since(start) > 5*time.Second || got.Outcome.Status != "timed out" || !strings.Contains(got.Error, "kill denied") {
		t.Fatalf("unbounded timeout: %+v", got)
	}
}

func TestFanoutFreshCoordinatorDoesNotInheritChildIdentity(t *testing.T) {
	for _, name := range []string{"HERMES_SESSION_ID", "HERMES_DELEGATED_CHILD_CONTEXT", "HERMES_KANBAN_TASK", "CLAUDE_CODE_ENTRYPOINT", "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "OPENCODE_PARENT_SESSION_ID"} {
		t.Setenv(name, "parent")
	}
	t.Setenv("HERMES_HOME", "profile")
	values := map[string]string{}
	for _, entry := range fanoutEnvironment() {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	values["HERMES_SESSION_ID"] = "new-coordinator"
	caller := harness.For("hermes").Caller(func(name string) string { return values[name] })
	if caller.Is != harness.IsSelf || caller.Session != "new-coordinator" || caller.Root != "profile" {
		t.Fatalf("inherited identity: %+v", caller)
	}
}

func TestFanoutCoordinatorEvidenceIncludesEverySession(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "workspace")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var records []JournalRecord
	for _, session := range []string{"first", "second"} {
		body := `{"type":"assistant","message":{"id":"m","usage":{"input_tokens":2,"output_tokens":3},"content":[{"type":"tool_use","id":"call","name":"Bash","input":{"command":"wf next run"}}]}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(CoordinatorData{Harness: "claude", Root: root, Session: session})
		records = append(records, JournalRecord{Kind: JournalCoordinator, Seq: int64(len(records) + 1), Data: data})
	}
	result := fanoutResult{}
	collectFanoutEvidence(&result, records, 0, "wf")
	if result.CoordinatorTokens == nil || *result.CoordinatorTokens != 10 || !result.Coordinator.Known || result.Coordinator.Calls != 2 {
		t.Fatalf("lost session: %+v", result)
	}
	if err := os.Remove(filepath.Join(dir, "first.jsonl")); err != nil {
		t.Fatal(err)
	}
	result = fanoutResult{}
	collectFanoutEvidence(&result, records, 0, "wf")
	if result.CoordinatorTokens != nil || result.Coordinator.Known {
		t.Fatalf("missing evidence claimed complete: %+v", result)
	}
}
