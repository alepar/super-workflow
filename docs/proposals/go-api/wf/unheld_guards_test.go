package wf_test

import (
	"strings"
	"testing"
)

func TestUnheldGuardsEvaluatesEachGuardAgainstTheOptions(t *testing.T) {
	d := guardRouteDef()
	cases := []struct {
		name    string
		options []any
		want    string
	}{
		{"none", nil, "guard-route/wPick -> wAlt when Skip=true\nguard-route/wPick -> wFin when not Skip=true"},
		{"skip only", []any{wOpts{Skip: true}}, "guard-route/wPick -> wFin when not Skip=true"},
		{"both sides", []any{&wOpts{Skip: true}, wOpts{}}, ""},
	}
	for _, c := range cases {
		got, err := d.UnheldGuards(c.options...)
		if err != nil || strings.Join(got, "\n") != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	// A route-around guard on a Sub counts as well.
	if got, err := guardSubDef().UnheldGuards(wOpts{}); err != nil || len(got) != 1 {
		t.Errorf("sub guard: got %q, %v", got, err)
	}
	if _, err := d.UnheldGuards("not an options struct"); err == nil {
		t.Error("an options value of the wrong type decoded")
	}
}
