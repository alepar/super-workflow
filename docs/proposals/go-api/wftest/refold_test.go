package wftest_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// A run's journal is its whole state. TestRefoldEveryPrefix runs definitions to
// their end, then for every prefix of the finished journal starts a fresh
// engine on only that prefix and drives the rest of the script: each reaches
// the terminal the full run reached.

type refoldCase struct {
	name   string
	def    func() *wf.Def
	opts   any
	world  func(*wftest.World)
	script map[string][]string // answers by state name, in the order they are given
}

func refoldCases() []refoldCase {
	loopWorld := func(w *wftest.World) { wftest.Set(w, git.Head("."), "abc123") }
	return []refoldCase{
		{"loop accepted first time", func() *wf.Def { d, _, _, _ := loopDef(); return d }, Opts{Autonomous: true}, loopWorld,
			map[string][]string{"DraftS": {`{"Verdict":"pass"}`}}},
		{"loop refused then accepted", func() *wf.Def { d, _, _, _ := loopDef(); return d }, Opts{Autonomous: true}, loopWorld,
			map[string][]string{"DraftS": {`{"Verdict":"fail"}`, `{"Verdict":"pass"}`}}},
		{"map of three children then a final agent", func() *wf.Def { d, _, _, _ := pagedDef(); return d }, nil, nil,
			map[string][]string{"pgWork": {`{"OK":true}`, `{"OK":true}`, `{"OK":true}`}, "pgFinal": {`{"OK":true}`}}},
		{"quorum of one over three seats", func() *wf.Def { return crFanDef(3, wf.Quorum(1)) }, nil, nil,
			map[string][]string{"crFWork": {`{"OK":true}`, `{"OK":true}`, `{"OK":true}`}}},
	}
}

// baseState is the last path element of an instruction's qualified state.
func baseState(state string) string { return state[strings.LastIndex(state, "/")+1:] }

// driveScript answers every open instruction from the script until none is
// left. The queues are consumed in place.
func driveScript(t *testing.T, e wf.Executor, id wf.RunID, script map[string][]string) {
	t.Helper()
	for range 100 {
		ins, err := e.Next(id, "")
		if err != nil {
			t.Fatal(err)
		}
		ins = slices.DeleteFunc(slices.Clone(ins), func(in wf.Instruction) bool { return in.Kind == wf.InstructionEnded })
		if len(ins) == 0 {
			return
		}
		for _, in := range ins {
			if in.Kind == wf.InstructionManualVerification {
				// An effect with no result in the journal and no way to ask the
				// world whether it ran: the operator reports that it did. A
				// command is settled with the result the operator saw.
				if err := e.(*wf.Engine).SettleAs(id, in.Effect, true, nil, "test", wf.Operator{Name: "t", Reason: "t", Harness: "generic"}); err != nil {
					if err := e.(*wf.Engine).SettleAs(id, in.Effect, true, &wf.RunResult{}, "test", wf.Operator{Name: "t", Reason: "t", Harness: "generic"}); err != nil {
						t.Fatal(err)
					}
				}
				continue
			}
			q := script[baseState(in.State)]
			if len(q) == 0 {
				t.Fatalf("no answer left for %s (attempt %d)", in.State, in.Attempt)
			}
			script[baseState(in.State)] = q[1:]
			if _, err := e.Submit(id, wf.Submission{Token: in.Token, Child: in.Child, Output: json.RawMessage(q[0])}); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("the script did not settle")
}

func cloneScript(s map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range s {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// terminalOf is the terminal of a run's own run_ended record, "" while it runs.
func terminalOf(recs []wf.JournalRecord) string {
	for _, r := range recs {
		var d wf.RunEndedData
		if r.Kind == wf.JournalRunEnded && r.Instance == "" && json.Unmarshal(r.Data, &d) == nil {
			return d.Terminal
		}
	}
	return ""
}

func TestRefoldEveryPrefix(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range refoldCases() {
		t.Run(c.name, func(t *testing.T) {
			engine := func(j wf.Journal, d *wf.Def, now time.Time) wf.Executor {
				w := wftest.NewWorld()
				if c.world != nil {
					c.world(w)
				}
				reg := wf.NewAdapters()
				if err := reg.Register(w); err != nil {
					t.Fatal(err)
				}
				return wf.NewEngine(j, reg, wf.EngineOptions{Defs: []*wf.Def{d}, RunID: "refold", Clock: func() time.Time { return now }})
			}
			full := wf.MemJournal()
			d := c.def()
			e := engine(full, d, t0)
			id, err := e.Run(d, nil, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			driveScript(t, e, id, cloneScript(c.script))
			unlock, err := full.Lock(id)
			if err != nil {
				t.Fatal(err)
			}
			recs, err := full.Records(id)
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			want := terminalOf(recs)
			if want == "" {
				t.Fatalf("the full run did not end (%d records)", len(recs))
			}
			for k := 0; k <= len(recs); k++ {
				j := wf.MemJournal()
				submitted := map[string]int{}
				unlock, err := j.Lock(id)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range recs[:k] {
					if r.Kind == wf.JournalSubmitted {
						submitted[r.State]++
					}
					if err := j.Append(r); err != nil {
						t.Fatal(err)
					}
				}
				unlock()
				rest := cloneScript(c.script)
				for state, n := range submitted {
					rest[state] = rest[state][min(n, len(rest[state])):]
				}
				// A new process, long after the prefix: every window has run out.
				d := c.def()
				e := engine(j, d, t0.Add(time.Hour))
				if _, err := e.Run(d, nil, c.opts); err != nil {
					t.Fatalf("prefix %d of %d (ends after %s): %v", k, len(recs), lastKind(recs[:k]), err)
				}
				driveScript(t, e, id, rest)
				unlock, err = j.Lock(id)
				if err != nil {
					t.Fatal(err)
				}
				got, err := j.Records(id)
				unlock()
				if err != nil {
					t.Fatal(err)
				}
				if terminalOf(got) != want {
					t.Errorf("prefix %d of %d (ends after %s): terminal %q, want %q", k, len(recs), lastKind(recs[:k]), terminalOf(got), want)
				}
			}
		})
	}
}

func lastKind(recs []wf.JournalRecord) string {
	if len(recs) == 0 {
		return "nothing"
	}
	return recs[len(recs)-1].Kind.String()
}
