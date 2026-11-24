package wf

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type (
	arItem struct{ ID string }
	arRes  struct{}
	arFan  struct{}
	arWork struct{}
	arOut  struct{ V string }
	arKDon struct{}
	arEnd  struct{}
	arCap  struct{}
	arPart struct{}
	arLoop struct{}
)

// arrivedAtScan is the reference: arrivedAt as it read the journal before the
// fold carried the time, a backwards scan for the latest transition, else the
// child_spawned that created the instance, else the first record.
func arrivedAtScan(recs []JournalRecord, in *instance) time.Time {
	for i := len(recs) - 1; i >= 0; i-- {
		if r := recs[i]; r.Kind == JournalTransition && r.Instance == in.ID {
			return r.At
		}
	}
	if in.ID == "" {
		return recs[0].At
	}
	for _, r := range recs {
		if r.Kind != JournalChildSpawned {
			continue
		}
		if d, err := decodeData(r); err == nil && d.(*ChildSpawnedData).ID == in.ID {
			return r.At
		}
	}
	return recs[0].At
}

// arClock ticks one second per call, so every journaled At differs.
func arClock() func() time.Time {
	t := unitT0
	return func() time.Time { t = t.Add(time.Second); return t }
}

// arKid is a one-agent child that loops once (a revisit) before it ends.
func arKid() *Def {
	kid := New("ar-kid")
	Typed[arItem, arRes](kid)
	w := NewAgent[arWork, arOut](kid)
	done := NewTerminal[arKDon](kid, Success)
	bad := NewTerminal[arPart](kid, Stopped)
	w.Spec(PromptText("Work."), Retry(Const(1), w.To(bad, "gave up")))
	again := w.To(w, "again")
	w.Route(func(arOut, Ctx) Edge[arWork] { return again }, again)
	MaxVisits(Const(2), w.To(done, "cap"))
	return kid
}

func arItems(n int) Source[arItem] {
	var xs []arItem
	for i := 1; i <= n; i++ {
		xs = append(xs, arItem{ID: string(rune('a'+i%26)) + strings.Repeat("x", i/26)})
	}
	return FromValues(xs...)
}

// arMapDef is a Map over n items with arKid; again re-enters the Map once, so
// its children spawn a second time with the #2 suffix.
func arMapDef(n int, again bool) *Def {
	d := New("ar-map")
	m := NewMap[arFan, arItem, string, arRes](d, arItems(n), func(i arItem) string { return i.ID }, Typed[arItem, arRes](arKid()))
	end := NewTerminal[arEnd](d, Success)
	part := NewTerminal[arPart](d, Stopped)
	if again {
		met := m.To(m, "again")
		MaxVisits(Const(2), m.To(end, "cap"))
		m.Join(AllOK(), met, m.To(part, "partial"))
	} else {
		m.Join(AllOK(), m.To(end, "met"), m.To(part, "partial"))
	}
	return d
}

// arLoopDef is a root agent that is revisited once: Visits > 1, and the later
// transition must win.
func arLoopDef() *Def {
	d := New("ar-loop")
	w := NewAgent[arLoop, arOut](d)
	end := NewTerminal[arEnd](d, Success)
	bad := NewTerminal[arPart](d, Stopped)
	w.Spec(PromptText("Loop."), Retry(Const(1), w.To(bad, "gave up")))
	again := w.To(w, "again")
	w.Route(func(arOut, Ctx) Edge[arLoop] { return again }, again)
	MaxVisits(Const(3), w.To(end, "cap"))
	return d
}

// arDrive runs d to its end (or until nothing is handed out), submitting an
// empty output for every instruction, and returns the journal.
func arDrive(t *testing.T, d *Def) []JournalRecord {
	t.Helper()
	j := MemJournal()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{d}, Clock: arClock()}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		ins, err := e.Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(ins) == 0 {
			break
		}
		for _, in := range ins {
			if _, err := e.Submit(run, Submission{Token: in.Token, Output: json.RawMessage(`{"V":"x"}`)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	unlock, _ := j.Lock(run)
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestArrivedAtMatchesScanEveryPrefix(t *testing.T) {
	_, fold, _, _ := recordRun(t)
	raw, err := os.ReadFile("testdata/journal_v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var v1 []JournalRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec JournalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		v1 = append(v1, rec)
	}
	cases := []struct {
		name   string
		def    *Def
		recs   []JournalRecord
		revist bool // some instance must have been entered twice
		spawn2 bool // a child must carry the #2 suffix
	}{
		{name: "fold", def: foldDef(), recs: fold},
		{name: "journal_v1", def: journalV1Def(), recs: v1},
		{name: "revisit", def: arLoopDef(), recs: arDrive(t, arLoopDef()), revist: true},
		{name: "map", def: arMapDef(3, false), recs: arDrive(t, arMapDef(3, false)), revist: true},
		{name: "map re-entered", def: arMapDef(2, true), recs: arDrive(t, arMapDef(2, true)), revist: true, spawn2: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spawns, transitions := 0, 0
			var sawRevisit, sawSuffix bool
			for _, r := range tc.recs {
				switch r.Kind {
				case JournalChildSpawned:
					spawns++
					if strings.Contains(r.Instance+string(r.Data), "#2") {
						sawSuffix = true
					}
				case JournalTransition:
					transitions++
				}
			}
			final, err := foldRun(tc.recs, tc.def)
			if err != nil {
				t.Fatal(err)
			}
			for _, in := range final.insts {
				for _, v := range in.Visits {
					if v > 1 {
						sawRevisit = true
					}
				}
			}
			if tc.revist && !sawRevisit {
				t.Fatalf("fixture has no revisit (%d transitions)", transitions)
			}
			if tc.spawn2 && !sawSuffix {
				t.Fatalf("fixture has no #2 child among %d spawns", spawns)
			}
			for k := 1; k <= len(tc.recs); k++ {
				st, err := foldRun(tc.recs[:k], tc.def)
				if err != nil {
					t.Fatalf("fold of %d records: %v", k, err)
				}
				c := &cycle{st: st, recs: tc.recs[:k]}
				for _, id := range st.order {
					in := st.insts[id]
					if got, want := c.arrivedAt(in), arrivedAtScan(tc.recs[:k], in); !got.Equal(want) {
						t.Fatalf("after %d records (%s) instance %q arrived at %v, the scan says %v", k, tc.recs[k-1].Kind, id, got, want)
					}
				}
			}
		})
	}
}

// arWide is the cycle over a Map of n items with every child spawned and pending.
func arWide(t testing.TB, n int) *cycle {
	t.Helper()
	d := arMapDef(n, false)
	j := MemJournal()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{d}, Clock: arClock()}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ins, err := e.HandOut(run, 0); err != nil || len(ins) != 0 {
		t.Fatalf("HandOut(0) = %v, %v", ins, err)
	}
	unlock, _ := j.Lock(run)
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	st, err := foldRun(recs, d)
	if err != nil {
		t.Fatal(err)
	}
	c := &cycle{e: e, st: st, recs: recs}
	if got := len(c.pendingUnits()); got != n {
		t.Fatalf("pending units = %d, want %d", got, n)
	}
	return c
}

func TestArrivedAtCostLinearInWidth(t *testing.T) {
	cost := func(n int) float64 {
		c := arWide(t, n)
		return testing.AllocsPerRun(5, func() { c.waitCounts() })
	}
	small, wide := cost(50), cost(200)
	if ratio := wide / small; ratio >= 8 {
		t.Fatalf("waitCounts allocs: %.0f at width 50, %.0f at 200 (x%.1f); the cost grows faster than the width", small, wide, ratio)
	}
}

func BenchmarkWaitHintWideMap(b *testing.B) {
	c := arWide(b, 200)
	b.ResetTimer()
	for range b.N {
		c.waitCounts()
	}
}
