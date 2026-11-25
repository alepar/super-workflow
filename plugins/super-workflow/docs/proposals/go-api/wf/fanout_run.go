package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

type fanoutJob struct {
	Target           FanoutTarget
	Fork             SnapshotFork
	Engine           *Engine
	Directory, Brief string
	Command          []string
}

type fanoutResult struct {
	Target                         FanoutTarget
	Fork                           SnapshotFork
	Outcome                        FanoutOutcome
	Elapsed                        string
	Command                        []string
	ExitError                      string `json:",omitempty"`
	Error                          string `json:",omitempty"`
	ChildTokens, CoordinatorTokens *int
	Coordinator                    harness.CoordinatorEvidence
}

type fanoutReport struct {
	Caveat    string
	SourceRun RunID
	Parallel  int
	Timeout   string
	Forks     []fanoutResult
	// Skipped are the points the plan left out, with why.
	Skipped []FanoutSkip `json:",omitempty"`
	// Manufactured: the source run is a manufactured world (its run_started
	// mark), which every fork inherits.
	Manufactured *ManufacturedData `json:",omitempty"`
}

type fanoutProcess interface {
	Wait() error
	Kill() error
}
type fanoutStarter func(fanoutJob) (fanoutProcess, error)

type osFanoutProcess struct {
	cmd *exec.Cmd
	log *os.File
}

func (p *osFanoutProcess) Wait() error { defer p.log.Close(); return p.cmd.Wait() }
func (p *osFanoutProcess) Kill() error {
	err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func startFanoutProcess(job fanoutJob) (fanoutProcess, error) {
	log, err := os.OpenFile(filepath.Join(job.Directory, "coordinator.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(job.Command[0], job.Command[1:]...)
	cmd.Dir = job.Fork.Workspace
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = fanoutEnvironment()
	if err = cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	return &osFanoutProcess{cmd: cmd, log: log}, nil
}

// New coordinators establish their own identities. Keep profile homes, but
// remove inherited world routing and parent harness/Herdr identity markers.
func fanoutEnvironment() []string {
	removed := map[string]bool{"HC_TERMINAL_ID": true, "CODEX_SESSION_ID": true, "HERMES_DELEGATED_CHILD_CONTEXT": true, "HERMES_KANBAN_TASK": true}
	for _, name := range harness.Markers(os.Getenv) {
		removed[name] = true
	}
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if removed[name] || strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "BEADS_") || strings.HasPrefix(name, "BD_") || strings.HasPrefix(name, "DOLT_") || strings.HasPrefix(name, "HERDR_") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func runFanoutJobs(jobs []fanoutJob, parallel int, timeout time.Duration, start fanoutStarter) []fanoutResult {
	results := make([]fanoutResult, len(jobs))
	queue := make(chan int)
	var workers sync.WaitGroup
	for n := 0; n < parallel && n < len(jobs); n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range queue {
				results[i] = runFanoutJob(jobs[i], timeout, start)
			}
		}()
	}
	for i := range jobs {
		queue <- i
	}
	close(queue)
	workers.Wait()
	return results
}

func runFanoutJob(job fanoutJob, timeout time.Duration, start fanoutStarter) fanoutResult {
	began := time.Now()
	result := fanoutResult{Target: job.Target, Fork: job.Fork, Command: job.Command, Outcome: FanoutOutcome{Status: "stalled"}}
	process, err := start(job)
	if err != nil {
		result.Error = "launch: " + err.Error()
		result.Elapsed = time.Since(began).String()
		return result
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	timer := time.NewTimer(timeout)
	timedOut := false
	select {
	case err = <-done:
	case <-timer.C:
		timedOut = true
		// Stop is durable before killing the coordinator. If it holds the journal
		// lock, retain the error and retry after its process group has died.
		stopErr := job.Engine.Stop(job.Fork.Run, "fanout wall-clock limit exceeded")
		killErr := process.Kill()
		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			err = fmt.Errorf("coordinator did not exit after kill")
			result.Error = err.Error()
		}
		if stopErr != nil {
			stopErr = job.Engine.Stop(job.Fork.Run, "fanout wall-clock limit exceeded")
		}
		if stopErr != nil {
			result.Error = "stop: " + stopErr.Error()
		}
		if killErr != nil {
			result.Error = strings.TrimSpace(result.Error + " kill: " + killErr.Error())
		}
	}
	timer.Stop()
	// A coordinator may exit leaving children in its process group. Kill those
	// before collecting evidence; children that escape the group are outside
	// this process cleanup contract and may leave incomplete evidence.
	if killErr := process.Kill(); killErr != nil && result.Error == "" {
		result.Error = "cleanup: " + killErr.Error()
	}
	if err != nil {
		result.ExitError = err.Error()
	}
	records, readErr := ReadJournal(job.Fork.StateDir, job.Fork.Run)
	if readErr != nil {
		result.Error = "journal: " + readErr.Error()
	} else {
		result.Outcome, readErr = ClassifyFanout(records, job.Target)
		if readErr != nil {
			result.Error = "classify: " + readErr.Error()
		}
		result.ChildTokens = result.Outcome.Tokens
		collectFanoutEvidence(&result, records, job.Target.SnapshotSeq, job.Engine.opts.caller.Executable)
	}
	if timedOut {
		result.Outcome.Status = "timed out"
	}
	result.Elapsed = time.Since(began).String()
	return result
}

func collectFanoutEvidence(result *fanoutResult, records []JournalRecord, after int64, executable string) {
	type transcript struct{ path, harness string }
	children := map[string]transcript{}
	coordinators := map[CoordinatorData]bool{}
	for _, record := range records {
		if record.Seq <= after {
			continue
		}
		switch record.Kind {
		case JournalCoordinator:
			var coordinator CoordinatorData
			if json.Unmarshal(record.Data, &coordinator) == nil && coordinator.Session != "" {
				coordinators[coordinator] = true
			}
		case JournalAttemptStarted:
			var d AttemptStartedData
			if json.Unmarshal(record.Data, &d) == nil && d.Transcript != "" {
				children[d.Transcript] = transcript{d.Transcript, d.Harness}
			}
		case JournalMetadataAttached:
			var d MetadataAttachedData
			if json.Unmarshal(record.Data, &d) == nil && d.Transcript != "" {
				children[d.Transcript] = transcript{d.Transcript, d.Harness}
			}
		}
	}
	total, known := 0, len(children) > 0
	for _, child := range children {
		n, ok := 0, false
		if t, has := harness.For(child.harness).(harness.Telemetry); has {
			n, ok = t.Usage(child.path)
		}
		if !ok {
			known = false
		}
		total += n
	}
	if known {
		result.ChildTokens = &total
	}

	coTokens, coKnown := 0, len(coordinators) > 0
	result.Coordinator = harness.CoordinatorEvidence{Known: len(coordinators) > 0, ChildVerbs: map[string]int{}}
	for coordinator := range coordinators {
		path, n, ok := "", 0, false
		if t, has := harness.For(coordinator.Harness).(harness.Telemetry); has {
			path = t.CoordinatorTranscript(coordinator.Root, coordinator.Session)
			n, ok = t.Usage(path)
		}
		coTokens += n
		coKnown = coKnown && ok
		evidence := harness.InspectCoordinator(path, coordinator.Harness, executable)
		result.Coordinator.Known = result.Coordinator.Known && evidence.Known
		result.Coordinator.Calls += evidence.Calls
		for verb, n := range evidence.ChildVerbs {
			result.Coordinator.ChildVerbs[verb] += n
		}
	}
	if coKnown {
		result.CoordinatorTokens = &coTokens
	}
	if result.Coordinator.Known {
		calls := result.Coordinator.Calls
		result.Outcome.CoordinatorCalls = &calls
		verbs := 0
		for _, n := range result.Coordinator.ChildVerbs {
			verbs += n
		}
		result.Outcome.ChildVerbsAttempted = &verbs
	}
}
