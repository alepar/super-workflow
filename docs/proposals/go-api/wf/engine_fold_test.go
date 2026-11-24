package wf

import (
	"encoding/json"
	"errors"
	"testing"
)

type (
	fA    struct{}
	fB    struct{}
	fDone struct{}
	fBad  struct{}
	fOut  struct{ V string }
)

// foldDef: A -> B -> Done, with a counter and an effect-free edge, so the
// journal holds issue, submit, accept, scope-less transition and terminal
// records.
func foldDef() *Def {
	d := New("fold")
	n := d.Counter("n")
	a := NewAgent[fA, fOut](d)
	c := NewCompute[fB, fOut](d, func(c Ctx) (fOut, error) { return fOut{V: "c"}, nil })
	done := NewTerminal[fDone](d, Success)
	bad := NewTerminal[fBad](d, Stopped)
	a.Spec(PromptText("p"), Retry[fA](Const(2), a.To(bad, "x")))
	toC := a.To(c, "go").With(n.Inc())
	a.Route(func(fOut, Ctx) Edge[fA] { return toC }, toC)
	toDone := c.To(done, "done")
	c.Route(func(fOut, Ctx) Edge[fB] { return toDone }, toDone)
	return d
}

// recordRun drives foldDef to its terminal and returns the journal records and
// the digest the live cycle held after each of them.
func recordRun(t *testing.T) (*Def, []JournalRecord, []string, []int64) {
	t.Helper()
	d := foldDef()
	j := MemJournal()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{d}}).(*Engine)
	var digests []string
	var seqs []int64
	e.afterAppend = func(c *cycle, rec JournalRecord) {
		digests = append(digests, c.st.digest())
		seqs = append(seqs, rec.Seq)
	}
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("Next = %v, %v", ins, err)
	}
	// One refused attempt first, so the fold sees a re-issue.
	if _, err := e.Fail(run, ins[0].Token, "retry me"); err != nil {
		t.Fatal(err)
	}
	ins, _ = e.Next(run, "")
	if _, err := e.Submit(run, Submission{Token: ins[0].Token, Output: json.RawMessage(`{"V":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	unlock, _ := j.Lock(run)
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	return d, recs, digests, seqs
}

func TestRefoldReachesSameState(t *testing.T) {
	d, recs, digests, _ := recordRun(t)
	if len(digests) != len(recs) {
		t.Fatalf("hook saw %d records, journal has %d", len(digests), len(recs))
	}
	if last := recs[len(recs)-1].Kind; last != JournalRunEnded {
		t.Fatalf("run did not end: last record %s", last)
	}
	for i := range recs {
		st, err := foldRun(recs[:i+1], d)
		if err != nil {
			t.Fatalf("fold of the first %d records: %v", i+1, err)
		}
		if got := st.digest(); got != digests[i] {
			t.Fatalf("after record %d (%s) the refold differs from the live state:\nlive:   %s\nrefold: %s", i+1, recs[i].Kind, digests[i], got)
		}
	}
}

func TestCycleSeqMatchesJournal(t *testing.T) {
	_, recs, _, seqs := recordRun(t)
	for i, r := range recs {
		if r.Seq != int64(i+1) || seqs[i] != r.Seq {
			t.Fatalf("record %d: journal Seq %d, cycle Seq %d, want %d", i, r.Seq, seqs[i], i+1)
		}
	}
}

func TestFoldRejectsUnknownKindAndBadPayload(t *testing.T) {
	d, recs, _, _ := recordRun(t)
	bad := append([]JournalRecord{}, recs[:2]...)
	bad = append(bad, JournalRecord{Seq: 3, Kind: JournalKind(999), Run: recs[0].Run})
	if _, err := foldRun(bad, d); err == nil {
		t.Fatal("unknown kind folded")
	}
	torn := append([]JournalRecord{}, recs[:2]...)
	torn[1].Data = json.RawMessage(`{"Token": 7}`)
	if _, err := foldRun(torn, d); err == nil {
		t.Fatal("undecodable payload folded")
	}
}

type (
	fSel   struct{}
	fWrite struct{}
	fEnd   struct{}
	fFiles struct {
		Files string `json:"findingFiles"`
	}
)

const fFilesText = "f1\t{\"id\":1}\nf2\t{\"id\":2}\n"

// stdinFoldDef: a compute selects the payload, a keyed idempotent command
// writes it from stdin, then a terminal.
func stdinFoldDef() *Def {
	d := New("fold-stdin")
	sel := NewCompute[fSel, fFiles](d, func(Ctx) (fFiles, error) { return fFiles{Files: fFilesText}, nil })
	w := NewCommand[fWrite](d, "write").Stdin("{findingFiles}").Key("write").Idempotent()
	end := NewTerminal[fEnd](d, Success)
	toW := sel.To(w, "write")
	sel.Route(func(fFiles, Ctx) Edge[fSel] { return toW }, toW)
	toEnd := w.To(end, "written")
	w.Route(func(RunResult, Ctx) Edge[fWrite] { return toEnd }, toEnd)
	return d
}

// fakeShell serves shell.run with a zero exit and never finds a prior effect.
type fakeShell struct{}

func (fakeShell) Ops() []string { return []string{"shell.run"} }
func (fakeShell) Read(string, ObservationDesc) (json.RawMessage, error) {
	return nil, errors.New("fakeShell reads nothing")
}
func (fakeShell) Apply(string, ExternalEffect) (json.RawMessage, error) {
	return json.Marshal(RunResult{Exit: 0})
}
func (fakeShell) Check(string, ExternalEffect) (EffectStatus, error) { return EffectUnknown, nil }

// Every prefix of a run with a stdin command refolds to the state the live
// cycle held, including the prefix that ends before the writer's effect_intent
// (a death between the compute and the writer) and the intent that carries the
// bound stdin.
func TestRefoldStdinCommandEveryPrefix(t *testing.T) {
	d := stdinFoldDef()
	reg := NewAdapters()
	if err := reg.Register(fakeShell{}); err != nil {
		t.Fatal(err)
	}
	j := MemJournal()
	e := NewEngine(j, reg, EngineOptions{Defs: []*Def{d}}).(*Engine)
	var digests []string
	e.afterAppend = func(c *cycle, rec JournalRecord) { digests = append(digests, c.st.digest()) }
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	unlock, _ := j.Lock(run)
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != len(recs) {
		t.Fatalf("hook saw %d records, journal has %d", len(digests), len(recs))
	}
	if last := recs[len(recs)-1].Kind; last != JournalRunEnded {
		t.Fatalf("run did not end: last record %s", last)
	}
	intentAt, accepted := -1, -1
	for i, r := range recs {
		switch r.Kind {
		case JournalEffectIntent:
			intentAt = i
		case JournalAccepted:
			if accepted < 0 {
				accepted = i
			}
		}
	}
	if accepted < 0 || intentAt <= accepted {
		t.Fatalf("compute accepted at %d, writer intent at %d: want the compute first", accepted, intentAt)
	}
	for i := range recs {
		st, err := foldRun(recs[:i+1], d)
		if err != nil {
			t.Fatalf("fold of the first %d records: %v", i+1, err)
		}
		if got := st.digest(); got != digests[i] {
			t.Fatalf("after record %d (%s) the refold differs from the live state:\nlive:   %s\nrefold: %s", i+1, recs[i].Kind, digests[i], got)
		}
	}
	var in EffectIntentData
	if err := json.Unmarshal(recs[intentAt].Data, &in); err != nil {
		t.Fatal(err)
	}
	if got := in.Effect.Args["stdin"]; got != fFilesText {
		t.Errorf("effect_intent stdin = %q, want the compute's Files %q", got, fFilesText)
	}
}
