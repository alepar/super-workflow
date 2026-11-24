package wf

import (
	"bytes"
	"strings"
	"testing"
)

func TestTopHelpUsesConfiguredBinaryWithoutDefinitions(t *testing.T) {
	var out, err bytes.Buffer
	c := &cli{cfg: MainConfig{Binary: "custom-flow"}, stdout: &out, stderr: &err}
	if code := c.topHelp(); code != 0 || !strings.Contains(out.String(), "usage: custom-flow ") || err.Len() != 0 {
		t.Fatalf("help exit=%d out=%q err=%q", code, out.String(), err.String())
	}
}

func TestEndedResumeRequiresCompleteDeclaredRunKey(t *testing.T) {
	d := New("workflow")
	p := d.Param("topic")
	c := &cli{cfg: MainConfig{Defs: []Registration{{Def: d}}}}
	started := RunStartedData{Def: "workflow", Params: map[string]string{"topic": "example"}}
	if c.canSucceed(started) {
		t.Fatal("unkeyed ended run advertised a successor")
	}
	d.RunKey(p)
	if !c.canSucceed(started) {
		t.Fatal("complete keyed run cannot advertise its successor")
	}
	delete(started.Params, "topic")
	if c.canSucceed(started) {
		t.Fatal("missing run-key parameter advertised a runnable successor")
	}
}
