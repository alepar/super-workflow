package wf

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// The engine the CLI builds, with no ChildLive injected, reaches the pi
// adapter's ChildLive from loseEndedChildren: a transcript that ended on a
// final error is offered for recovery only once no process of the child is
// live. The harness/pi adapter's marker is the only thing that tells.
func TestCLIEngineSettlesAnEndedChildThroughTheAdaptersChildLive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	raw, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "ended", "pi-final-error.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "wf-kid.jsonl")
	if err := os.WriteFile(transcript, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	now := mt.Add(time.Second)
	d := foldDef()
	c := &cli{stdout: io.Discard, stderr: io.Discard, env: func(string) string { return "" }}
	x, err := c.engine(t.TempDir(), EngineOptions{
		Defs:            []*Def{d},
		Clock:           func() time.Time { return now },
		TranscriptMtime: func(string) (time.Time, error) { return mt, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	e := x.(*Engine)
	if e.opts.ChildLive == nil {
		t.Fatal("the CLI-built engine has no ChildLive")
	}
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	co := Coordinator{Harness: "pi", Session: "coord"}
	ins, err := e.HandOutAs(run, 1, co)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOutAs = %+v, %v", ins, err)
	}
	agent := "wf-clisettle" + strconv.Itoa(os.Getpid())
	if err := e.Started(run, ins[0].Name, Claim{Harness: "pi", Session: "coord", Agent: agent, Transcript: transcript}); err != nil {
		t.Fatal(err)
	}
	recovery := func() bool {
		t.Helper()
		got, err := e.HandOutAs(run, 1, co)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range got {
			if in.Kind == InstructionRecovery {
				return true
			}
		}
		return false
	}

	cmd := startFakePi(t, "SUPER_PI_CHILD="+agent, agent)

	if recovery() {
		t.Fatal("offered before the settle")
	}
	now = mt.Add(endSettle)
	if recovery() {
		t.Fatal("offered while a process of the child is live")
	}
	cmd.Process.Kill() // the child's process; its descendant keeps the marker
	cmd.Wait()
	now = mt.Add(2 * endSettle)
	if !recovery() {
		t.Fatal("not offered once the child's process is gone (a descendant holding the marker does not hold it live)")
	}
}
