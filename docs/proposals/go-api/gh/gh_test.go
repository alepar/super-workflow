package gh_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/gh"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestGh(t *testing.T) {
	for _, c := range []struct {
		d      wf.ObservationDesc
		op, rt string
	}{
		{gh.PluginJSON("o/r", "main").Desc(), "plugin_json", "gh.Plugin"},
		{gh.IssueView("o/r", 1).Desc(), "issue_view", "gh.Issue"},
	} {
		if c.d.Adapter != "gh" || c.d.Op != c.op || c.d.Result != c.rt {
			t.Errorf("%s = %+v", c.op, c.d)
		}
	}
	e := gh.IssueCreate(wf.KeyOf("k"), "o/r", "t", "b")
	if e.Adapter != "gh" || e.Op != "issue_create" || e.Key == nil {
		t.Errorf("issue_create = %+v", e)
	}
}
