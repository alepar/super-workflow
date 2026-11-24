package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The recorded task.4 child naming task.5 with no claim token is refused and
// writes nothing. Holding task.5's token it is accepted, whatever its own
// delegation log shows: the token binds the call, and the journal notes that
// the caller is not the unit's first fetcher.
func TestHermesSiblingNeedsTheUnitsToken(t *testing.T) {
	for _, verb := range []string{"prompt", "validate", "done", "failed"} {
		for _, tokened := range []bool{false, true} {
			for _, siblingRecord := range []bool{false, true} {
				t.Run(verb+map[bool]string{false: "/by-name", true: "/bound-token"}[tokened]+map[bool]string{false: "/missing-own-record", true: "/own-record"}[siblingRecord], func(t *testing.T) {
					strictCallers(t)
					root, state := t.TempDir(), filepath.Join(t.TempDir(), "state")
					t.Setenv("HERMES_HOME", root)
					t.Setenv("HERMES_SESSION_ID", "coordinator")
					t.Setenv("HERMES_DELEGATED_CHILD_CONTEXT", "")
					def := fanDef("sibling-guard", chItems(1, 2), 2, wf.AllSettled())
					mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
					var ins []wf.Instruction
					if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "2", "--json", "--state-dir", state)), &ins); err != nil {
						t.Fatal(err)
					}
					a, b := agentsOf(ins)[0], agentsOf(ins)[1]
					for i, fixture := range []string{"first", "second"} {
						data, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "hermes", "cross-task", fixture+".log"))
						if err != nil {
							t.Fatal(err)
						}
						data = []byte(strings.NewReplacer("2020202020202020", claimToken(t, a), "3030303030303030", claimToken(t, b)).Replace(string(data)))
						p := filepath.Join(root, "cache", "delegation", "live", "deleg_recorded", []string{"task-1.log", "task-2.log"}[i])
						if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(p, data, 0600); err != nil {
							t.Fatal(err)
						}
					}
					t.Setenv("HERMES_DELEGATED_CHILD_CONTEXT", root)
					// Bind the caller's own unit first: its journal identity plus
					// transcript proves this is a real sibling, not a forged marker.
					t.Setenv("HERMES_SESSION_ID", "fixture-first")
					mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, a), "--json", "--state-dir", state)
					t.Setenv("HERMES_SESSION_ID", "fixture-second")
					mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, b), "--json", "--state-dir", state)
					writeAllocated(t, b.Output, `{"OK":true}`)
					if !siblingRecord {
						if err := os.Remove(filepath.Join(root, "cache", "delegation", "live", "deleg_recorded", "task-1.log")); err != nil {
							t.Fatal(err)
						}
					}
					journal := filepath.Join(state, "runs", "r1", "journal.jsonl")
					before, err := os.ReadFile(journal)
					if err != nil {
						t.Fatal(err)
					}
					t.Setenv("HERMES_SESSION_ID", "fixture-first")
					args := []string{"wf", verb, b.Name, "--state-dir", state}
					if tokened {
						args = append(args, "--claim", claimToken(t, b))
					}
					if verb == "failed" {
						args = append(args, "--reason", "wrong child")
					}
					code, out, stderr := singleCLI(def, args...)
					if want := map[bool]int{false: 4, true: 0}[tokened]; code != want {
						t.Fatalf("sibling: exit %d, want %d: %s%s", code, want, out, stderr)
					}
					after, err := os.ReadFile(journal)
					if err != nil {
						t.Fatal(err)
					}
					if tokened && verb != "validate" {
						if !bytes.Contains(after[len(before):], []byte("child caller mismatch")) || bytes.Contains(after[len(before):], []byte("child refused")) {
							t.Fatalf("want a caller mismatch note and no refusal: %s", after[len(before):])
						}
					} else if !bytes.Equal(before, after) {
						t.Fatal("a refused or read-only call changed the journal")
					}
					if verb == "done" || verb == "failed" {
						if tokened {
							return // the sibling's call settled the attempt
						}
					}
					t.Setenv("HERMES_SESSION_ID", "fixture-second")
					// The unit is still its first fetcher's to finish.
					mustSingle(t, def, "wf", "validate", "--claim", claimToken(t, b), "--state-dir", state)
					mustSingle(t, def, "wf", "done", "--claim", claimToken(t, b), "--state-dir", state)
				})
			}
		}
	}
}

// A person has an explicit dev route; agents cannot use it as a name-only escape.
func TestChildDevNameRouteIsOperatorOnly(t *testing.T) {
	strictCallers(t)
	root, state := t.TempDir(), filepath.Join(t.TempDir(), "state")
	t.Setenv("HERMES_HOME", root)
	t.Setenv("HERMES_SESSION_ID", "coordinator")
	t.Setenv("HERMES_DELEGATED_CHILD_CONTEXT", "")
	def := fanDef("operator-child", chItems(1), 1, wf.AllSettled())
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil {
		t.Fatal(err)
	}
	in := agentsOf(ins)[0]
	for _, verb := range []string{"prompt", "validate", "done", "failed"} {
		args := []string{"wf", "dev", verb, in.Name, "--state-dir", state}
		if verb == "failed" {
			args = append(args, "--reason", "operator")
		}
		if code, out, err := singleCLI(def, args...); code != 4 {
			t.Fatalf("agent dev %s exit%d %s%s", verb, code, out, err)
		}
	}
	for _, key := range []string{"HERMES_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID"} {
		t.Setenv(key, "")
	}
	mustSingle(t, def, "wf", "dev", "prompt", in.Name, "--json", "--state-dir", state)
	writeAllocated(t, in.Output, `{"OK":true}`)
	mustSingle(t, def, "wf", "dev", "validate", in.Name, "--state-dir", state)
	mustSingle(t, def, "wf", "dev", "done", in.Name, "--state-dir", state)
}

// Two first fetches with the unit's token can pass a dry precheck together:
// both are served, the unit binds once to the first, and the second is noted
// as a caller mismatch.
func TestFirstClaimBindsOnceForTwoCallers(t *testing.T) {
	d := clAgentDef("atomic-caller")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	in := agentsOf(ins)[0]
	token := claimToken(t, in)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, session := range []string{"one", "two"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := e.PromptFetchWhole(run, in.Name, token, hn.Caller{Harness: "hermes", Session: session, Is: hn.IsChild, FromEnv: true})
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	fetches := h.kind(run, wf.JournalPromptFetched)
	if len(fetches) != 2 {
		t.Fatalf("prompt fetches=%d, want both served", len(fetches))
	}
	var first, second wf.PromptFetchedData
	if json.Unmarshal(fetches[0].Data, &first) != nil || json.Unmarshal(fetches[1].Data, &second) != nil || first.Claim != token || second.Claim != "" {
		t.Fatalf("binding fetches %s then %s, want the token bound by the first only", fetches[0].Data, fetches[1].Data)
	}
	notes := h.kind(run, wf.JournalDegraded)
	if len(notes) != 1 || !strings.Contains(string(notes[0].Data), string(wf.DegradedChildCaller)) {
		t.Fatalf("degraded records %+v, want one child caller mismatch", notes)
	}
}

// A token of another unit, no token and an unknown token are refused. A
// Codex sibling holding the unit's own token is served and noted.
func TestClaimRejectsWrongUnitAndNotesCodexSibling(t *testing.T) {
	d := fanDef("claim-target", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins := handOutAs(t, h, run, 2, "parent")
	a, b := ins[0], ins[1]
	owner := codexChild("parent", "one")
	before := len(h.kind(run, wf.JournalPromptFetched))
	for _, tc := range []struct{ name, token string }{
		{b.Name, claimToken(t, a)}, {a.Name, ""}, {a.Name, "unknown-token"},
	} {
		if _, err := e.PromptFetchWhole(run, tc.name, tc.token, owner); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if got := len(h.kind(run, wf.JournalPromptFetched)); got != before {
		t.Fatal("invalid claim fetched prompt")
	}
	if _, err := e.PromptFetchWhole(run, a.Name, claimToken(t, a), owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PromptFetchWhole(run, a.Name, claimToken(t, a), codexChild("parent", "two")); err != nil {
		t.Fatalf("Codex sibling holding the token=%v", err)
	}
	if notes := h.kind(run, wf.JournalDegraded); len(notes) != 1 || !strings.Contains(string(notes[0].Data), string(wf.DegradedChildCaller)) {
		t.Fatalf("degraded records %+v, want one child caller mismatch", notes)
	}
	if _, err := e.PromptFetchWhole(run, a.Name, claimToken(t, a), owner); err != nil {
		t.Fatal(err)
	}
}

// A Hermes coordinator that rewrote the brief so its claim command fell past
// Hermes's cut (models/m2, CoverageReview): its log shows no claim. The
// child's fetch is served on its token; the records admit nothing.
func TestHermesChildAfterATruncatedBriefIsServed(t *testing.T) {
	d := clAgentDef("hermes-cut")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	root := t.TempDir()
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "parent", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	in := agentsOf(ins)[0]
	token := claimToken(t, in)
	rules := "Run the assigned unit of work for " + in.Name + ". Rules: change state only through the workflow commands yo …(+1013 chars)"
	log := "=== Hermes subagent live transcript ===\ndelegation: deleg_cut   task: 0\ngoal: " + rules +
		"\nstarted: today\n========================================\n" +
		"07:47:51 user     | kickoff: " + rules + " | context: Your working directory is /home\n" +
		"07:47:51 start    | " + rules + "\n" +
		"07:47:55 tool     | -> terminal(/bin/super wf prompt --claim " + token + " --state-dir /s …(+305 chars))\n"
	p := filepath.Join(root, "cache", "delegation", "live", "deleg_cut", "task-0.log")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PromptFetchWhole(run, in.Name, token, hn.Caller{Harness: "hermes", Session: "child", Root: root, Is: hn.IsChild, FromEnv: true}); err != nil {
		t.Fatalf("the fetch with no visible launch = %v, want it served", err)
	}
	if got := h.kind(run, wf.JournalDegraded); len(got) != 0 {
		t.Fatalf("the fetch journaled %+v", got)
	}
}
