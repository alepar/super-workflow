package wf

import (
	"encoding/json"
	"testing"
	"time"
)

type (
	rqFan  struct{}
	rqEnd  struct{}
	rqWork struct{}
	rqKEnd struct{}
	rqOut  struct{}
	rqRes  struct{}
)

type rqItem struct {
	ID   string
	Wait bool
}

func (i rqItem) WaitingOnDeps() bool { return i.Wait }

type rqSource struct{ items []rqItem }

func (rqSource) Ops() []string { return []string{"*"} }
func (s *rqSource) Read(string, ObservationDesc) (json.RawMessage, error) {
	return json.Marshal(s.items)
}
func (*rqSource) Apply(string, ExternalEffect) (json.RawMessage, error) { return nil, nil }
func (*rqSource) Check(string, ExternalEffect) (EffectStatus, error)    { return EffectUnknown, nil }

func rqDef() *Def {
	kid := New("rq-kid")
	child := Typed[rqItem, rqRes](kid)
	work := NewAgent[rqWork, rqOut](kid)
	kend := NewTerminal[rqKEnd](kid, Success)
	work.Spec(PromptText("Work."), Retry(Const(1), work.To(kend, "gave up")))
	next := work.To(kend, "done")
	work.Route(func(rqOut, Ctx) Edge[rqWork] { return next }, next)

	d := New("rq-wait")
	src := FromObservation[rqItem](Observe[[]rqItem]("kv", "items", nil))
	m := NewMap[rqFan, rqItem, string, rqRes](d, src, func(i rqItem) string { return i.ID }, child).Requery()
	end := NewTerminal[rqEnd](d, Success)
	m.Parallel(5).Join(AllOK(), m.To(end, "met"), m.To(end, "partial"))
	return d
}

func TestWaitingOnDepsCounted(t *testing.T) {
	d := rqDef()
	rt := &rqSource{items: []rqItem{{ID: "a"}, {ID: "b", Wait: true}, {ID: "c", Wait: true}}}
	reg := NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	e := NewEngine(MemJournal(), reg, EngineOptions{Defs: []*Def{d}, Clock: func() time.Time { return now }}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := func() (n int, spawned []string) {
		err := e.withRun(run, func(c *cycle) error {
			node := c.st.insts[""].node()
			n = len(node.kind.(*mapKind).waiting(c, c.st.insts[""], node))
			for _, ch := range c.entryKids(c.st.insts[""], node) {
				spawned = append(spawned, ch.ID)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return n, spawned
	}

	ins, err := e.HandOut(run, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Child != "rqFan[a]" {
		t.Fatalf("HandOut = %+v, want only rqFan[a]: b and c wait on dependencies", ins)
	}
	if n, spawned := count(); n != 2 || len(spawned) != 1 {
		t.Fatalf("waiting = %d, spawned = %v, want 2 waiting and only a spawned", n, spawned)
	}

	rt.items[1].Wait = false // b's dependencies are met
	ins, err = e.HandOut(run, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Child != "rqFan[b]" {
		t.Fatalf("HandOut after b became ready = %+v, want rqFan[b]", ins)
	}
	if n, _ := count(); n != 1 {
		t.Fatalf("waiting = %d, want 1 (only c)", n)
	}
}
