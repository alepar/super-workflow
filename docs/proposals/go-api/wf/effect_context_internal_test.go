package wf

import (
	"encoding/json"
	"testing"
	"time"
)

type contextEffectRuntime struct {
	got    EffectContext
	legacy bool
}

func (r *contextEffectRuntime) Ops() []string { return []string{"test.write"} }
func (r *contextEffectRuntime) Read(string, ObservationDesc) (json.RawMessage, error) {
	return nil, nil
}
func (r *contextEffectRuntime) Check(string, ExternalEffect) (EffectStatus, error) {
	return EffectNotDone, nil
}
func (r *contextEffectRuntime) Apply(string, ExternalEffect) (json.RawMessage, error) {
	r.legacy = true
	return nil, nil
}
func (r *contextEffectRuntime) ApplyAt(_ string, at EffectContext, _ ExternalEffect) (json.RawMessage, error) {
	r.got = at
	return nil, nil
}

func TestRecoveredEffectKeepsOwningInstanceContext(t *testing.T) {
	rt := &contextEffectRuntime{}
	reg := NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	root := newInstance("", "", New("root"))
	parent := newInstance("p#1", "", New("parent"))
	child := newInstance("child#2", "p#1", New("child"))
	child.Cur = "Work"
	effect := EffectData{Adapter: "test", Op: "write", Key: "work"}
	child.Pending = &pendingEdge{Open: 1, OpenEffect: effect}
	c := &cycle{run: "run", dry: true, e: &Engine{reg: reg, opts: EngineOptions{Clock: func() time.Time { return time.Time{} }}}, st: &runState{started: &RunStartedData{}, insts: map[string]*instance{"": root, "p#1": parent, "child#2": child}}, recs: []JournalRecord{{Kind: JournalNote, Instance: "p#1", State: "Unrelated"}}}
	resolved, err := c.recoverEdgeIntent(child, 1, 0, effect)
	if err != nil || !resolved {
		t.Fatalf("recover effect: %v %v", resolved, err)
	}
	want := EffectContext{Run: "run", Instance: "child#2", State: "parent/child/Work"}
	if rt.got != want || rt.legacy {
		t.Fatalf("effect context %+v, legacy=%v; want %+v", rt.got, rt.legacy, want)
	}
}
