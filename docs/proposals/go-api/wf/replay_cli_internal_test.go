package wf

import (
	"flag"
	"io"
	"testing"
)

// wf dev replay takes --observe and --same-key (converge E5).
func TestDevReplayFlags(t *testing.T) {
	row, ok := findDevRow("replay")
	if !ok || row.define == nil {
		t.Fatal("no dev replay row with flags")
	}
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f flags
	row.define(fs, &f)
	if err := fs.Parse([]string{"--observe", "fixture.json", "--same-key"}); err != nil {
		t.Fatal(err)
	}
	if f.observe != "fixture.json" || !f.sameKey {
		t.Fatalf("flags %+v: want observe and same-key set", f)
	}
}
