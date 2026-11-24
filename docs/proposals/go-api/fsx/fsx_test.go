package fsx_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type cfg struct{ N int }

func TestFsx(t *testing.T) {
	for _, c := range []struct {
		d      wf.ObservationDesc
		op, rt string
	}{
		{fsx.Exists("p").Desc(), "exists", "bool"},
		{fsx.Hash("p").Desc(), "hash", "string"},
		{fsx.ReadJSON[cfg]("p", "/a/0").Desc(), "read_json", "fsx_test.cfg"},
		{fsx.SchemaCheck("p", fsx.Schema{Name: "s", Path: "s.json"}).Desc(), "schema_check", "fsx.SchemaResult"},
		{fsx.Glob("d", "*.json").Desc(), "glob", "[]fsx.File"},
	} {
		if c.d.Adapter != "fsx" || c.d.Op != c.op || c.d.Result != c.rt {
			t.Errorf("%s = %+v", c.op, c.d)
		}
	}
	if got := fsx.ReadJSON[cfg]("p", "/a/0").Desc().Args["pointer"]; got != "/a/0" {
		t.Errorf("pointer = %v", got)
	}
}
