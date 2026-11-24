package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func jsonOut(t *testing.T, s string) wf.Submission {
	t.Helper()
	return wf.Submission{Output: json.RawMessage(s)}
}

// quorumEarlyDecide runs the early-decide split for join over three seats:
// seats 1 and 3 are started, seat 2 stays handed, and seat 1's success meets
// the join.
func quorumEarlyDecide(t *testing.T, join wf.JoinPolicy) {
	t.Helper()
	d := fanDef("quorum-"+t.Name(), chItems(1, 2, 3), 3, join)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	seats := h.handOut(run, 3)
	if len(seats) != 3 {
		t.Fatalf("HandOut(3) = %d units, want 3", len(seats))
	}
	byChild := map[string]string{}
	for _, s := range seats {
		byChild[s.Child] = s.Name
	}
	one, two, three := byChild["chFan[1]"], byChild["chFan[2]"], byChild["chFan[3]"]
	for _, n := range []string{one, two, three} {
		if n == "" {
			t.Fatalf("hand-out names %v lack a seat", byChild)
		}
	}
	for i, n := range []string{one, three} {
		agent := []string{"a1", "a3"}[i]
		if err := h.e.Started(run, n, claim(agent)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.e.Prompt(run, n); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := h.e.Done(run, one, jsonOut(t, `{"OK":true}`)); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")

	var withdrawn []wf.WithdrawnData
	for _, r := range h.kind(run, wf.JournalWithdrawn) {
		var w wf.WithdrawnData
		if err := json.Unmarshal(r.Data, &w); err != nil {
			t.Fatal(err)
		}
		withdrawn = append(withdrawn, w)
	}
	if len(withdrawn) != 1 || withdrawn[0].Name != two || withdrawn[0].Reason != "join decided" {
		t.Fatalf("withdrawn = %+v, want only the handed seat %s", withdrawn, two)
	}
	running := 0
	for _, r := range h.kind(run, wf.JournalChildSettled) {
		var c wf.ChildSettledData
		if err := json.Unmarshal(r.Data, &c); err != nil {
			t.Fatal(err)
		}
		if c.Running {
			running++
			if c.ID != "chFan[3]" {
				t.Fatalf("child %s settled Running, want only the started seat chFan[3]", c.ID)
			}
		}
	}
	if running != 1 {
		t.Fatalf("Running settles = %d, want 1", running)
	}
	if err := h.e.Started(run, two, claim("late")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Started on the withdrawn seat = %v, want ErrRejected", err)
	}

	accepted := h.kind(run, wf.JournalAccepted)
	transitions := len(h.kind(run, wf.JournalTransition))
	before := h.status(run).Tokens
	late := jsonOut(t, `{"OK":true}`)
	late.Tokens = 14755
	res, err := h.e.Done(run, three, late)
	if err != nil || res.Class != wf.Accepted {
		t.Fatalf("late Done = %+v, %v, want Accepted", res, err)
	}
	votes := h.kind(run, wf.JournalLateVote)
	if len(votes) != 1 {
		t.Fatalf("late_vote records = %d, want 1", len(votes))
	}
	var v wf.LateVoteData
	if err := json.Unmarshal(votes[0].Data, &v); err != nil || v.Name != three || string(v.Output) != `{"OK":true}` || v.Tokens != 14755 {
		t.Fatalf("late_vote = %+v (%v), want seat %s with its output and its reported 14755 tokens", v, err, three)
	}
	// The reported usage is the run's, counted once, here and after a replay.
	if got := h.status(run).Tokens; got != before+14755 {
		t.Fatalf("run tokens after the late vote = %d, want %d", got, before+14755)
	}
	if got := h.kind(run, wf.JournalAccepted); len(got) != len(accepted) || len(h.kind(run, wf.JournalTransition)) != transitions {
		t.Fatalf("a late vote moved the decision: %d accepted records (was %d)", len(got), len(accepted))
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
	if _, err := h.e.Done(run, three, jsonOut(t, `{"OK":true}`)); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("second Done on the late voter = %v, want ErrRejected", err)
	}
	if n := len(h.kind(run, wf.JournalLateVote)); n != 1 {
		t.Fatalf("late_vote records after a rejected repeat = %d, want 1", n)
	}
	if got := h.status(run).Tokens; got != before+14755 {
		t.Fatalf("run tokens after a rejected repeat = %d, want %d", got, before+14755)
	}
}

func TestQuorumWithdrawsPendingRecordsLateVote(t *testing.T) { quorumEarlyDecide(t, wf.Quorum(1)) }

func TestJoinWhenEarlyDecide(t *testing.T) {
	quorumEarlyDecide(t, wf.JoinWhen("one accepted", func(r wf.MapResult[string, chRes]) bool {
		for _, it := range r.Items {
			if it.Class == wf.Accepted {
				return true
			}
		}
		return false
	}))
}

func TestLateFailureWithdrawsStartedSeat(t *testing.T) {
	d := fanDef("quorum-late-fail", chItems(1, 2), 2, wf.Quorum(1))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	seats := h.handOut(run, 2)
	for i, s := range seats {
		if err := h.e.Started(run, s.Name, claim([]string{"a", "b"}[i])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.e.Done(run, seats[0].Name, jsonOut(t, `{"OK":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Failed(run, seats[1].Name, "crashed"); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
	if _, err := h.e.Done(run, seats[1].Name, jsonOut(t, `{"OK":true}`)); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Done after a late failure = %v, want ErrRejected", err)
	}
	if n := len(h.kind(run, wf.JournalLateVote)); n != 0 {
		t.Fatalf("late_vote records = %d, want 0 after a late failure", n)
	}
}

func TestCancelSiblingsLateDoneRejected(t *testing.T) {
	cancel := func(m *wf.Map[chFan, chItem, string, chRes]) { m.OnSiblingFailure(wf.CancelSiblings) }
	failOne := func(t *testing.T, seats []wf.Instruction, done func(name, out string) error) {
		t.Helper()
		if err := done(seats[0].Name, `{"OK":false}`); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("engine", func(t *testing.T) {
		d := fanDef("cancel-late-engine", chItems(1, 2), 2, wf.AllOK(), cancel)
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		seats := h.handOut(run, 2)
		for i, s := range seats {
			if err := h.e.Started(run, s.Name, claim([]string{"a", "b"}[i])); err != nil {
				t.Fatal(err)
			}
		}
		failOne(t, seats, func(name, out string) error {
			_, err := h.e.Done(run, name, jsonOut(t, out))
			return err
		})
		statusOf(t, h.status(run), wf.RunEnded, "chPartial")
		if n := len(h.kind(run, wf.JournalLateVote)); n != 0 {
			t.Fatalf("late_vote records = %d: a cancelled sibling must not vote", n)
		}
		if _, err := h.e.Done(run, seats[1].Name, jsonOut(t, `{"OK":true}`)); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("Done on the cancelled sibling = %v, want ErrRejected", err)
		}
	})

	t.Run("cli", func(t *testing.T) {
		d := fanDef("cancel-late-cli", chItems(1, 2), 2, wf.AllOK(), cancel)
		cfg := wf.MainConfig{Adapters: wf.NewAdapters(), Defs: []wf.Registration{{Def: d}}}
		state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
		call := func(args ...string) (int, string, string) {
			var o, er bytes.Buffer
			code := fixtureCLI(cfg, append(args, "--state-dir", state), &o, &er)
			return code, o.String(), er.String()
		}
		code, out, er := call("run", d.Name(), "--workspace", ws, "--run-id", d.Name()+"-1")
		if code != 0 {
			t.Fatalf("run exited %d: %s", code, er)
		}
		run := strings.TrimSpace(out)
		code, out, er = call("next", run, "2", "--json")
		var seats []wf.Instruction
		if err := json.Unmarshal([]byte(out), &seats); err != nil || code != 0 || len(seats) != 2 {
			t.Fatalf("next 2 = %d %q (%v) %s", code, out, err, er)
		}
		for i, s := range seats {
			if code, _, er := call("dev", "started", run, s.Name, "--harness", "claude", "--session", "s1", "--agent", []string{"a", "b"}[i], "--transcript", "/t.jsonl"); code != 0 {
				t.Fatalf("started %s exited %d: %s", s.Name, code, er)
			}
		}
		outFile := func(content string) string {
			f := filepath.Join(t.TempDir(), "out.json")
			if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			return f
		}
		if code, _, er := call("done", run, seats[0].Name, "--output", writeAllocated(t, seats[0].Output, `{"OK":false}`)); code != 0 {
			t.Fatalf("done on the failing sibling exited %d: %s", code, er)
		}
		if code, _, er := call("done", run, seats[1].Name, "--output", outFile(`{"OK":true}`)); code != 4 {
			t.Fatalf("done on the cancelled sibling exited %d, want 4: %s", code, er)
		}
	})
}
