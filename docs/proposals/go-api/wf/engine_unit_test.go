package wf

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type (
	jvItem struct{ ID string }
	jvRes  struct{}
	jvFan  struct{}
	jvEnd  struct{}
	jvWork struct{}
	jvKDon struct{}
	jvOut  struct{}
)

// journalV1Def is the definition behind testdata/journal_v1.jsonl: a Map with a
// lease over a one-agent child, so the journal holds token-only
// instruction_issued records and lease_claimed / lease_expired.
func journalV1Def() *Def {
	kid := New("kid")
	child := Typed[jvItem, jvRes](kid)
	work := NewAgent[jvWork, jvOut](kid)
	kdone := NewTerminal[jvKDon](kid, Success)
	work.Spec(PromptText("Work."), Retry(Const(2), work.To(kdone, "gave up")))
	next := work.To(kdone, "done")
	work.Route(func(jvOut, Ctx) Edge[jvWork] { return next }, next)

	d := New("lease")
	src := FromObservation[jvItem](Observe[[]jvItem]("x", "items", nil))
	NewMap[jvFan, jvItem, string, jvRes](d, src, func(i jvItem) string { return i.ID }, child).Lease(time.Minute)
	NewTerminal[jvEnd](d, Success)
	return d
}

var unitT0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// issuedRun is foldDef's journal up to the first instruction_issued, with the
// folded state, ready for hand-appended unit records.
type issuedRun struct {
	t    *testing.T
	def  *Def
	recs []JournalRecord
	tok  string
	n    int
}

func newIssuedRun(t *testing.T) *issuedRun {
	t.Helper()
	d := foldDef()
	j := MemJournal()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return unitT0 }}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// HandOut(0) drives the run to its open instruction without handing it out
	// (the wave Next would hand it out and claim it).
	if ins, err := e.HandOut(run, 0); err != nil || len(ins) != 0 {
		t.Fatalf("HandOut(0) = %v, %v", ins, err)
	}
	unlock, _ := j.Lock(run)
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	var tok string
	for _, r := range recs {
		if r.Kind == JournalInstructionIssued {
			var d InstructionIssuedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				t.Fatal(err)
			}
			tok = d.Token
		}
	}
	if tok == "" {
		t.Fatal("no instruction_issued record")
	}
	return &issuedRun{t: t, def: d, recs: recs, tok: tok}
}

// add appends a record for the root instance, one second after the last.
func (r *issuedRun) add(kind JournalKind, payload any) error {
	r.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		r.t.Fatal(err)
	}
	r.n++
	r.recs = append(r.recs, JournalRecord{Seq: int64(len(r.recs) + 1), Kind: kind, Run: r.recs[0].Run, State: "fA",
		DefHash: r.recs[0].DefHash, At: unitT0.Add(time.Duration(r.n) * time.Second), Data: raw})
	_, err = foldRun(r.recs, r.def)
	if err != nil {
		r.recs = r.recs[:len(r.recs)-1]
	}
	return err
}

func (r *issuedRun) fold() *runState {
	r.t.Helper()
	st, err := foldRun(r.recs, r.def)
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func (r *issuedRun) must(kind JournalKind, payload any) {
	r.t.Helper()
	if err := r.add(kind, payload); err != nil {
		r.t.Fatal(err)
	}
}

func (r *issuedRun) handOut() string {
	r.t.Helper()
	name := r.fold().unitName(r.fold().insts[""])
	r.must(JournalHandedOut, HandedOutData{Name: name, Token: r.tok, Session: "s1", Expires: unitT0.Add(2 * time.Minute)})
	return name
}

func wantState(t *testing.T, st *runState, want UnitState) *unit {
	t.Helper()
	u := st.insts[""].Unit
	if u == nil || u.State != want {
		t.Fatalf("unit = %+v, want state %s", u, want)
	}
	return u
}

func TestNewRecordKindsFold(t *testing.T) {
	r := newIssuedRun(t)
	name := r.handOut()
	if name != "fA#1" {
		t.Fatalf("first name = %q, want fA#1", name)
	}
	u := wantState(t, r.fold(), UnitHanded)
	if u.Token != r.tok || u.Attempt != 1 || !u.HandedAt.Equal(r.recs[len(r.recs)-1].At) || !u.HandExpires.Equal(unitT0.Add(2*time.Minute)) {
		t.Fatalf("handed unit = %+v", u)
	}
	if tok, ok := r.fold().tokenForName(name); !ok || tok != r.tok {
		t.Fatalf("tokenForName = %q, %v", tok, ok)
	}

	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Harness: "h", Session: "s1", Agent: "ag", Transcript: "/t", Expires: unitT0.Add(time.Hour)})
	u = wantState(t, r.fold(), UnitStarted)
	if u.Claim == nil || *u.Claim != (unitClaim{"h", "s1", "ag", "/t", "", "", false}) || !u.LeaseExpires.Equal(unitT0.Add(time.Hour)) || u.StartedAt.IsZero() {
		t.Fatalf("started unit = %+v", u)
	}
	r.must(JournalPromptFetched, PromptFetchedData{Name: name})
	if u = wantState(t, r.fold(), UnitRunning); u.FetchedAt.IsZero() {
		t.Fatal("prompt_fetched left no stamp")
	}
	r.must(JournalHeartbeat, HeartbeatData{Name: name, Status: "active", By: "ag", Expires: unitT0.Add(2 * time.Hour)})
	if u = wantState(t, r.fold(), UnitRunning); u.BeatAt.IsZero() || !u.LeaseExpires.Equal(unitT0.Add(2*time.Hour)) {
		t.Fatalf("heartbeat unit = %+v", u)
	}
	// A same-agent re-claim keeps the running state; another agent is refused.
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Agent: "ag", Expires: unitT0.Add(3 * time.Hour)})
	wantState(t, r.fold(), UnitRunning)
	if err := r.add(JournalAttemptStarted, AttemptStartedData{Name: name, Agent: "other"}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("claim by another agent = %v, want ErrCorruptJournal", err)
	}
	if err := r.add(JournalHeartbeat, HeartbeatData{Name: name, Status: "bogus"}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("heartbeat status bogus = %v, want ErrCorruptJournal", err)
	}
	if err := r.add(JournalPromptFetched, PromptFetchedData{Name: "nope#1"}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("prompt_fetched for an unknown name = %v, want ErrCorruptJournal", err)
	}
	// An accepted submission settles the unit and retires its name.
	r.must(JournalAccepted, AcceptedData{Output: json.RawMessage(`{"V":"x"}`), Plan: EdgePlan{Steps: []EdgeStep{{From: "fA", To: "fB"}}}})
	wantState(t, r.fold(), UnitDone)
	if _, ok := r.fold().tokenForName(name); ok {
		t.Fatal("name still live after accept")
	}
}

func TestClaimMetadataFoldPreservesTimingAndOwnership(t *testing.T) {
	r := newIssuedRun(t)
	name := r.handOut()
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Session: "s1", Expires: unitT0.Add(time.Hour)})
	started := r.recs[len(r.recs)-1].At
	r.must(JournalPromptFetched, PromptFetchedData{Name: name})
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Harness: "h", Agent: "ag", Transcript: "/t", Expires: unitT0.Add(2 * time.Hour)})
	u := wantState(t, r.fold(), UnitRunning)
	if *u.Claim != (unitClaim{"h", "s1", "ag", "/t", "", "", false}) || !u.StartedAt.Equal(started) || !u.LeaseExpires.Equal(unitT0.Add(time.Hour)) {
		t.Fatalf("attachment changed ownership or clocks: %+v, claim=%+v", u, u.Claim)
	}
	for _, add := range []AttemptStartedData{
		{Name: name, Session: "other"}, {Name: name, Harness: "other"},
		{Name: name, Agent: "other"}, {Name: name, Transcript: "/other"},
	} {
		if err := r.add(JournalAttemptStarted, add); !errors.Is(err, ErrCorruptJournal) {
			t.Errorf("conflicting journal ownership %+v = %v, want ErrCorruptJournal", add, err)
		}
	}
}

// A unit path segment is built from what the instance recorded and keeps the
// spellings unit names have always had; an id from before the current
// constructors is its own segment.
func TestUnitPathAndNames(t *testing.T) {
	root, outer := &instance{ID: ""}, &instance{ID: "Outer#2"}
	sub, fan := &node{name: "Outer", kind: &subKind{}}, &node{name: "Judges", kind: &mapKind{}}
	for _, c := range []struct {
		parent *instance
		n      *node
		x      instance
		want   string
	}{
		{root, sub, instance{ID: "Outer#1", Spawner: "Outer", Entry: 1}, "Outer"},
		{root, sub, instance{ID: "Outer#2", Spawner: "Outer", Entry: 2}, "Outer.2"},
		{root, fan, instance{ID: "Judges[f17]", Spawner: "Judges", Entry: 1, Key: "f17", gen: 1}, "Judges/f17"},
		{outer, fan, instance{ID: "Outer#2/Judges[f17]", Spawner: "Judges", Entry: 1, Key: "f17", gen: 1}, "Judges/f17"},
		{root, fan, instance{ID: "Judges[a%2Fb]", Spawner: "Judges", Entry: 1, Key: "a/b", gen: 1}, "Judges/a%2Fb"},
		{root, fan, instance{ID: "Judges[k]~2", Spawner: "Judges", Entry: 1, Key: "k", gen: 2}, "Judges[k]~2"},
		{root, fan, instance{ID: "Judges[k]#2", Spawner: "Judges", Entry: 2, Key: "k", gen: 1}, "Judges[k].2"},
		{root, fan, instance{ID: "Judges[k]~2#3", Spawner: "Judges", Entry: 3, Key: "k", gen: 2}, "Judges[k]~2.3"},
		{root, fan, instance{ID: "Judges[a#b]#2", Spawner: "Judges", Entry: 2, Key: "a#b", gen: 1}, "Judges[a#b].2"},
		{root, fan, instance{ID: "kid-1", Spawner: "Judges", Entry: 1, Key: "x", gen: 1}, "kid-1"},
	} {
		if got := unitSegment(c.parent, c.n, &c.x); got != c.want {
			t.Errorf("unitSegment(%q) = %q, want %q", c.x.ID, got, c.want)
		}
	}
}

func TestUnitNamesStableAndUnique(t *testing.T) {
	r := newIssuedRun(t)
	st := r.fold()
	root := st.insts[""]
	if a, b := st.unitName(root), st.unitName(root); a != b || a != "fA#1" {
		t.Fatalf("unitName not stable: %q then %q", a, b)
	}
	// A revisit: another instance state resolves to its own ordinal.
	child := &instance{ID: "kid-1", Cur: "Refute"}
	if got := st.unitName(child); got != "kid-1/Refute#1" {
		t.Fatalf("map child name = %q", got)
	}
	name := r.handOut()
	st = r.fold()
	if got := st.unitName(st.insts[""]); got != "fA#2" {
		t.Fatalf("name after hand-out = %q, want fA#2 (no reuse)", got)
	}
	// A second handed_out for the still handed unit is a re-offer, not a new unit.
	if err := r.add(JournalHandedOut, HandedOutData{Name: name, Token: r.tok, Claim: "c2"}); err != nil {
		t.Fatalf("re-offer of the handed unit = %v, want accepted", err)
	}
	if st = r.fold(); st.insts[""].Unit.Name != name || st.insts[""].Unit.State != UnitHanded {
		t.Fatalf("after a re-offer unit = %+v, want %s still handed", st.insts[""].Unit, name)
	}

	spent := st.insts[""].Spent
	r.must(JournalAttemptLost, AttemptLostData{Name: name, Cause: LostCauseSpawn})
	if err := r.add(JournalHandedOut, HandedOutData{Name: name, Token: r.tok}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("handed_out of a retired name = %v, want ErrCorruptJournal", err)
	}
	st = r.fold()
	if _, ok := st.tokenForName(name); ok {
		t.Fatal("lost name still live")
	}
	if _, _, ok := st.unitByName(name); ok {
		t.Fatal("lost name still found")
	}
	in := st.insts[""]
	u := wantState(t, st, UnitPending)
	if u.Attempt != 2 || in.Attempt != 2 || in.Spent != spent || u.Predecessor != name || in.Phase != phaseReady {
		t.Fatalf("after loss: unit %+v, attempt %d, spent %d (was %d), phase %s", u, in.Attempt, in.Spent, spent, in.Phase)
	}
	if got := st.unitName(in); got != "fA#2" {
		t.Fatalf("reissue name = %q, want fA#2 (the retired #1 is not reused)", got)
	}
}

func TestLostCapSettlesFailed(t *testing.T) {
	r := newIssuedRun(t)
	name := r.handOut()
	r.must(JournalAttemptLost, AttemptLostData{Name: name, Cause: LostCauseSpawn})
	wantState(t, r.fold(), UnitPending)
	// Reissue: a fresh instruction_issued, a fresh hand-out, lost again at the cap.
	r.n++
	raw, _ := json.Marshal(InstructionIssuedData{Token: "tok2", Prompt: "p"})
	r.recs = append(r.recs, JournalRecord{Seq: int64(len(r.recs) + 1), Kind: JournalInstructionIssued, Run: r.recs[0].Run, State: "fA",
		DefHash: r.recs[0].DefHash, At: unitT0.Add(time.Duration(r.n) * time.Second), Data: raw})
	r.tok = "tok2"
	name2 := r.handOut()
	if u := wantState(t, r.fold(), UnitHanded); u.Predecessor != name {
		t.Fatalf("predecessor = %q, want %q", u.Predecessor, name)
	}
	r.must(JournalAttemptLost, AttemptLostData{Name: name2, Cause: LostCauseCap})
	st := r.fold()
	wantState(t, st, UnitFailed)
	if _, ok := st.tokenForName(name2); ok {
		t.Fatal("capped name still live")
	}
	if got := st.insts[""].Lost["handed"]; got != 2 {
		t.Fatalf("Lost[handed] = %d, want 2", got)
	}
}

func TestWithdrawnAndLateVote(t *testing.T) {
	r := newIssuedRun(t)
	name := r.handOut()
	r.must(JournalWithdrawn, WithdrawnData{Name: name, Reason: "left source"})
	st := r.fold()
	wantState(t, st, UnitWithdrawn)
	if _, ok := st.tokenForName(name); ok {
		t.Fatal("withdrawn name still live")
	}
	if err := r.add(JournalWithdrawn, WithdrawnData{Name: name}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("withdrawing a retired name = %v, want ErrCorruptJournal", err)
	}

	r = newIssuedRun(t)
	name = r.handOut()
	if err := r.add(JournalLateVote, LateVoteData{Name: name}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("late_vote on a handed unit = %v, want ErrCorruptJournal", err)
	}
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Agent: "ag"})
	if err := r.add(JournalWithdrawn, WithdrawnData{Name: name}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("withdrawing a started unit = %v, want ErrCorruptJournal", err)
	}
	r.must(JournalLateVote, LateVoteData{Name: name, Output: json.RawMessage(`{}`)})
	st = r.fold()
	wantState(t, st, UnitDone)
	if _, ok := st.tokenForName(name); ok {
		t.Fatal("late-voted name still live")
	}
}

func TestEffectStartedAttachesToIntent(t *testing.T) {
	r := newIssuedRun(t)
	if err := r.add(JournalEffectStarted, EffectStartedData{Intent: 1, Pid: 7}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("effect_started without an intent = %v, want ErrCorruptJournal", err)
	}
	// Open a Command-style intent by hand: with no Pending edge it lands on Cmd.
	r.must(JournalEffectIntent, EffectIntentData{Effect: EffectData{Kind: "external", Adapter: "shell", Op: "run"}})
	seq := r.recs[len(r.recs)-1].Seq
	if err := r.add(JournalEffectStarted, EffectStartedData{Intent: seq + 5, Pid: 7}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("effect_started for another intent = %v, want ErrCorruptJournal", err)
	}
	r.must(JournalEffectStarted, EffectStartedData{Intent: seq, Pid: 7, Pgid: 7, Start: unitT0})
	got := r.fold().insts[""].Cmd
	if got == nil || got.Started == nil || got.Started.Pid != 7 || got.Started.Pgid != 7 {
		t.Fatalf("Cmd = %+v, want Started pid 7", got)
	}
}

func TestJournalV1FixtureFoldsUnchanged(t *testing.T) {
	raw, err := os.ReadFile("testdata/journal_v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var recs []JournalRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec JournalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	kinds := map[JournalKind]bool{}
	for _, rec := range recs {
		kinds[rec.Kind] = true
	}
	for _, k := range []JournalKind{JournalLeaseClaimed, JournalLeaseExpired, JournalInstructionIssued} {
		if !kinds[k] {
			t.Fatalf("fixture lacks %s", k)
		}
	}
	st, err := foldRun(recs, journalV1Def())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/journal_v1.digest")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.digest(); got != string(want) {
		t.Fatalf("v1 journal digest changed:\n got: %s\nwant: %s", got, want)
	}
	for _, in := range st.insts {
		if in.Unit != nil || in.Lost != nil {
			t.Fatalf("old journal produced a unit on %q", in.ID)
		}
	}
}

func TestUnitNameIndexRebuildsOnRefold(t *testing.T) {
	r := newIssuedRun(t)
	name := r.handOut()
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Agent: "ag"})
	r.must(JournalAttemptLost, AttemptLostData{Name: name, Cause: LostCauseLease})
	a, b := r.fold(), r.fold()
	if a.digest() != b.digest() {
		t.Fatal("digests differ across folds")
	}
	if len(a.names) != 1 || len(b.names) != 1 || a.names[name] != b.names[name] || !a.retired[name] || !b.retired[name] || a.nameToken[name] != r.tok {
		t.Fatalf("name index differs: %+v / %+v", a.names, b.names)
	}
}

func TestBackoffRetiredUnitReplaysHistoricalLiveness(t *testing.T) {
	for _, tc := range []struct {
		kind JournalKind
		data func(string) any
	}{
		{JournalHeartbeat, func(n string) any { return HeartbeatData{Name: n, Status: "stalled"} }},
		{JournalHeartbeat, func(n string) any { return HeartbeatData{Name: n, Status: "active"} }},
		{JournalHeartbeat, func(n string) any { return HeartbeatData{Name: n, Status: "died"} }},
		{JournalAttemptStarted, func(n string) any { return AttemptStartedData{Name: n, Agent: "ag"} }},
		{JournalPromptFetched, func(n string) any { return PromptFetchedData{Name: n} }},
		{JournalAttemptLost, func(n string) any { return AttemptLostData{Name: n, Cause: LostCauseLease} }},
		{JournalRecoveryOffered, func(n string) any { return RecoveryOfferedData{Unit: n} }},
		{JournalRecoveryAnswered, func(n string) any { return RecoveryAnsweredData{Unit: n, Choice: RecoveryResume} }},
	} {
		t.Run(tc.kind.String(), func(t *testing.T) {
			r := newIssuedRun(t)
			name := r.handOut()
			r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Agent: "ag"})
			r.must(JournalBackoffScheduled, BackoffScheduledData{Class: Unavailable, Delay: time.Second, Due: unitT0.Add(time.Minute)})
			before := r.fold().digest()
			r.must(tc.kind, tc.data(name))
			st := r.fold()
			if got := st.digest(); got != before {
				t.Fatalf("historical %s changed retired state:\n%s\nwant:\n%s", tc.kind, got, before)
			}
			wantState(t, st, UnitFailed)
			if _, _, ok := st.unitByName(name); ok {
				t.Fatal("historical liveness revived retired claim")
			}
		})
	}
}

func TestRetiredUnitHeartbeatWithoutBackoffRemainsCorrupt(t *testing.T) {
	r := newIssuedRun(t)
	name := r.handOut()
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Agent: "ag"})
	r.must(JournalRefused, RefusedData{Halt: true})
	if err := r.add(JournalHeartbeat, HeartbeatData{Name: name, Status: "stalled"}); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("heartbeat = %v, want corrupt journal", err)
	}
}

// an earlier revision: a fetch credits its offer's call only when the hand-out recorded
// one. A hand-out from before Call is its own call for ordering, but its
// fetch is no evidence against any other unit.
func TestFetchCreditNeedsARecordedCall(t *testing.T) {
	for _, tc := range []struct {
		call int64
		want int64
	}{{0, 0}, {7, 7}} {
		r := newIssuedRun(t)
		name := r.fold().unitName(r.fold().insts[""])
		r.must(JournalHandedOut, HandedOutData{Name: name, Token: r.tok, Session: "s1", Claim: "c1", Call: tc.call})
		r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Harness: "codex", Session: "s1", Agent: "k"})
		r.must(JournalPromptFetched, PromptFetchedData{Name: name, Claim: "c1"})
		if got := r.fold().fetchedCall["s1"]; got != tc.want {
			t.Errorf("hand-out Call %d: credited call %d, want %d", tc.call, got, tc.want)
		}
	}
}

// an earlier revision on wf-fanout: a snapshot fork turns its source's coordinator
// records into notes, so its history's segments keep their bindings from
// their session-bearing hand-outs; the fork's own segment stays unbound until
// its first coordinator.
func TestForkHistoryKeepsItsSegmentBindings(t *testing.T) {
	r := newIssuedRun(t)
	name := r.fold().unitName(r.fold().insts[""])
	r.must(JournalNote, NoteData{Text: "source coordinator not inherited by snapshot fork"})
	r.must(JournalHandedOut, HandedOutData{Name: name, Token: r.tok, Session: "c-coord", Harness: "codex", Segment: 1, Expires: unitT0.Add(2 * time.Minute)})
	r.must(JournalRewound, RewoundData{From: "source", Seq: int64(len(r.recs)), State: "snapshot 3"})
	st := r.fold()
	if len(st.segments) != 2 || st.segments[0].Harness != "codex" || st.segments[1].Harness != "" || st.segments[1].Cause != SegmentFork {
		t.Fatalf("segments = %+v; want the history bound to codex and the fork's own unbound", st.segments)
	}
}
