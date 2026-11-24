package wf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// byProblem is eligibility by the candidate's journal marker alone.
func byProblem(c *fanoutCandidate) (string, error) { return c.problem, nil }

func TestFanoutSelectionLatestAndExplicit(t *testing.T) {
	candidates := []fanoutCandidate{
		{target: FanoutTarget{State: "child/Review", Instance: "map[a]", IssueSeq: 3}},
		{target: FanoutTarget{State: "Build", IssueSeq: 5}},
		{target: FanoutTarget{State: "child/Review", Instance: "map[b]", IssueSeq: 7}},
		{target: FanoutTarget{State: "child/Review", Instance: "map[a]", IssueSeq: 9}},
	}
	for _, tt := range []struct {
		mode string
		seqs []int64
	}{{"", []int64{5, 9}}, {"distinct", []int64{5, 9}}, {"all", []int64{5, 7, 9}}, {"child/Review,Build", []int64{5, 9}}} {
		got, _, err := selectFanout(candidates, tt.mode, byProblem)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(tt.seqs) {
			t.Fatalf("%s length %d", tt.mode, len(got))
		}
		for i, c := range got {
			if c.target.IssueSeq != tt.seqs[i] {
				t.Fatalf("%s seq %d", tt.mode, c.target.IssueSeq)
			}
		}
	}
	for _, mode := range []string{"Missing", "Review", "Build,Build", "Build,"} {
		if _, _, err := selectFanout(candidates, mode, byProblem); err == nil {
			t.Fatalf("accepted %s", mode)
		}
	}
}

// The latest offer of a state could not be forked (siblings live): the
// plan takes the latest offer that can, says so on the target (which carries
// its own offer seq, the point any comparison with the source must use) and
// lists the left-out offer; a state none of whose offers can be forked is
// left out with its reason, or refused when named explicitly.
func TestFanoutSubstitutesTheLatestForkableOffer(t *testing.T) {
	candidates := []fanoutCandidate{
		{target: FanoutTarget{State: "Review", IssueSeq: 3, SnapshotSeq: 2}},
		{target: FanoutTarget{State: "Review", IssueSeq: 5, SnapshotSeq: 4}},
		{target: FanoutTarget{State: "Review", IssueSeq: 7}, problem: "world has live children or commands"},
		{target: FanoutTarget{State: "Judge", IssueSeq: 9}, problem: "world has live children or commands"},
	}
	got, skipped, err := selectFanout(candidates, "distinct", byProblem)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].target.IssueSeq != 5 || got[0].target.SnapshotSeq != 4 ||
		got[0].target.Substituted != "latest offer 7: world has live children or commands; used offer 5" {
		t.Fatalf("picked %+v", got)
	}
	if len(skipped) != 2 || skipped[0] != (FanoutSkip{State: "Review", IssueSeq: 7, Reason: "world has live children or commands"}) ||
		skipped[1] != (FanoutSkip{State: "Judge", IssueSeq: 9, Reason: "world has live children or commands"}) {
		t.Fatalf("skipped %+v", skipped)
	}
	got, skipped, err = selectFanout(candidates, "Judge,Review", byProblem)
	if err != nil || len(got) != 2 || len(skipped) != 2 {
		t.Fatalf("explicit: %+v %+v %v", got, skipped, err)
	}
	if got[0].target.IssueSeq != 5 || got[0].target.Refused != "" || got[1].target.State != "Judge" ||
		got[1].target.Refused != "no offer of Judge can be forked; latest offer 9: world has live children or commands" {
		t.Fatalf("explicit picks %+v", got)
	}
}

func TestFanoutOutcomeIgnoresSourcePrefixAndOtherTargets(t *testing.T) {
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	target := FanoutTarget{SnapshotSeq: 5, Instance: "a", State: "Review", Attempt: 1}
	records := []JournalRecord{
		{Seq: 3, Kind: JournalAccepted, Instance: "a", State: "Review", Attempt: 1, Data: raw(AcceptedData{})},
		{Seq: 6, Kind: JournalAccepted, Instance: "other", State: "Review", Attempt: 1, Data: raw(AcceptedData{})},
		{Seq: 6, Kind: JournalInstructionIssued, Instance: "a", State: "Review", Attempt: 1, Data: raw(InstructionIssuedData{Token: "current"})},
		{Seq: 7, Kind: JournalSubmissionRefused, Instance: "a", State: "Review", Attempt: 1, Data: raw(SubmissionRefusedData{Reasons: []string{"bad evidence"}})},
		{Seq: 8, Kind: JournalSubmitted, Instance: "a", State: "Review", Attempt: 1, Data: raw(SubmittedData{Token: "current", Tokens: 12})},
	}
	got, err := ClassifyFanout(records, target)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "refused" || len(got.Refused) != 1 || (got.Tokens == nil || *got.Tokens != 12) {
		t.Fatalf("%+v", got)
	}
	records = append(records, JournalRecord{Seq: 9, Kind: JournalAccepted, Instance: "a", State: "Review", Attempt: 1, Data: raw(AcceptedData{})})
	got, err = ClassifyFanout(records, target)
	if err != nil || got.Status != "done" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestPlanFanoutUsesOnlyExactHistoricalCheckpoint(t *testing.T) {
	for _, mode := range []string{"valid", "reused", "reencoded", "other-metadata", "unknown-adapter", "missing-adapter-dir", "missing", "intervening", "wrong-attempt", "wrong-before", "unavailable", "missing-artifact"} {
		t.Run(mode, func(t *testing.T) {
			issued := newIssuedRun(t)
			j := MemJournal()
			root := t.TempDir()
			run := RunID("fanout")
			reg := NewAdapters()
			if err := reg.Register(planWorld{}); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(j, reg, EngineOptions{Defs: []*Def{issued.def}, RunDir: func(RunID) string { return root }}).(*Engine)
			raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
			// A forkable checkpoint: its adapter is registered here and its
			// directory is in the checkpoint.
			data := WorldSnapshotData{Version: 1, Before: JournalInstructionIssued, Directory: "snapshots/one",
				Adapters: map[string]json.RawMessage{"world": json.RawMessage(`{"n":9007199254740993}`)}}
			if mode == "unknown-adapter" {
				data.Adapters["ghost"] = json.RawMessage(`{}`)
			}
			if mode == "wrong-before" {
				data.Before = JournalTransition
			}
			if mode == "unavailable" {
				data.Unavailable = "live children"
			}
			manifest := data
			if mode == "reencoded" {
				// A relocated journal re-encodes record data: the same values
				// in other bytes.
				manifest.Adapters = map[string]json.RawMessage{"world": json.RawMessage(`{ "n" : 9007199254740993 }`)}
			}
			if mode == "other-metadata" {
				manifest.Adapters = map[string]json.RawMessage{"world": json.RawMessage(`{"n":9007199254740992}`)}
			}
			if mode == "reused" {
				// A capture taken before a transition and named again by
				// the offer's checkpoint later in the same cycle.
				manifest.Before = JournalTransition
			}
			if mode != "missing-artifact" {
				if err := os.MkdirAll(filepath.Join(root, data.Directory), 0700); err != nil {
					t.Fatal(err)
				}
				for name := range data.Adapters {
					if mode == "missing-adapter-dir" && name == "world" {
						continue
					}
					if err := os.MkdirAll(filepath.Join(root, data.Directory, name), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(root, data.Directory, "manifest.json"), raw(manifest), 0600); err != nil {
					t.Fatal(err)
				}
			}
			unlock, _ := j.Lock(run)
			for _, r := range issued.recs {
				r.Run = run
				if r.Kind == JournalInstructionIssued && mode != "missing" {
					s := r
					s.Kind = JournalWorldSnapshot
					s.Data = raw(data)
					if mode == "wrong-attempt" {
						s.Attempt++
					}
					if err := j.Append(s); err != nil {
						t.Fatal(err)
					}
					if mode == "intervening" {
						s.Kind = JournalNote
						s.Data = raw(NoteData{})
						if err := j.Append(s); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := j.Append(r); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := j.Records(run)
			unlock()
			plan, err := e.PlanFanout(run, "distinct")
			got := plan.Targets
			if mode == "valid" || mode == "reused" || mode == "reencoded" {
				if err != nil || len(got) != 1 || got[0].Unit == "" || got[0].Visit != 1 {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil && len(got) > 0 {
				t.Fatalf("accepted %s: %+v", mode, got)
			} else if err == nil && len(plan.Skipped) != 1 {
				t.Fatalf("%s left out unnamed: %+v", mode, plan.Skipped)
			}
			unlock, _ = j.Lock(run)
			after, _ := j.Records(run)
			unlock()
			if !reflect.DeepEqual(before, after) {
				t.Fatal("planner mutated journal")
			}
		})
	}
}

func TestFanoutAllRetainsVisitsButCollapsesRetries(t *testing.T) {
	candidates := []fanoutCandidate{
		{target: FanoutTarget{State: "Review", Visit: 1, Attempt: 1, IssueSeq: 2}},
		{target: FanoutTarget{State: "Review", Visit: 1, Attempt: 2, IssueSeq: 4}},
		{target: FanoutTarget{State: "Review", Visit: 2, Attempt: 1, IssueSeq: 6}},
		{target: FanoutTarget{State: "Review", Instance: "m[x]", Visit: 1, Attempt: 1, IssueSeq: 8}, problem: "world has live children or commands"},
	}
	got, skipped, err := selectFanout(candidates, "all", byProblem)
	if err != nil || len(got) != 2 || got[0].target.IssueSeq != 4 || got[1].target.IssueSeq != 6 {
		t.Fatalf("%+v %v", got, err)
	}
	if len(skipped) != 1 || skipped[0] != (FanoutSkip{State: "Review", Instance: "m[x]", Visit: 1, IssueSeq: 8, Reason: "world has live children or commands"}) {
		t.Fatalf("skipped %+v", skipped)
	}
}

func TestFanoutOutcomeStopsAtTargetVisitBoundary(t *testing.T) {
	target := FanoutTarget{SnapshotSeq: 5, State: "Review", LocalState: "Review", Attempt: 1}
	records := []JournalRecord{{Seq: 6, Kind: JournalTransition, State: "Review", Attempt: 1, Data: json.RawMessage(`{"To":"Review"}`)}, {Seq: 8, Kind: JournalAccepted, State: "Review", Attempt: 1, Data: json.RawMessage(`{}`)}}
	got, err := ClassifyFanout(records, target)
	if err != nil || got.Status != "ended-without-done" || got.Tokens != nil || got.CoordinatorCalls != nil || got.ChildVerbsAttempted != nil {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestFanoutReissuedOfferDoesNotUsePreviousUnitName(t *testing.T) {
	st := &runState{}
	st.initIndex()
	st.retired["Review#1"] = true
	in := &instance{Cur: "Review", Unit: &unit{Name: "Review#1", Token: "old"}}
	if got := fanoutUnitName(st, in, "new"); got != "Review#2" {
		t.Fatalf("got %s", got)
	}
	if got := fanoutUnitName(st, in, "old"); got != "Review#1" {
		t.Fatalf("same token got %s", got)
	}
}

func TestFanoutDoneRequiresMatchingChildSubmission(t *testing.T) {
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	for _, mode := range []string{"guard", "wrong-token", "wrong-attempt", "refused", "valid"} {
		t.Run(mode, func(t *testing.T) {
			target := FanoutTarget{SnapshotSeq: 5, State: "Review", Attempt: 1}
			records := []JournalRecord{{Seq: 6, Kind: JournalInstructionIssued, State: "Review", Attempt: 1, Data: raw(InstructionIssuedData{Token: "current"})}}
			if mode != "guard" {
				token, attempt := "current", 1
				if mode == "wrong-token" {
					token = "stale"
				}
				if mode == "wrong-attempt" {
					attempt = 2
				}
				records = append(records, JournalRecord{Seq: 7, Kind: JournalSubmitted, State: "Review", Attempt: attempt, Data: raw(SubmittedData{Token: token})})
			}
			if mode == "refused" {
				records = append(records, JournalRecord{Seq: 8, Kind: JournalSubmissionRefused, State: "Review", Attempt: 1, Data: raw(SubmissionRefusedData{Token: "current", Reasons: []string{"invalid"}})})
			}
			records = append(records, JournalRecord{Seq: 9, Kind: JournalAccepted, State: "Review", Attempt: 1, Data: raw(AcceptedData{})})
			got, err := ClassifyFanout(records, target)
			if err != nil {
				t.Fatal(err)
			}
			want := "ended-without-done"
			if mode == "valid" || mode == "reused" || mode == "reencoded" {
				want = "done"
			}
			if mode == "refused" {
				want = "refused"
			}
			if got.Status != want {
				t.Fatalf("status %s want %s", got.Status, want)
			}
		})
	}
}

// planWorld is a snapshot adapter named "world" a plan can find registered.
type planWorld struct{}

func (planWorld) Ops() []string                                         { return []string{"world.read"} }
func (planWorld) Read(string, ObservationDesc) (json.RawMessage, error) { return nil, nil }
func (planWorld) Apply(string, ExternalEffect) (json.RawMessage, error) { return nil, nil }
func (planWorld) Check(string, ExternalEffect) (EffectStatus, error)    { return EffectDone, nil }
func (planWorld) SnapshotPreflight(SnapshotRequest) error               { return nil }
func (planWorld) Snapshot(SnapshotRequest) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (planWorld) Restore(RestoreRequest, json.RawMessage) error { return nil }
