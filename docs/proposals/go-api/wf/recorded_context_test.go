package wf

import (
	"encoding/json"
	"testing"
)

func TestHarvestRecordedReferences(t *testing.T) {
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	records := []JournalRecord{
		{Run: "r", Seq: 1, Kind: JournalRunStarted, Data: raw(RunStartedData{Def: "flow", Shape: map[string]string{"flow/Review": "agent"}})},
		{Run: "r", Seq: 2, State: "Seed", Kind: JournalAccepted, Data: raw(AcceptedData{Computed: true, Output: json.RawMessage(`42`)})},
		{Run: "r", Seq: 3, State: "Review", Kind: JournalAccepted, Data: raw(AcceptedData{Output: json.RawMessage(`{}`), Context: &CheckContexts{Version: 1, Checks: []RecordedCheck{{Name: "seed", Reads: []RecordedRead{{Kind: "computed", Def: "flow", State: "Seed", Seq: 2}}}}}})},
	}
	entries, err := Harvest(records, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || string(entries[0].Context.Checks[0].Reads[0].Value) != "42" {
		t.Fatalf("entries: %+v", entries)
	}
	for _, mutation := range []func(*RecordedRead){func(r *RecordedRead) { r.Seq = 3 }, func(r *RecordedRead) { r.Instance = "other" }, func(r *RecordedRead) { r.Def = "other" }, func(r *RecordedRead) { r.Kind = "output" }} {
		var d AcceptedData
		_ = json.Unmarshal(records[2].Data, &d)
		mutation(&d.Context.Checks[0].Reads[0])
		records2 := append([]JournalRecord(nil), records...)
		records2[2].Data = raw(d)
		if _, err := Harvest(records2, t.TempDir()); err == nil {
			t.Fatal("accepted malformed producer ref")
		}
	}
}

func TestRecordedContextLegacyEmptyAndChildIsolation(t *testing.T) {
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	records := []JournalRecord{
		{Run: "r", Seq: 1, Kind: JournalRunStarted, Data: raw(RunStartedData{Def: "parent", Shape: map[string]string{"child/Review": "agent"}})},
		{Run: "r", Seq: 2, Kind: JournalChildSpawned, State: "Fan", Data: raw(ChildSpawnedData{ID: "one", Def: "child", State: "Fan"})},
		{Run: "r", Seq: 3, Kind: JournalChildSpawned, State: "Fan", Data: raw(ChildSpawnedData{ID: "two", Def: "child", State: "Fan"})},
		{Run: "r", Seq: 4, Kind: JournalAccepted, Instance: "one", State: "Seed", Data: raw(AcceptedData{Computed: true, Output: json.RawMessage(`1`)})},
		{Run: "r", Seq: 5, Kind: JournalAccepted, Instance: "two", State: "Seed", Data: raw(AcceptedData{Computed: true, Output: json.RawMessage(`2`)})},
		{Run: "r", Seq: 6, Kind: JournalAccepted, Instance: "one", State: "Review", Data: raw(AcceptedData{Output: json.RawMessage(`true`), Context: &CheckContexts{Version: 1, Checks: []RecordedCheck{{Name: "same-state", Reads: []RecordedRead{{Kind: "computed", Def: "child", State: "Seed", Instance: "one", Seq: 4}}}}}})},
		{Run: "r", Seq: 7, Kind: JournalAccepted, Instance: "two", State: "Review", Data: raw(AcceptedData{Output: json.RawMessage(`true`)})},
	}
	entries, err := Harvest(records, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].State != "child/Review" || string(entries[0].Context.Checks[0].Reads[0].Value) != "1" || entries[1].Context != nil {
		t.Fatalf("entries: %+v", entries)
	}
	var d AcceptedData
	_ = json.Unmarshal(records[5].Data, &d)
	d.Context.Checks[0].Reads[0].Seq = 5
	records[5].Data = raw(d)
	if _, err := Harvest(records, t.TempDir()); err == nil {
		t.Fatal("cross-child producer accepted")
	}
	d.Context = &CheckContexts{Version: 1, Checks: []RecordedCheck{}}
	records[5].Data = raw(d)
	entries, err = Harvest(records, t.TempDir())
	if err != nil || entries[0].Context == nil || len(entries[0].Context.Checks) != 0 {
		t.Fatalf("known-empty: %+v %v", entries, err)
	}
}

func TestRecordingMarksUnsupportedAndLargeReads(t *testing.T) {
	d := New("capture")
	in := &instance{def: d, Attempt: 1}
	c := &cycle{st: &runState{started: &RunStartedData{Params: map[string]string{"big": string(make([]byte, 5000))}}}}
	ck := RecordedCheck{Name: "read"}
	ctx := &recordingCtx{live: &runCtx{c: c, in: in}, record: &ck}
	ctx.Param(d.Param("big"))
	ctx.Eval(Const(2))
	if len(ck.Incomplete) != 2 || len(ck.Reads) != 0 {
		t.Fatalf("record: %+v", ck)
	}
}

func TestHarvestObservedBoundDescriptorValidation(t *testing.T) {
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	bound := ObservationDesc{Adapter: "a", Op: "read", Args: map[string]any{"key": "42"}, Result: "bool"}
	unbound := bound
	unbound.Args = map[string]any{"key": "{ticket}"}
	records := []JournalRecord{
		{Run: "r", Seq: 1, Kind: JournalRunStarted, Data: raw(RunStartedData{Def: "flow", Shape: map[string]string{"flow/Review": "agent"}})},
		{Run: "r", Seq: 2, State: "Review", Kind: JournalObserved, Data: raw(ObservedData{Desc: bound, Value: json.RawMessage(`true`)})},
		{Run: "r", Seq: 3, State: "Review", Kind: JournalAccepted, Data: raw(AcceptedData{Output: json.RawMessage(`true`), Context: &CheckContexts{Version: 1, Checks: []RecordedCheck{{Name: "obs", Reads: []RecordedRead{{Kind: "observed", Def: "flow", State: "Review", Desc: &unbound, Name: canonDesc(bound), Seq: 2}}}}}})},
	}
	entries, err := Harvest(records, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Context.Checks[0].Reads[0].Value) != "true" {
		t.Fatal("lost observation")
	}
	bound.Args = map[string]any{"key": "different"}
	records[1].Data = raw(ObservedData{Desc: bound, Value: json.RawMessage(`true`)})
	if _, err := Harvest(records, t.TempDir()); err == nil {
		t.Fatal("accepted mismatched bound descriptor")
	}
}

func TestResolveRejectsUnprovenInlineReads(t *testing.T) {
	for _, r := range []RecordedRead{
		{Kind: "output", Value: json.RawMessage(`true`)},
		{Kind: "computed", Value: json.RawMessage(`null`)},
		{Kind: "child", Value: json.RawMessage(`{}`)},
		{Kind: "joined", Value: json.RawMessage(`[]`)},
		{Kind: "observed", Value: json.RawMessage(`true`)},
		{Kind: "param", Name: "p", Value: json.RawMessage(`42`)},
		{Kind: "count", Name: "n", Value: json.RawMessage(`"bad"`)},
		{Kind: "attempt", Value: json.RawMessage(`1.5`)},
		{Kind: "input", Value: json.RawMessage(`invalid`)},
		{Kind: "input", Value: append(append(json.RawMessage(`"`), make([]byte, 5000)...), '"')},
	} {
		context := &CheckContexts{Version: 1, Checks: []RecordedCheck{{Name: "check", Reads: []RecordedRead{r}}}}
		if _, err := resolveCheckContexts(context, JournalRecord{Run: "r", Seq: 9}, nil, ""); err == nil {
			t.Fatalf("accepted unproven inline read: %+v", r)
		}
	}
}

func TestCaptureAbsentProducerIsIncomplete(t *testing.T) {
	d := New("capture")
	a := NewAgent[fA, bool](d)
	in := &instance{def: d, Outputs: map[string]json.RawMessage{}}
	ck := RecordedCheck{Name: "absent"}
	x := &recordingCtx{live: &runCtx{in: in}, record: &ck}
	if v, err := x.value(ctxKey{kind: "output", ref: a.n}); err != nil || len(v.(json.RawMessage)) != 0 {
		t.Fatalf("live read: %v %v", v, err)
	}
	if len(ck.Incomplete) != 1 || len(ck.Reads) != 0 {
		t.Fatalf("absence invented provenance: %+v", ck)
	}
}
