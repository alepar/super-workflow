package wf_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// TestCheckObservesFrom covers submission-scoped check observations: the
// descriptor is built from the decoded submission (here its ID) and bound with
// the instance scope ({tag} from a run param), read and journaled with the
// check's reads, served to the check's Observed, and left out of the Stale
// digest that the issue-time reads feed.
func TestCheckObservesFrom(t *testing.T) {
	obsFor := func(id string) wf.Observation[eRes] {
		return wf.Observe[eRes]("x", "y", map[string]any{"id": id, "tag": "{tag}"})
	}
	d := wf.New("observes-from")
	d.Param("tag")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	stale := wf.NewTerminal[eStale](d, wf.Stopped)
	a.Spec(wf.PromptText("p"),
		wf.Retry[eA](wf.Const(3), a.To(bad, "x")).OnClass(wf.Stale, a.To(stale, "stale")),
		wf.Checkf("reads", func(o eOut, c wf.Ctx) error {
			r, err := wf.Observed(c, obsFor(o.ID))
			if err != nil {
				return err
			}
			if !r.OK {
				return errors.New("not ok: " + o.ID)
			}
			return nil
		}).ObservesFrom(func(o eOut) []wf.Reader { return []wf.Reader{obsFor(o.ID)} }))
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)

	rt := newFakeRuntime()
	rt.set("x", "y", map[string]any{"id": "a", "tag": "T"}, eRes{OK: false})
	rt.set("x", "y", map[string]any{"id": "b", "tag": "T"}, eRes{OK: false})
	h := newHarness(t, d, rt)
	run := h.run(d, map[string]string{"tag": "T"})

	in := h.next(run)[0]
	// Changed after issue: a definition-time observation would make this Stale.
	rt.set("x", "y", map[string]any{"id": "a", "tag": "T"}, eRes{OK: true})
	if res := h.submit(run, in, `{"ID":"b"}`); res.Class != wf.Refused || len(res.Refusal) != 1 || res.Refusal[0] != "reads: not ok: b" {
		t.Fatalf("b: %+v, want refused reading the submitted id", res)
	}
	var reads []map[string]any
	for _, r := range h.kind(run, wf.JournalObserved) {
		var od wf.ObservedData
		if err := json.Unmarshal(r.Data, &od); err != nil {
			t.Fatal(err)
		}
		if od.Purpose != "check" {
			t.Errorf("observed purpose = %q, want check", od.Purpose)
		}
		reads = append(reads, od.Desc.Args)
	}
	if len(reads) != 1 || reads[0]["id"] != "b" || reads[0]["tag"] != "T" {
		t.Fatalf("journaled check reads = %v, want one bound read of id b", reads)
	}

	in = h.next(run)[0]
	if res := h.submit(run, in, `{"ID":"a"}`); res.Class != wf.Accepted {
		t.Fatalf("a: %+v, want accepted", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eDone")

	t.Run("unavailable", func(t *testing.T) {
		rt := newFakeRuntime()
		rt.down["x.y"] = true
		h := newHarness(t, d, rt)
		run := h.run(d, map[string]string{"tag": "T"})
		res := h.submit(run, h.next(run)[0], `{"ID":"a"}`)
		if res.Class != wf.Unavailable {
			t.Fatalf("class = %s, want unavailable", res.Class)
		}
		raw := h.kind(run, wf.JournalObserved)[0].Data
		var od wf.ObservedData
		if err := json.Unmarshal(raw, &od); err != nil || !od.Unavailable {
			t.Fatalf("observed = %s, want an unavailable read", raw)
		}
	})
}

// reportingRuntime answers every apply with a fixed result and error.
type reportingRuntime struct {
	*scriptedRuntime
	res json.RawMessage
	err error
}

func (r reportingRuntime) Apply(string, wf.ExternalEffect) (json.RawMessage, error) {
	return r.res, r.err
}

// TestEffectResultPostDoneMutationJournaled: an Apply result carrying a
// post_done_mutation is journaled as one, with Unit filled by the engine, on a
// successful and on a failed apply, and the effect_result keeps the result on
// both; a result without one journals none.
func TestEffectResultPostDoneMutationJournaled(t *testing.T) {
	report := `{"Head":"m","post_done_mutation":{"TaskRef":"task-x","RefTarget":"late","Commit":"h"}}`
	for name, tc := range map[string]struct {
		res  string
		err  error
		want bool
	}{
		"applied":   {report, nil, true},
		"conflict":  {report, errors.New("conflict"), true},
		"unmoved":   {`{"Head":"m"}`, nil, false},
		"no object": {`"m"`, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := effectDef(nil, mergeEffect(wf.KeyOf("merge", "feat")))
			h := newHarness(t, d, reportingRuntime{newFakeRuntime(), json.RawMessage(tc.res), tc.err})
			run := h.run(d, nil)
			h.drive(run, map[string][]string{"eA": {`{}`}})
			var rd wf.EffectResultData
			if ers := h.kind(run, wf.JournalEffectResult); len(ers) != 1 || json.Unmarshal(ers[0].Data, &rd) != nil {
				t.Fatalf("effect_result records %d, want 1", len(ers))
			}
			if string(rd.Result) != tc.res {
				t.Errorf("effect_result Result = %s, want %s (kept on a failed apply too)", rd.Result, tc.res)
			}
			recs := h.kind(run, wf.JournalPostDoneMutation)
			if !tc.want {
				if len(recs) != 0 {
					t.Fatalf("journaled %d post_done_mutation records", len(recs))
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("journaled %d post_done_mutation records, want 1", len(recs))
			}
			var got wf.PostDoneMutationData
			if err := json.Unmarshal(recs[0].Data, &got); err != nil {
				t.Fatal(err)
			}
			want := wf.PostDoneMutationData{Unit: "eA", TaskRef: "task-x", RefTarget: "late", Commit: "h"}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}
