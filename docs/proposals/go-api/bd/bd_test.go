package bd_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestCreateEffect(t *testing.T) {
	e := bd.Create(bd.IssueKey("sp:x", "t"), bd.Issue{Title: "t", Parent: "e", NoInheritLabels: true, RootLabel: "sp:e"})
	if e.Adapter != "bd" || e.Op != "create" || e.Key == nil {
		t.Fatalf("create = %+v", e)
	}
	if got := e.Args["issue"].(bd.Issue); got.Parent != "e" || !got.NoInheritLabels || got.RootLabel != "sp:e" {
		t.Fatalf("I3 flag triple lost: %+v", got)
	}
}

func TestEffects(t *testing.T) {
	k := wf.KeyOf("k")
	for op, e := range map[string]wf.ExternalEffect{
		"update_status": bd.UpdateStatus(k, "b-1", bd.Closed),
		"dep_add":       bd.DepAdd(k, "b-2", "b-1", "blocked-by b-1: consumes X"),
		"close":         bd.Close(k, "b-1", "done"),
	} {
		if e.Adapter != "bd" || e.Op != op || e.Key == nil {
			t.Errorf("%s = %+v", op, e)
		}
	}
	if got := bd.IssueKey("sp:x", "t"); got != wf.KeyOf("bd", "sp:x", "t") {
		t.Errorf("IssueKey = %v", got)
	}
}

func TestObservations(t *testing.T) {
	for _, c := range []struct {
		d      wf.ObservationDesc
		op, rt string
	}{
		{bd.Ready("e").Desc(), "ready", "[]bd.Issue"},
		{bd.ReadyIn("l").Desc(), "ready", "[]bd.Issue"},
		{bd.Show("b-1").Desc(), "show", "bd.Issue"},
		{bd.ListByLabel("sp:e").Desc(), "list", "[]bd.Issue"},
		{bd.CloseEligible("e").Desc(), "epic_close_eligible", "[]string"},
	} {
		if c.d.Adapter != "bd" || c.d.Op != c.op || c.d.Result != c.rt {
			t.Errorf("%s = %+v", c.op, c.d)
		}
	}
	if !strings.Contains(bd.Ready("e").Desc().Result, "Issue") {
		t.Error("Ready result does not name Issue")
	}
}

func TestSupersedeEffect(t *testing.T) {
	e := bd.Supersede(wf.KeyOf("replace", "old"), "old", "new")
	if e.Adapter != "bd" || e.Op != "supersede" || e.KeyID() != "replace/old" || e.Args["old"] != "old" || e.Args["new"] != "new" {
		t.Fatalf("supersede = %+v", e)
	}
}
