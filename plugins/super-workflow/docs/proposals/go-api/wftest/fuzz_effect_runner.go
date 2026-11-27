package wftest

import (
	"encoding/json"
	"sync"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// fuzzEffectRunner injects at the detached launch boundary. Synthetic results
// stay keyed by the intent's result path, so later polls need no ambient state.
// Unaffected launches and their lifecycle retain the factory's runner behavior.
type fuzzEffectRunner struct {
	wf.EffectRunner
	world     *World
	mu        sync.Mutex
	results   map[string]wf.DetachedReport
	processes map[string]wf.DetachedProcess
}

type fuzzEffectRunnerProber struct {
	*fuzzEffectRunner
	wf.DetachProber
}

func newFuzzEffectRunner(r wf.EffectRunner, w *World) wf.EffectRunner {
	wrapped := &fuzzEffectRunner{EffectRunner: r, world: w, results: map[string]wf.DetachedReport{}, processes: map[string]wf.DetachedProcess{}}
	if prober, ok := r.(wf.DetachProber); ok {
		return &fuzzEffectRunnerProber{fuzzEffectRunner: wrapped, DetachProber: prober}
	}
	return wrapped
}

func (r *fuzzEffectRunner) Start(req wf.DetachedRequest) (wf.DetachedProcess, error) {
	// Detached commands always have an engine maximum and timeout exit 124.
	effect := wf.ExternalEffect{Adapter: "shell", Op: "run", Args: map[string]any{"argv": req.Argv, "Timeout": "detached maximum"}}
	r.world.mu.Lock()
	class := r.world.nextFuzzFault(req.Context, effect)
	good := r.world.fuzzGood
	var scripted json.RawMessage
	var hasScript bool
	if class == "" {
		scripted, hasScript = r.world.nextFuzzResult(req.Context, effect)
	}
	r.world.mu.Unlock()
	if class == "" && !good && !hasScript {
		return r.EffectRunner.Start(req)
	}
	var result wf.RunResult
	if class != "" {
		raw, err := effectFaultResult(class, effect)
		r.world.mu.Lock()
		if r.world.fuzzInjected != nil {
			r.world.fuzzInjected()
		}
		r.world.mu.Unlock()
		if err != nil {
			return wf.DetachedProcess{}, err
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return wf.DetachedProcess{}, err
		}
	} else if hasScript {
		if err := json.Unmarshal(scripted, &result); err != nil {
			return wf.DetachedProcess{}, err
		}
	} else {
		r.world.mu.Lock()
		result.Stdout = r.world.stdoutOf(req.Argv)
		r.world.mu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := wf.DetachedProcess{Pid: 1000000 + len(r.results) + 1, Pgid: 1000000 + len(r.results) + 1}
	report := wf.DetachedReport{Status: wf.DetachedPresent, Result: result}
	if class == "effect-timeout" {
		report.Status = wf.DetachedRunning
	}
	r.results[req.ResultPath], r.processes[req.ResultPath] = report, p
	return p, nil
}

func (r *fuzzEffectRunner) Status(p wf.DetachedProcess, req wf.DetachedRequest) (wf.DetachedReport, error) {
	r.mu.Lock()
	result, ok := r.results[req.ResultPath]
	r.mu.Unlock()
	if ok {
		return result, nil
	}
	return r.EffectRunner.Status(p, req)
}
func (r *fuzzEffectRunner) Launched(req wf.DetachedRequest) (wf.DetachedProcess, bool, error) {
	r.mu.Lock()
	p, ok := r.processes[req.ResultPath]
	r.mu.Unlock()
	if ok {
		return p, true, nil
	}
	return r.EffectRunner.Launched(req)
}
func (r *fuzzEffectRunner) Terminate(p wf.DetachedProcess, req wf.DetachedRequest) error {
	r.mu.Lock()
	report, ok := r.results[req.ResultPath]
	if ok {
		report.Status = wf.DetachedPresent
		r.results[req.ResultPath] = report
	}
	r.mu.Unlock()
	if ok {
		return nil
	}
	return r.EffectRunner.Terminate(p, req)
}
func (r *fuzzEffectRunner) GroupEmpty(p wf.DetachedProcess, req wf.DetachedRequest) (bool, error) {
	r.mu.Lock()
	report, ok := r.results[req.ResultPath]
	r.mu.Unlock()
	if ok {
		return report.Status != wf.DetachedRunning, nil
	}
	return r.EffectRunner.GroupEmpty(p, req)
}
