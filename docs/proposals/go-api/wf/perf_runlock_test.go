package wf_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Run-lock perf harness. Skipped unless SW_PERF is set; it times verbs the
// way the CLI runs them: a fresh engine (journal reader, registry, caches)
// per verb, over a file journal.
//
//	python3 scripts/perf-runlock-repo.py <dir>  # 310 MB history, 1 GB ignored target/
//	SW_PERF=1 SW_PERF_REPO=<dir> go test -run TestPerfCheckpoint -v ./wf
//	SW_PERF=1 SW_PERF_N=400 go test -run TestPerfJournal -v ./wf

func perfEnabled(t *testing.T) {
	if os.Getenv("SW_PERF") == "" {
		t.Skip("SW_PERF not set")
	}
}

func perfInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

type perfRig struct {
	d         *wf.Def
	ws, state string
	snapshots bool
	withGit   bool
	shared    *wf.Engine
}

func (p perfRig) engine(t *testing.T) *wf.Engine {
	if p.shared != nil {
		return p.shared
	}
	reg := wf.NewAdapters()
	if p.withGit {
		if err := reg.Register(git.Runtime(git.Config{})); err != nil {
			t.Fatal(err)
		}
	}
	return wf.NewEngine(wf.FileJournal(p.state), reg, wf.EngineOptions{Defs: []*wf.Def{p.d}, Workspace: p.ws, StateRoot: p.state, Snapshots: p.snapshots,
		RunDir: func(r wf.RunID) string { return filepath.Join(p.state, "runs", string(r)) }}).(*wf.Engine)
}

func perfDu(dir string) string {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return fmt.Sprintf("%dMB", n>>20)
}

func perfStats(name string, ds []time.Duration) string {
	if len(ds) == 0 {
		return name + ": none"
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	return fmt.Sprintf("%s: n=%d mean=%v p50=%v p90=%v max=%v", name, len(s), (sum / time.Duration(len(s))).Round(time.Millisecond),
		s[len(s)/2].Round(time.Millisecond), s[len(s)*9/10].Round(time.Millisecond), s[len(s)-1].Round(time.Millisecond))
}

// TestPerfCheckpoint times a next that issues one unit, with snapshots on,
// over the git workspace SW_PERF_REPO: each is a fresh cycle, so each takes a
// full world capture with no live children.
func TestPerfCheckpoint(t *testing.T) {
	perfEnabled(t)
	ws := os.Getenv("SW_PERF_REPO")
	if ws == "" {
		t.Skip("SW_PERF_REPO not set")
	}
	n := perfInt("SW_PERF_N", 5)
	ns := make([]int, n)
	for i := range ns {
		ns[i] = i + 1
	}
	rig := perfRig{d: fanDef("perf-ckpt", chItems(ns...), 1, wf.AllSettled()), ws: ws, state: t.TempDir(), snapshots: true, withGit: true}
	start := time.Now()
	run, err := rig.engine(t).Run(rig.d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("run: %v", time.Since(start).Round(time.Millisecond))
	var nexts, submits []time.Duration
	for i := 0; i < n; i++ {
		start = time.Now()
		ins, err := rig.engine(t).Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		nexts = append(nexts, time.Since(start))
		for _, in := range ins {
			if os.Getenv("SW_PERF_CHURN") != "" {
				// One small commit since the last checkpoint.
				f := filepath.Join(ws, fmt.Sprintf("churn-%d.txt", i))
				if err := os.WriteFile(f, []byte(time.Now().String()), 0o644); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{{"add", f}, {"commit", "-q", "-m", "churn"}} {
					if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
						t.Fatalf("%v %s", err, out)
					}
				}
			}
			start = time.Now()
			if _, err := rig.engine(t).Submit(run, wf.Submission{Token: in.Token, Output: []byte(`{"OK":true}`)}); err != nil {
				t.Fatal(err)
			}
			submits = append(submits, time.Since(start))
		}
	}
	t.Log(perfStats("next(checkpoint)", nexts))
	t.Log(perfStats("submit", submits))
	recs, _ := wf.ReadJournal(rig.state, run)
	t.Logf("records=%d store=%s", len(recs), perfDu(filepath.Join(rig.state, "runs", string(run), "snapshots")))
}

// TestPerfJournal grows a journal through SW_PERF_N children run two at a
// time and reports how a verb's cost grows with it. Snapshots are off, so
// this is the journal read and fold alone.
func TestPerfJournal(t *testing.T) {
	perfEnabled(t)
	n := perfInt("SW_PERF_N", 400)
	ns := make([]int, n)
	for i := range ns {
		ns[i] = i + 1
	}
	rig := perfRig{d: fanDef("perf-journal", chItems(ns...), 2, wf.AllSettled()), ws: t.TempDir(), state: t.TempDir()}
	if os.Getenv("SW_PERF_SHARED") != "" {
		// One engine for every verb: what a process repeats across its own
		// lock acquisitions (a CLI verb takes several).
		shared := rig.engine(t)
		rig.shared = shared
	}
	run, err := rig.engine(t).Run(rig.d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	type sample struct {
		recs         int
		next, submit time.Duration
		status       time.Duration
	}
	var samples []sample
	for i := 0; ; i++ {
		start := time.Now()
		ins, err := rig.engine(t).Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		nd := time.Since(start)
		ins = withoutEnded(ins)
		if len(ins) == 0 {
			break
		}
		var sd time.Duration
		for _, in := range ins {
			start = time.Now()
			if _, err := rig.engine(t).Submit(run, wf.Submission{Token: in.Token, Output: []byte(`{"OK":true}`)}); err != nil {
				t.Fatal(err)
			}
			sd += time.Since(start)
		}
		sd /= time.Duration(len(ins))
		start = time.Now()
		if _, err := rig.engine(t).Status(run); err != nil {
			t.Fatal(err)
		}
		std := time.Since(start)
		recs, _ := wf.ReadJournal(rig.state, run)
		samples = append(samples, sample{len(recs), nd, sd, std})
	}
	for i, s := range samples {
		if i%(len(samples)/8+1) == 0 || i == len(samples)-1 {
			t.Logf("records=%5d next=%7v submit=%7v status=%7v", s.recs, s.next.Round(time.Microsecond*100), s.submit.Round(time.Microsecond*100), s.status.Round(time.Microsecond*100))
		}
	}
	fi, _ := os.Stat(filepath.Join(rig.state, "runs", string(run), "journal.jsonl"))
	if fi != nil {
		t.Logf("journal bytes=%d", fi.Size())
	}
}
