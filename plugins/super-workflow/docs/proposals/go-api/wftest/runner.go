package wftest

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// FakeRunner is a scripted wf.EffectRunner for a Sim: no process is started.
//
// Owns: per detached intent, a leader (alive or gone) and a member count that
// obey the shell runner's group rule: Status is Running only while the leader
// lives, DeadLive while it is gone and members remain, Dead when both are
// gone, and Present when SetStatus scripted a result; Terminate on a non-empty
// group kills the leader and the members; GroupEmpty is true with neither. It
// also owns whether Launched finds a proc record, and a record of every launch
// and termination. Intent 0 in a setter is the default for every intent
// without its own entry. A launch has a live leader and no members until a
// setter says otherwise, and SetStatus(Present, Dead or DeadLive) means the
// leader is gone.
// Does not: write result files, signal anything or read a clock.
// Extend by: a new scripted behaviour is one setter here.
type FakeRunner struct {
	mu         sync.Mutex
	started    []wf.DetachedRequest
	intentOf   map[int]int64 // pid to the intent it was launched for
	procOf     map[string]wf.DetachedProcess
	status     map[int64]wf.DetachedStatus
	result     map[int64]wf.RunResult
	leader     map[int64]bool
	members    map[int64]int
	procOK     map[int64]bool
	terminated []int64
	startErr   error
}

// NewFakeRunner returns a runner whose launches are all Running.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{intentOf: map[int]int64{}, procOf: map[string]wf.DetachedProcess{}, status: map[int64]wf.DetachedStatus{}, result: map[int64]wf.RunResult{},
		leader: map[int64]bool{}, members: map[int64]int{}, procOK: map[int64]bool{}}
}

// SetStatus makes the detached effect of intent report st; for DetachedPresent
// r is the RunResult the result file holds.
func (f *FakeRunner) SetStatus(intent int64, st wf.DetachedStatus, r wf.RunResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[intent], f.result[intent] = st, r
}

// SetLeader makes the leader of intent's process group alive or gone.
func (f *FakeRunner) SetLeader(intent int64, alive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leader[intent] = alive
}

// SetMembers sets how many members besides the leader the group of intent has.
func (f *FakeRunner) SetMembers(intent int64, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[intent] = n
}

// SetProc makes Launched find, or not find, the proc record of intent.
func (f *FakeRunner) SetProc(intent int64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procOK[intent] = ok
}

// SetGroupEmpty makes the process group of intent empty or not: not empty is
// one live member.
func (f *FakeRunner) SetGroupEmpty(intent int64, empty bool) {
	n := 1
	if empty {
		n = 0
	}
	f.SetMembers(intent, n)
}

// SetStartError makes Start fail with err (an Unavailable launch when err wraps
// wf.ErrUnavailable).
func (f *FakeRunner) SetStartError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErr = err
}

// Started lists the launches in order.
func (f *FakeRunner) Started() []wf.DetachedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wf.DetachedRequest(nil), f.started...)
}

// Terminated lists the intent of each Terminate call in order.
func (f *FakeRunner) Terminated() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.terminated...)
}

// lookup is m[intent], else m[0].
func lookup[V any](m map[int64]V, intent int64) (V, bool) {
	if v, ok := m[intent]; ok {
		return v, true
	}
	v, ok := m[0]
	return v, ok
}

func intentOfPath(resultPath string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSuffix(filepath.Base(resultPath), ".json"), 10, 64)
	return n
}

// Start implements wf.EffectRunner. The intent is the base name of the result
// path, which the Engine builds from the effect_intent seq.
func (f *FakeRunner) Start(r wf.DetachedRequest) (wf.DetachedProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return wf.DetachedProcess{}, f.startErr
	}
	f.started = append(f.started, r)
	n := len(f.started)
	p := wf.DetachedProcess{Pid: 1000 + n, Pgid: 1000 + n, StartTicks: uint64(1000 + n)}
	i := intentOfPath(r.ResultPath)
	f.intentOf[p.Pid] = i
	f.procOf[r.ProcPath] = p
	return p, nil
}

// Launched implements wf.EffectRunner: the proc record of a launch exists
// unless SetProc(intent, false) says otherwise.
func (f *FakeRunner) Launched(r wf.DetachedRequest) (wf.DetachedProcess, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procOf[r.ProcPath]
	if !ok {
		return wf.DetachedProcess{}, false, nil
	}
	if v, set := lookup(f.procOK, f.intentOf[p.Pid]); set && !v {
		return wf.DetachedProcess{}, false, nil
	}
	return p, true, nil
}

// group is the leader and member count of intent i: a scripted status other
// than Running means the leader is gone unless SetLeader says otherwise.
func (f *FakeRunner) group(i int64) (leader bool, members int) {
	leader, ok := f.leader[i]
	if !ok {
		leader, ok = f.leader[0]
	}
	if !ok {
		st, _ := lookup(f.status, i)
		leader = st == 0 || st == wf.DetachedRunning
	}
	members, _ = lookup(f.members, i)
	return leader, members
}

// Status implements wf.EffectRunner.
func (f *FakeRunner) Status(p wf.DetachedProcess, _ wf.DetachedRequest) (wf.DetachedReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.intentOf[p.Pid]
	if st, ok := lookup(f.status, i); ok && st != 0 {
		r, _ := lookup(f.result, i)
		return wf.DetachedReport{Status: st, Result: r}, nil
	}
	leader, members := f.group(i)
	switch {
	case leader:
		return wf.DetachedReport{Status: wf.DetachedRunning}, nil
	case members > 0:
		return wf.DetachedReport{Status: wf.DetachedDeadLive}, nil
	}
	return wf.DetachedReport{Status: wf.DetachedDead}, nil
}

// Terminate implements wf.EffectRunner: a non-empty group loses its leader and
// its members.
func (f *FakeRunner) Terminate(p wf.DetachedProcess, _ wf.DetachedRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.intentOf[p.Pid]
	f.terminated = append(f.terminated, i)
	if leader, members := f.group(i); leader || members > 0 {
		f.leader[i], f.members[i] = false, 0
	}
	return nil
}

// GroupEmpty implements wf.EffectRunner.
func (f *FakeRunner) GroupEmpty(p wf.DetachedProcess, _ wf.DetachedRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	leader, members := f.group(f.intentOf[p.Pid])
	return !leader && members == 0, nil
}
