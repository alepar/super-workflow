package wf

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// Breaking inputs from the parsing audit, reproduced on the engine and
// skipped until the slice that fixes each one unskips it.

func TestReproKeyOfPartsCollide(t *testing.T) {
	if KeyOf("fix", "a/b").(idemKey) == KeyOf("fix", "a", "b").(idemKey) {
		t.Fatalf("two effects share the key %v", KeyOf("fix", "a/b").key())
	}
	// Bound parts are escaped too: a placeholder's value holding "/" does
	// not move a part boundary.
	s := scope{}
	a, _ := s.bindEffect(EffectData{Kind: "external", Key: "fix/a%2Fb", KeyParts: []string{"fix", "a/b"}})
	b, _ := s.bindEffect(EffectData{Kind: "external", Key: "fix/a/b", KeyParts: []string{"fix", "a", "b"}})
	if a.Key == b.Key || a.Key != "fix/a%2Fb" || a.LegacyKey != "fix/a/b" || b.LegacyKey != "" {
		t.Fatalf("bound keys %q (legacy %q) and %q (legacy %q)", a.Key, a.LegacyKey, b.Key, b.LegacyKey)
	}
}

// The spawn line carries the instruction's claim, never one read off the
// brief, whose workspace may hold " --claim ".
func TestReproBriefClaimInWorkspacePath(t *testing.T) {
	in := Instruction{Kind: InstructionAgent, Name: "u", Claim: "abc123", Brief: "cd '/home/u/x --claim y' && '/bin/wf' prompt --claim abc123\n"}
	call := renderUnit(harness.For("pi"), in)
	if !strings.Contains(call, "wf-abc123") || strings.Contains(call, "wf-y") {
		t.Fatalf("the spawn line does not carry claim abc123:\n%s", call)
	}
}

// The binary a headless child may run is the engine's own, never read off
// the brief (whose workspace may hold "&& "): a library engine's is the
// default VerbLine's "workflow".
func TestReproBriefBinaryWithAndInWorkspace(t *testing.T) {
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{}).(*Engine)
	if e.opts.Binary != "workflow" {
		t.Fatalf("default Binary = %q, want workflow", e.opts.Binary)
	}
}

func TestReproUnitPathOfGenericState(t *testing.T) {
	// A Map child of a generic state G[X] with key k: its id is G[X][k],
	// and its unit path is built from what it recorded, not read off the id.
	n := &node{name: "G[X]", kind: &mapKind{}}
	child := &instance{ID: "G[X][k]", Spawner: "G[X]", Entry: 1, Key: "k", gen: 1}
	if got := unitSegment(&instance{ID: ""}, n, child); got != "G[X]/k" {
		t.Fatalf("unit segment = %q, want G[X]/k", got)
	}
}

// chainJournals writes, under state, each run as a run_started (with
// params k=v and the given predecessor), an optional rewound record, and an
// ended record.
func chainJournals(t *testing.T, state string, runs ...chainRun) {
	t.Helper()
	j := FileJournal(state)
	for _, r := range runs {
		unlock, err := j.Lock(r.id)
		if err != nil {
			t.Fatal(err)
		}
		started, _ := json.Marshal(RunStartedData{Def: "chain", Params: map[string]string{"k": "v"}, Predecessor: r.pred})
		recs := []JournalRecord{{Run: r.id, Kind: JournalRunStarted, Data: started}}
		if r.fork != "" {
			rewound, _ := json.Marshal(RewoundData{From: r.fork})
			recs = append(recs, JournalRecord{Run: r.id, Kind: JournalRewound, Data: rewound})
		}
		recs = append(recs, JournalRecord{Run: r.id, Kind: JournalRunEnded, Data: json.RawMessage(`{}`)})
		for _, rec := range recs {
			if err := j.Append(rec); err != nil {
				t.Fatal(err)
			}
		}
		unlock()
	}
}

type chainRun struct{ id, pred, fork RunID }

func chainDefAndCLI(t *testing.T) (*Def, *cli, RunID) {
	t.Helper()
	d := New("chain")
	d.RunKey(d.Param("k"))
	base, err := deriveRunID(d, map[string]string{"k": "v"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, &cli{env: func(string) string { return "" }, stderr: io.Discard}, base
}

// A fork (here of a successor) continues its own chain: its successor
// follows the fork, never the last run of its source's chain, and the
// params check reads the fork's recorded params, not its id.
func TestReproChainBaseOfForkedSuccessor(t *testing.T) {
	d, c, x := chainDefAndCLI(t)
	state := t.TempDir()
	x2 := successorRunID(x, 2)
	fork := forkRunID(x2, 1)
	chainJournals(t, state, chainRun{id: x}, chainRun{id: x2, pred: x}, chainRun{id: fork, pred: x, fork: x2}, chainRun{id: successorRunID(x, 3), pred: x2})
	if base, err := c.chainStart(state, d, fork); err != nil || base != fork {
		t.Fatalf("chainStart(%s) = %s, %v; want the fork itself", fork, base, err)
	}
	cont := &RunStartedData{Def: "chain", Params: map[string]string{"k": "v"}, self: fork}
	live, pred, err := c.runChain(d, map[string]string{"k": "v"}, state, &flags{}, cont)
	if err != nil || live != "" || pred == nil || pred.self != fork {
		t.Fatalf("runChain --continue %s = live %q, pred %+v, %v; want the fork as predecessor", fork, live, pred, err)
	}
	// A successor of the fork goes back to the fork.
	chainJournals(t, state, chainRun{id: successorRunID(fork, 2), pred: fork})
	if base, err := c.chainStart(state, d, successorRunID(fork, 2)); err != nil || base != fork {
		t.Fatalf("chainStart of the fork's successor = %s, %v; want %s", base, err, fork)
	}
	if base, err := c.chainStart(state, d, x2); err != nil || base != x {
		t.Fatalf("chainStart(%s) = %s, %v; want %s", x2, base, err, x)
	}
}

// A run at a chain's id that records another predecessor is refused,
// naming both; one recorded before Predecessor existed stays a member.
func TestChainRefusesAForeignRunAtItsID(t *testing.T) {
	d, c, x := chainDefAndCLI(t)
	state := t.TempDir()
	chainJournals(t, state, chainRun{id: x}, chainRun{id: successorRunID(x, 2), pred: "elsewhere"})
	_, _, err := c.runChain(d, map[string]string{"k": "v"}, state, &flags{}, nil)
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "elsewhere") || !strings.Contains(err.Error(), string(x)) {
		t.Fatalf("runChain over a foreign run = %v; want a refusal naming elsewhere and %s", err, x)
	}
	legacy := t.TempDir()
	chainJournals(t, legacy, chainRun{id: x}, chainRun{id: successorRunID(x, 2)})
	if live, pred, err := c.runChain(d, map[string]string{"k": "v"}, legacy, &flags{}, nil); err != nil || live != "" || pred == nil || pred.self != successorRunID(x, 2) {
		t.Fatalf("runChain over a legacy member = %q, %+v, %v; want it succeeded", live, pred, err)
	}
}
