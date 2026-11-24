package wf_test

import (
	"encoding/json"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type aliasTerm struct{}

func aliasDef() *wf.Def {
	d := wf.New("alias")
	p := d.Param("new").Alias("old", "older")
	d.Param("repo")
	d.RunKey(p)
	wf.NewTerminal[aliasTerm](d, wf.Success)
	return d
}

func startedParams(t *testing.T, h *harness, id wf.RunID) map[string]string {
	t.Helper()
	recs := h.records(id)
	var data wf.RunStartedData
	if err := json.Unmarshal(recs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	return data.Params
}

func TestParamAliasMapsOldName(t *testing.T) {
	d := aliasDef()
	h := newHarness(t, d, nil)
	viaOld := h.run(d, map[string]string{"old": "v", "repo": "/r"})
	got := startedParams(t, h, viaOld)
	if got["new"] != "v" || got["repo"] != "/r" {
		t.Errorf("journaled params %v, want new=v and repo kept", got)
	}
	if _, ok := got["old"]; ok {
		t.Errorf("journaled params %v keep the old name", got)
	}

	h2 := newHarness(t, d, nil)
	if viaNew := h2.run(d, map[string]string{"new": "v", "repo": "/r"}); viaNew != viaOld {
		t.Errorf("run id via the new name %s, via the old name %s: want equal", viaNew, viaOld)
	}
	h3 := newHarness(t, d, nil)
	if other := h3.run(d, map[string]string{"older": "w", "repo": "/r"}); other == viaOld {
		t.Errorf("a different value gave the same run id %s", other)
	}
}

func TestParamAliasNewNameWins(t *testing.T) {
	d := aliasDef()
	h := newHarness(t, d, nil)
	id := h.run(d, map[string]string{"old": "stale", "new": "fresh"})
	got := startedParams(t, h, id)
	if got["new"] != "fresh" || len(got) != 1 {
		t.Errorf("journaled params %v, want only new=fresh", got)
	}
}
