package wf

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type (
	dlGate struct{}
	dlAns  string
	dlWait struct{}
	dlObs  struct{ Up bool }
	dlDone struct{}
	dlStop struct{}
)

// dlCycle is a cycle over the folded records with the options the deadlines read.
func dlCycle(t *testing.T, recs []JournalRecord, d *Def) *cycle {
	t.Helper()
	st, err := foldRun(recs, d)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: EngineOptions{SpawnWindow: 3 * time.Minute, EffectPoll: 2 * time.Second, UnstartedGrace: 5 * time.Minute}}
	return &cycle{e: e, st: st, recs: recs}
}

// dlStarted is the run_started record of d, journaled at unitT0.
func dlStarted(t *testing.T, d *Def) []JournalRecord {
	t.Helper()
	j := MemJournal()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return unitT0 }}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	unlock, _ := j.Lock(run)
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// wantDeadlines checks the kinds and due times, in order.
func wantDeadlines(t *testing.T, got []deadline, want ...deadline) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("deadlines = %+v, want %+v", got, want)
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.Kind || !g.Due.Equal(w.Due) || g.Unit != w.Unit || g.Intent != w.Intent {
			t.Fatalf("deadline %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestDeadlinesListUnitWindows(t *testing.T) {
	r := newIssuedRun(t)
	c := dlCycle(t, r.recs, r.def)
	wantDeadlines(t, c.deadlines(unitT0))

	name := r.handOut()
	handed := r.recs[len(r.recs)-1].At
	c = dlCycle(t, r.recs, r.def)
	// A unit handed to a named session is never lost before a claim (B1); its
	// coordinator is offered it again once UnstartedGrace passes.
	wantDeadlines(t, c.deadlines(unitT0), deadline{Kind: deadlineHandOut, Unit: name, Due: handed.Add(5 * time.Minute)})

	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Agent: "ag", Expires: unitT0.Add(45 * time.Minute)})
	started := r.recs[len(r.recs)-1].At
	c = dlCycle(t, r.recs, r.def)
	wantDeadlines(t, c.deadlines(unitT0),
		deadline{Kind: deadlineLease, Unit: name, Due: unitT0.Add(45 * time.Minute)},
		deadline{Kind: deadlineSpawn, Unit: name, Due: started.Add(3 * time.Minute)})

	// A prompt fetch is the spawn signal: the unit lives by its lease alone.
	r.must(JournalPromptFetched, PromptFetchedData{Name: name})
	c = dlCycle(t, r.recs, r.def)
	wantDeadlines(t, c.deadlines(unitT0),
		deadline{Kind: deadlineLease, Unit: name, Due: unitT0.Add(45 * time.Minute)})

	// Late attachment retains the first start and lease; replay begins watching
	// the transcript without extending either timestamp.
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Transcript: "/t", Expires: unitT0.Add(time.Hour)})
	c = dlCycle(t, r.recs, r.def)
	wantDeadlines(t, c.deadlines(unitT0),
		deadline{Kind: deadlineLease, Unit: name, Due: unitT0.Add(45 * time.Minute)},
		deadline{Kind: deadlineStall, Unit: name, Due: started.Add(c.stallThreshold(c.st.insts[""]))})
	u := c.st.insts[""].Unit
	if !u.StartedAt.Equal(started) || u.Claim.Agent != "ag" || u.Claim.Transcript != "/t" {
		t.Fatalf("replayed attachment changed start or ownership: %+v", u)
	}
}

func TestDeadlinesWaveClaimWithoutTranscript(t *testing.T) {
	r := newIssuedRun(t)
	name := r.handOut()
	r.must(JournalAttemptStarted, AttemptStartedData{Name: name, Harness: "wave", Session: "wave", Agent: "wave"})
	r.must(JournalPromptFetched, PromptFetchedData{Name: name})
	c := dlCycle(t, r.recs, r.def)
	wantDeadlines(t, c.deadlines(unitT0.Add(time.Hour)))
}

func TestDeadlinesListOldLease(t *testing.T) {
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
		if rec.Kind == JournalLeaseClaimed {
			break
		}
	}
	d, err := decodeData(recs[len(recs)-1])
	if err != nil {
		t.Fatal(err)
	}
	c := dlCycle(t, recs, journalV1Def())
	wantDeadlines(t, c.deadlines(recs[0].At), deadline{Kind: deadlineLease, Due: d.(*LeaseClaimedData).Expires})
	if got := c.deadlines(recs[0].At)[0].Instance; got != recs[len(recs)-1].Instance {
		t.Fatalf("lease instance = %q, want %q", got, recs[len(recs)-1].Instance)
	}
}

func TestDeadlinesListDetachedEffect(t *testing.T) {
	r := newIssuedRun(t)
	r.must(JournalEffectIntent, EffectIntentData{Effect: EffectData{Kind: "external", Adapter: "shell", Op: "run"}})
	seq := r.recs[len(r.recs)-1].Seq
	r.must(JournalEffectStarted, EffectStartedData{Intent: seq, Pid: 7, Pgid: 7, Start: unitT0, Max: 10 * time.Minute})
	c := dlCycle(t, r.recs, r.def)
	wantDeadlines(t, c.deadlines(unitT0.Add(5*time.Second)),
		deadline{Kind: deadlineEffectPoll, Intent: seq, Due: unitT0.Add(6 * time.Second)},
		deadline{Kind: deadlineEffectMax, Intent: seq, Due: unitT0.Add(10 * time.Minute)})

	r.must(JournalEffectResult, EffectResultData{Intent: seq, OK: true})
	c = dlCycle(t, r.recs, r.def)
	wantDeadlines(t, c.deadlines(unitT0.Add(5*time.Second)))
}

func TestDeadlinesGateWaitBackoff(t *testing.T) {
	// A timed gate.
	g := New("dl-gate")
	ans := NewEnum(g, "dl-answer", dlAns("go"))
	gate := NewGate[dlGate, dlAns](g, "Go?", ans, PolicyAnswerable)
	stop := NewTerminal[dlStop](g, Stopped)
	gate.Answers(On(dlAns("go"), gate.To(stop, "go"))).Timeout(5*time.Minute, gate.To(stop, "timeout"))
	c := dlCycle(t, dlStarted(t, g), g)
	wantDeadlines(t, c.deadlines(unitT0), deadline{Kind: deadlineGateTimeout, Due: unitT0.Add(5 * time.Minute)})

	// A timed wait with a poll: the poll is the next multiple after now.
	w := New("dl-wait")
	wt := NewWait[dlWait, dlObs](w, Observe[dlObs]("x", "up", nil), "up", func(o dlObs) bool { return o.Up })
	done := NewTerminal[dlDone](w, Success)
	wstop := NewTerminal[dlStop](w, Stopped)
	wt.Ready(wt.To(done, "up")).Poll(10*time.Second).Timeout(time.Minute, wt.To(wstop, "timeout"))
	c = dlCycle(t, dlStarted(t, w), w)
	wantDeadlines(t, c.deadlines(unitT0.Add(25*time.Second)),
		deadline{Kind: deadlineWaitTimeout, Due: unitT0.Add(time.Minute)},
		deadline{Kind: deadlineWaitPoll, Due: unitT0.Add(30 * time.Second)})
	wantDeadlines(t, c.deadlines(unitT0.Add(30 * time.Second))[1:], deadline{Kind: deadlineWaitPoll, Due: unitT0.Add(40 * time.Second)})
	if d, ok := c.dueFor(c.st.insts[""], deadlineGateTimeout, unitT0); ok {
		t.Fatalf("a wait has a gate timeout: %+v", d)
	}

	// A backoff.
	r := newIssuedRun(t)
	due := unitT0.Add(20 * time.Second)
	r.must(JournalBackoffScheduled, BackoffScheduledData{Class: Unavailable, Delay: 20 * time.Second, Due: due})
	c = dlCycle(t, r.recs, r.def)
	wantDeadlines(t, c.deadlines(unitT0), deadline{Kind: deadlineBackoff, Due: due})
}

func TestDeadlineKindNames(t *testing.T) {
	seen := map[string]deadlineKind{}
	for k := deadlineGateTimeout; k <= deadlineEndSettle; k++ {
		s := k.String()
		if s == "" || s == deadlineKind(0).String() {
			t.Errorf("kind %d has no name", int(k))
		}
		if prev, dup := seen[s]; dup {
			t.Errorf("kinds %d and %d share the name %q", int(prev), int(k), s)
		}
		seen[s] = k
	}
}
