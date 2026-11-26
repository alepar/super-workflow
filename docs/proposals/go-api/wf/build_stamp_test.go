package wf_test

import (
	"bytes"
	"encoding/json"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

const stampEngine = "github.com/alepar/super-workflow/docs/proposals/go-api"

func stampInfo(rev string) *debug.BuildInfo {
	return &debug.BuildInfo{GoVersion: "go1.26.0", Main: debug.Module{Path: "example.com/someflow", Version: "(devel)"},
		Deps:     []*debug.Module{{Path: stampEngine, Version: "v0.3.0"}},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "false"}}}
}

func stampedStart(t *testing.T, recs []wf.JournalRecord) wf.RunStartedData {
	t.Helper()
	var d wf.RunStartedData
	if len(recs) == 0 || recs[0].Kind != wf.JournalRunStarted || json.Unmarshal(recs[0].Data, &d) != nil {
		t.Fatalf("no run_started: %+v", recs)
	}
	return d
}

// run_started records the creating binary's stamp; status shows it, and
// notes when the binary reading the status is another one.
func TestRunStartedRecordsTheBuild(t *testing.T) {
	defer wf.SetBuildInfo(stampInfo("aaaa"))()
	d, _ := simple("stamped", wf.PromptText("go"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	b := stampedStart(t, h.records(run)).Build
	if b == nil || b.Workflow.Revision != "aaaa" || b.Workflow.Source != wf.StampVCS || b.Engine.Version != "v0.3.0" || b.Engine.Source != wf.StampModule || b.JournalFormat != wf.JournalFormat {
		t.Fatalf("stamp %+v", b)
	}
	view := wf.RenderStatus(h.status(run))
	if !strings.Contains(view, "build: engine "+stampEngine+" v0.3.0; workflow example.com/someflow (devel) revision aaaa (vcs); go1.26.0 ") || !strings.Contains(view, "; journal format 1; exact\n") {
		t.Fatalf("status:\n%s", view)
	}
	if strings.Contains(view, "this binary differs") {
		t.Fatalf("same binary noted as differing:\n%s", view)
	}
	defer wf.SetBuildInfo(stampInfo("bbbb"))()
	if view := wf.RenderStatus(h.status(run)); !strings.Contains(view, "this binary differs: workflow example.com/someflow (devel) revision bbbb (vcs)\n") {
		t.Fatalf("status by another binary:\n%s", view)
	}
}

// A journal from before stamps says so; an engine reading a stamped
// journal it does not know the field of ignores it (json decoding is not
// strict), which the old-journal path here mirrors.
func TestStatusOfARunWithoutAStamp(t *testing.T) {
	rs := wf.RunStatus{Run: "r", Def: "d", Phase: wf.RunRunning}
	if view := wf.RenderStatus(rs); !strings.Contains(view, "build: not recorded (the run predates build stamps)\n") {
		t.Fatalf("status:\n%s", view)
	}
	var old struct{ Def string }
	if err := json.Unmarshal([]byte(`{"Def":"d","Build":{"JournalFormat":1}}`), &old); err != nil || old.Def != "d" {
		t.Fatalf("lenient decode: %v", err)
	}
}

// The stamp enters no hash and no other record: two binaries running the
// same definition write the same journal but for run_started's Build.
func TestBuildStampEntersNoHash(t *testing.T) {
	journal := func(rev string) []wf.JournalRecord {
		defer wf.SetBuildInfo(stampInfo(rev))()
		d, _ := simple("same", wf.PromptText("go"))
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		in := h.next(run)[0]
		h.submit(run, in, `{"V":"ok"}`)
		h.next(run)
		return h.records(run)
	}
	a, b := journal("aaaa"), journal("bbbb")
	if len(a) != len(b) || len(a) < 3 {
		t.Fatalf("journals differ in length: %d %d", len(a), len(b))
	}
	for i := range a {
		ra, rb := a[i], b[i]
		if i == 0 {
			sa, sb := stampedStart(t, a), stampedStart(t, b)
			if sa.Build.Workflow.Revision == sb.Build.Workflow.Revision {
				t.Fatal("the stamps are the same")
			}
			sa.Build, sb.Build = nil, nil
			ja, _ := json.Marshal(sa)
			jb, _ := json.Marshal(sb)
			if !bytes.Equal(ja, jb) {
				t.Fatalf("run_started differs beyond Build:\n%s\n%s", ja, jb)
			}
			ra.Data, rb.Data = nil, nil
		}
		if ra.DefHash != rb.DefHash || ra.Kind != rb.Kind {
			t.Fatalf("record %d: %s %s, %s %s", i, ra.Kind, ra.DefHash, rb.Kind, rb.DefHash)
		}
		// Run ids and tokens are random per run; everything else, prompt
		// digests and effect keys included, must match byte for byte.
		ra.Run, rb.Run = "", ""
		ja, _ := json.Marshal(ra)
		jb, _ := json.Marshal(rb)
		random := regexp.MustCompile(`[0-9a-f]{16}`)
		ja = random.ReplaceAll(bytes.ReplaceAll(ja, []byte(a[0].Run), []byte("RUN")), []byte("X"))
		jb = random.ReplaceAll(bytes.ReplaceAll(jb, []byte(b[0].Run), []byte("RUN")), []byte("X"))
		if !bytes.Equal(ja, jb) {
			t.Fatalf("record %d differs:\n%s\n%s", i, ja, jb)
		}
	}
}

// version prints this binary's stamp, as text or JSON.
func TestVersionVerb(t *testing.T) {
	defer wf.SetBuildInfo(stampInfo("aaaa"))()
	var out, er bytes.Buffer
	if code := wf.CLI(wf.MainConfig{}, []string{"version"}, &out, &er); code != 0 || er.Len() != 0 {
		t.Fatalf("code %d stderr %s", code, er.String())
	}
	for _, want := range []string{"engine   " + stampEngine + " v0.3.0\n", "workflow example.com/someflow (devel) revision aaaa (vcs)\n", "go       go1.26.0 ", "journal  format 1\n", "source   exact\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	out.Reset()
	var b wf.BuildStamp
	if code := wf.CLI(wf.MainConfig{}, []string{"version", "--json"}, &out, &er); code != 0 || json.Unmarshal(out.Bytes(), &b) != nil || b.Workflow.Revision != "aaaa" {
		t.Fatalf("code %d json %s", code, out.String())
	}
	if code := wf.CLI(wf.MainConfig{}, []string{"version", "extra"}, &out, &er); code != 2 {
		t.Fatalf("extra argument: code %d", code)
	}
}
