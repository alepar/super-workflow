package wf_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A command whose working directory is missing (a denied mkdir, a swept
// tree) is refused naming that directory, before anything is executed, and
// the coordinator's wait text and status say so: the host's own report,
// "fork/exec /usr/bin/bash: no such file or directory", blames the program
// instead.
func TestCommandInAMissingDirectoryNamesTheDirectory(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := map[bool]string{false: "inline", true: "detached"}[detached]
		t.Run(name, func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "never-made")
			d := detDef(func(c *wf.Command[dRun]) {
				c.Idempotent().In(missing)
				if detached {
					c.Detached()
				}
			})
			fr := newDetRunner()
			var h *harness
			if detached {
				h = newHarnessOpts(t, d, directoryCheckingWorld{newWorld()}, detOpts(t, fr))
			} else {
				h = newHarnessOpts(t, d, shell.Runtime(shell.Config{}), wf.EngineOptions{})
			}
			run := h.run(d, map[string]string{"topic": "build"})
			h.next(run)
			results := resultsOf(t, h, run)
			if len(results) != 1 {
				t.Fatalf("effect results %+v", results)
			}
			r := results[0]
			if r.OK || !strings.Contains(r.Error, missing) || strings.Contains(r.Error, "fork/exec") {
				t.Fatalf("result %+v, want a refusal naming %s", r, missing)
			}
			if fr.startCount() != 0 {
				t.Fatalf("a detached runner was started %d times", fr.startCount())
			}
			// Whoever drives the run sees it: the wait text and status name
			// the directory, not only the journal.
			// The detached case's test world words its own check.
			want := "after an unavailable failure: "
			if !detached {
				want += "adapter unavailable: command working directory " + missing + " does not exist (delay 1"
			}
			ins := h.next(run)
			if len(ins) != 1 || ins[0].Kind != wf.InstructionWait || !strings.Contains(ins[0].Prompt, want) || !strings.Contains(ins[0].Prompt, missing) {
				t.Fatalf("wait %+v, want its text to contain %q and name %s", ins, want, missing)
			}
			st := h.status(run)
			if len(st.Instances) != 1 || !strings.Contains(st.Instances[0].Reason, missing) {
				t.Fatalf("status instances %+v, want the reason to name %s", st.Instances, missing)
			}
		})
	}
}

// Each backoff shows its own failure: a later schedule on the same visit
// whose record carries no reasons (a journal from before them) does not keep
// showing the earlier failure in the wait text or status.
func TestALaterBackoffDoesNotShowAnEarlierFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-made")
	d := detDef(func(c *wf.Command[dRun]) { c.Idempotent().In(missing) })
	h := newHarnessOpts(t, d, shell.Runtime(shell.Config{}), wf.EngineOptions{})
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	recs := h.records(run)
	last := recs[len(recs)-1]
	if last.Kind != wf.JournalBackoffScheduled {
		t.Fatalf("last record %s, want the first backoff", last.Kind)
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(wf.BackoffScheduledData{Class: wf.Unavailable, Index: 1, Delay: time.Minute, Due: h.now.Add(time.Minute)})
	err = h.j.Append(wf.JournalRecord{Kind: wf.JournalBackoffScheduled, Run: run, Instance: last.Instance, State: last.State, Class: wf.Unavailable, DefHash: last.DefHash, At: h.now, Data: data})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionWait || !strings.Contains(ins[0].Prompt, "(delay 2") || strings.Contains(ins[0].Prompt, missing) {
		t.Fatalf("wait %+v, want the second schedule without the first failure", ins)
	}
	if st := h.status(run); len(st.Instances) != 1 || strings.Contains(st.Instances[0].Reason, missing) {
		t.Fatalf("status instances %+v, want no stale reason", st.Instances)
	}
}
