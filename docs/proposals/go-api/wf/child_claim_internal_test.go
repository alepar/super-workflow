package wf

import (
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// Deterministic interleaving: another caller preflights an unbound started
// unit, its real child binds it, then the other caller's missing-output done
// arrives with the unit's token. The token admits it: the done is taken as
// the unit's and refused only for its missing output, with the attempt left
// open; the first fetcher stays the recorded owner, and the journal notes the
// caller mismatch once.
func TestMissingOutputFromAnotherCallerIsNotedNotRefused(t *testing.T) {
	d := foldDef()
	j := MemJournal()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{d}}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOutAs(run, 1, Coordinator{Harness: "hermes", Session: "parent"})
	if err != nil || len(ins) != 1 {
		t.Fatalf("handout %v %v", ins, err)
	}
	name := ins[0].Name
	match := regexp.MustCompile(`--claim (\S+)`).FindStringSubmatch(ins[0].Brief)
	if len(match) != 2 {
		t.Fatal(ins[0].Brief)
	}
	token := match[1]
	if err := e.Started(run, name, Claim{}); err != nil {
		t.Fatal(err)
	}
	wrong := harness.Caller{Harness: "hermes", Session: "wrong", FromEnv: true, Is: harness.IsChild}
	right := wrong
	right.Session = "right"
	e.opts.caller = wrong
	e.opts.childClaim = token
	if err := e.ChildVerbCheck(run, "done", name, token, wrong); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if _, err := e.PromptFetchWhole(run, name, token, right); err != nil {
		t.Fatal(err)
	}
	if _, err = e.refuseMissingOutput(run, name, "missing-result", nil); !errors.Is(err, errOutputMissing) {
		t.Fatalf("missing-output done holding the unit's token = %v, want it taken and refused for its output alone", err)
	}
	if err := e.withDryRun(run, func(c *cycle) error {
		refused, notes := 0, 0
		for _, r := range c.recs {
			switch r.Kind {
			case JournalRefused:
				refused++
			case JournalPromptFetched:
				var d PromptFetchedData
				if json.Unmarshal(r.Data, &d) != nil || d.Caller == nil || d.Caller.Session != "right" {
					t.Fatalf("the first fetcher is not the recorded owner: %s", r.Data)
				}
			case JournalDegraded:
				var d DegradedData
				if json.Unmarshal(r.Data, &d) != nil || d.Kind != DegradedChildCaller || d.Unit != name {
					t.Fatalf("degraded record %s, want a child caller mismatch of %s", r.Data, name)
				}
				notes++
			}
		}
		if refused != 0 || notes != 1 {
			t.Fatalf("refused=%d caller notes=%d, want the attempt left open (no refused record) and one note", refused, notes)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Pre-binding journals mix environment identity and resolved transcript IDs.
// A Claude transcript's agent ID must not become a shell identity requirement.
func TestLegacyClaudeResolvedAgentDoesNotBecomeCallerIdentity(t *testing.T) {
	r := newIssuedRun(t)
	name := r.fold().unitName(r.fold().insts[""])
	r.must(JournalHandedOut, HandedOutData{Name: name, Token: r.tok, Claim: "legacy-claim", Session: "parent"})
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Harness: "claude", Session: "parent", Root: "/records", Env: true})
	r.must(JournalPromptFetched, PromptFetchedData{Name: name, Claim: "legacy-claim", Whole: true})
	r.must(JournalMetadataAttached, MetadataAttachedData{Name: name, Agent: "resolved-transcript-agent", Resolved: true})
	c := dlCycle(t, r.recs, r.def)
	who := harness.Caller{Harness: "claude", Session: "parent", Root: "/records", FromEnv: true}
	if err := c.childClaimCheck(name, "legacy-claim", who); err != nil {
		t.Fatalf("same legacy child: %v", err)
	}
}
