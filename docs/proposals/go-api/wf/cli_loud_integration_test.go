package wf_test

import (
	"bytes"
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLITopLevelHelpNeedsNoRunAndNamesBinary(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		var out, er bytes.Buffer
		cfg := wf.MainConfig{Binary: "review-tool", Defs: []wf.Registration{{Def: clAgentDef("review")}}}
		if code := wf.CLI(cfg, []string{arg}, &out, &er); code != 0 || !strings.Contains(out.String(), "usage: review-tool ") || er.Len() != 0 {
			t.Fatalf("%s: code=%d out=%q err=%q", arg, code, out.String(), er.String())
		}
	}
	var out, er bytes.Buffer
	if code := wf.CLI(wf.MainConfig{}, []string{"--help"}, &out, &er); code != 0 || !strings.Contains(out.String(), filepath.Base(os.Args[0])) {
		t.Fatalf("generic help: code=%d out=%q err=%q", code, out.String(), er.String())
	}
}

func TestCLIExplainAndJournalNamesAreActuallyWired(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := e.next(run)[0]
	code, out, er := e.cli("explain", string(run), in.Name, "--state-dir", e.state)
	if code != 0 || !strings.Contains(out, in.Name) {
		t.Fatalf("explain code=%d out=%q err=%q", code, out, er)
	}
	for _, line := range strings.Split(strings.TrimSpace(e.must("journal", string(run), "--state-dir", e.state)), "\n") {
		var view map[string]any
		if err := json.Unmarshal([]byte(line), &view); err != nil {
			t.Fatal(err)
		}
		if _, ok := view["KindName"].(string); !ok {
			t.Fatalf("journal kind has no readable name: %s", line)
		}
	}
}

func TestCLIEndedExplicitIDHasNoUnusableResumeLine(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := e.next(run)[0]
	e.submit(in, `{"OK":true}`)
	if st := e.status(run); st.Phase != wf.RunEnded || st.Resume != "" {
		t.Fatalf("ended unkeyed run advertises resume: %+v", st)
	}
	code, out, er := e.cli("next", string(run), "all", "--state-dir", e.state, "--json")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(out), &ins); err != nil {
		t.Fatal(err)
	}
	if code != exitEndedCode || len(ins) != 1 || ins[0].Resume != "" {
		t.Fatalf("ended instruction advertises resume: code=%d out=%q err=%q", code, out, er)
	}
}
