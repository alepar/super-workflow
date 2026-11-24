package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A call that carries a claim token is held to it whoever makes it. From a
// shell with no harness environment, a unit named with another live unit's
// token, or with a token no hand-out minted, is refused for prompt, validate,
// done and failed, before and after the units are claimed, and writes
// nothing. The units' own tokens still finish them.
func TestATokenMustBeItsUnitsFromAnyCaller(t *testing.T) {
	strictCallers(t)
	def := fanDef("token-form", chItems(1, 2), 2, wf.AllSettled())
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "2", "--json", "--state-dir", state)), &ins); err != nil || len(agentsOf(ins)) != 2 {
		t.Fatalf("next: %v %v", ins, err)
	}
	a, b := agentsOf(ins)[0], agentsOf(ins)[1]
	// A plain shell: no harness environment at all.
	for _, k := range []string{"CODEX_SESSION_ID", "CODEX_THREAD_ID", "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	journal := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(state, "runs", "r1", "journal.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	out := writeAllocated(t, b.Output, `{"OK":true}`)
	refused := func(when string) {
		t.Helper()
		before := journal()
		for _, token := range []string{claimToken(t, a), "0000000000000000"} {
			for _, args := range [][]string{
				{"wf", "prompt", b.Name},
				{"wf", "validate", b.Name, "--output", out},
				{"wf", "done", b.Name, "--output", out},
				{"wf", "failed", b.Name, "--reason", "x"},
			} {
				code, _, er := singleCLI(def, append(args, "--claim", token, "--state-dir", state)...)
				if code != 4 {
					t.Errorf("%s: %v with token %s: exit %d, %q; want refused", when, args[1], token, code, er)
				}
			}
		}
		if after := journal(); after != before {
			t.Errorf("%s: the refused calls appended to the journal:\n%s", when, strings.TrimPrefix(after, before))
		}
	}
	refused("before any claim")
	mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, a), "--state-dir", state)
	mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, b), "--state-dir", state)
	refused("with both units claimed")
	mustSingle(t, def, "wf", "validate", b.Name, "--claim", claimToken(t, b), "--output", out, "--state-dir", state)
	mustSingle(t, def, "wf", "done", b.Name, "--claim", claimToken(t, b), "--output", out, "--state-dir", state)
	mustSingle(t, def, "wf", "failed", a.Name, "--claim", claimToken(t, a), "--reason", "x", "--state-dir", state)
}

// lockHookJournal runs hook just before its at-th Lock, outside any lock: a
// writer that gets in between a verb's dry checks and its mutation cycle.
type lockHookJournal struct {
	wf.Journal
	n, at int
	hook  func()
}

func (j *lockHookJournal) Lock(run wf.RunID) (func(), error) {
	if j.n++; j.n == j.at {
		j.hook()
	}
	return j.Journal.Lock(run)
}

// The token is checked again inside the cycle that settles. A done or failed
// passes both of its dry checks while the unit is bound to no token; then,
// before the verb takes the lock it mutates under (its third), another token
// binds the unit. The verb is refused under that lock and settles nothing.
func TestATokenBoundAwayBetweenCheckAndSettleIsRefused(t *testing.T) {
	for _, verb := range []string{"done", "failed"} {
		t.Run(verb, func(t *testing.T) {
			d := lsDef("token-race-"+verb, true)
			h := newHarnessOpts(t, d, nil, lsOpts())
			run, name, t1, t2 := reofferedUnit(t, h, d)
			if _, err := h.e.Prompt(run, name); err != nil { // claimed, bound to no token yet
				t.Fatal(err)
			}
			settle := func(e *wf.Engine) error {
				if verb == "done" {
					_, err := e.Done(run, name, wf.Submission{Output: json.RawMessage(`{}`)})
					return err
				}
				_, err := e.Failed(run, name, "x")
				return err
			}
			raced := false
			j := &lockHookJournal{Journal: h.j, at: 3, hook: func() {
				raced = true
				if _, err := h.e.PromptClaim(run, name, t2); err != nil {
					t.Errorf("the racing bind: %v", err)
				}
			}}
			opts := lsOpts()
			opts.Clock, opts.Defs = func() time.Time { return h.now }, []*wf.Def{d}
			e := wf.NewEngine(j, h.reg, opts).(*wf.Engine)
			wf.SetChildClaim(e, t1)
			before := len(h.records(run))
			err := settle(e)
			if !raced {
				t.Fatalf("%s took %d locks: the race was never run", verb, j.n)
			}
			if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "another child already owns this unit") {
				t.Fatalf("%s with the token that lost the race = %v, want refused under the lock", verb, err)
			}
			// The racing fetch's own records, and nothing of the refused verb.
			for _, r := range h.records(run)[before:] {
				if r.Kind != wf.JournalPromptFetched && r.Kind != wf.JournalAttemptStarted && r.Kind != wf.JournalMetadataAttached {
					t.Fatalf("the refused %s wrote a %s record", verb, r.Kind)
				}
			}
			wf.SetChildClaim(e, t2)
			if err := settle(e); err != nil {
				t.Fatalf("%s with the binding token: %v", verb, err)
			}
		})
	}
}

// Another caller than the first fetcher, holding the unit's token, settles
// the unit (a noted mismatch) and loses the reply. Its identical retry is
// matched on the token and the recorded result, not on who fetched first: a
// done is told it was accepted, a failed that it is recorded, and neither
// appends anything. A failed with another reason is still refused.
func TestRetryAfterALostReplyFromAnotherCaller(t *testing.T) {
	journal := func(t *testing.T, d *drCLI) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(d.state, "runs", "r1", "journal.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	t.Run("done", func(t *testing.T) {
		d := newDrCLI(t, fanDef("retry-other-done", chItems(1), 1, wf.AllSettled()))
		in := d.hand()
		claim := claimToken(t, in)
		if code, msg := d.as("kid-a", claim, "prompt"); code != 0 {
			t.Fatalf("prompt: exit %d, %s", code, msg)
		}
		out := writeAllocated(t, in.Output, `{"OK":true}`)
		if code, msg := d.as("kid-b", claim, "done", in.Name, "--output", out); code != 0 || strings.Contains(msg, "already accepted") {
			t.Fatalf("another caller's done: exit %d, %q", code, msg)
		}
		before := journal(t, d)
		if !strings.Contains(before, "child caller mismatch") {
			t.Fatalf("the other caller's done was not noted:\n%s", before)
		}
		// The exact call again, by name as it was first sent, and with the
		// name left out; validate of the accepted result the same.
		for _, verb := range []string{"done", "validate"} {
			for _, named := range [][]string{{verb, in.Name}, {verb}} {
				if code, msg := d.as("kid-b", claim, append(named, "--output", out)...); code != 0 || !strings.Contains(msg, "already accepted") {
					t.Errorf("that caller's %v again: exit %d, %q", named, code, msg)
				}
			}
			// A name that is not the recorded unit's is still refused.
			if code, msg := d.as("kid-b", claim, verb, in.Name+"x", "--output", out); code != 4 || strings.Contains(msg, "already accepted") {
				t.Errorf("%s naming another unit with the settled token: exit %d, %q", verb, code, msg)
			}
		}
		if after := journal(t, d); after != before {
			t.Errorf("the retry appended to the journal:\n%s", strings.TrimPrefix(after, before))
		}
	})
	t.Run("failed", func(t *testing.T) {
		d := newDrCLI(t, plainAgentDef("retry-other-failed"))
		in := d.hand()
		claim := claimToken(t, in)
		if code, msg := d.as("kid-a", claim, "prompt"); code != 0 {
			t.Fatalf("prompt: exit %d, %s", code, msg)
		}
		if code, msg := d.as("kid-b", claim, "failed", in.Name, "--reason", "x", "--class", "transient"); code != 0 || strings.Contains(msg, "already recorded") {
			t.Fatalf("another caller's failed: exit %d, %q", code, msg)
		}
		before := journal(t, d)
		if !strings.Contains(before, "child caller mismatch") {
			t.Fatalf("the other caller's failed was not noted:\n%s", before)
		}
		for _, named := range [][]string{{"failed", in.Name}, {"failed"}} {
			if code, msg := d.as("kid-b", claim, append(named, "--reason", "x", "--class", "transient")...); code != 0 || !strings.Contains(msg, "already recorded") {
				t.Errorf("that caller's %v again: exit %d, %q", named, code, msg)
			}
		}
		if code, msg := d.as("kid-b", claim, "failed", in.Name+"x", "--reason", "x", "--class", "transient"); code != 4 || strings.Contains(msg, "already recorded") {
			t.Errorf("failed naming another unit with the settled token: exit %d, %q", code, msg)
		}
		if code, msg := d.as("kid-b", claim, "failed", "--reason", "y", "--class", "transient"); code != 4 || strings.Contains(msg, "already recorded") {
			t.Errorf("a failed with another reason: exit %d, %q", code, msg)
		}
		if after := journal(t, d); after != before {
			t.Errorf("the retries appended to the journal:\n%s", strings.TrimPrefix(after, before))
		}
	})
}
