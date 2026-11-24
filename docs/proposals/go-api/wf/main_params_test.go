package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// paramsDef is an agent definition with run params: a run key, a file param,
// a required one and two with defaults.
func paramsDef(name string) *wf.Def {
	d := clAgentDef(name)
	d.RunKey(d.Param("slug"))
	d.Param("goal")
	d.Param("cmd").Alias("old-cmd")
	d.Param("tag")
	d.Param("fixed")
	return d
}

func paramsReg(name string) wf.Registration {
	return wf.Registration{
		Def: paramsDef(name),
		Params: []wf.ParamSpec{
			{Name: "goal", Required: true, Value: "<text>", File: true},
			{Name: "cmd", Required: true, Value: "<cmd>", Source: "never derived"},
			{Name: "slug", Value: "<slug>", Source: "the goal's first word",
				Derive: func(rc wf.RunContext) (string, error) { return strings.Fields(rc.Params["goal"])[0], nil }},
			{Name: "tag", Value: "<slug>-x", Source: "the slug",
				Derive: func(rc wf.RunContext) (string, error) { return rc.Params["slug"] + "-x", nil }},
			{Name: "fixed", Value: "dot", Source: "a fixed default"},
		},
		DefaultOptions: map[string]bool{"auto": true},
	}
}

// namedCfg is a named binary over two registrations, first the default.
func namedCfg() wf.MainConfig {
	return wf.MainConfig{Adapters: clConfig().Adapters, Binary: "super",
		Defs: []wf.Registration{paramsReg("first"), paramsReg("second")}}
}

func namedCLI(args ...string) (int, string, string) {
	var o, er bytes.Buffer
	code := wf.CLI(namedCfg(), args, &o, &er)
	return code, o.String(), er.String()
}

func runStarted(t *testing.T, stateDir string) wf.RunStartedData {
	t.Helper()
	runs, err := wf.FileJournal(stateDir).Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs in %s: %v %v", stateDir, runs, err)
	}
	j := wf.FileJournal(stateDir)
	unlock, err := j.Lock(runs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	recs, err := j.Records(runs[0])
	if err != nil || len(recs) == 0 || recs[0].Kind != wf.JournalRunStarted {
		t.Fatalf("records: %v", err)
	}
	var d wf.RunStartedData
	if err := json.Unmarshal(recs[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRunDerivesParams(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "idea.md"), []byte("alpha beta\nsecond line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		params []string
		want   map[string]string
		code   int
		errHas string
	}{
		{name: "derived", params: []string{"goal=hello world", "cmd=go test"},
			want: map[string]string{"goal": "hello world", "slug": "hello", "tag": "hello-x", "fixed": "dot", "cmd": "go test"}},
		{name: "override", params: []string{"goal=hello world", "cmd=c", "slug=mine", "fixed=f"},
			want: map[string]string{"slug": "mine", "tag": "mine-x", "fixed": "f"}},
		{name: "file", params: []string{"goal=@idea.md", "cmd=c"},
			want: map[string]string{"goal": "alpha beta\nsecond line\n", "slug": "alpha"}},
		{name: "alias counts as given", params: []string{"goal=g", "old-cmd=c"},
			want: map[string]string{"cmd": "c", "slug": "g"}},
		{name: "required refused", params: []string{"goal=g"}, code: 2, errHas: "requires --param cmd=<cmd>"},
		{name: "missing file", params: []string{"goal=@nope.md", "cmd=c"}, code: 2, errHas: "--param goal=@nope.md"},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			args := []string{"run", "--state-dir", state, "--workspace", ws}
			for _, p := range c.params {
				args = append(args, "--param", p)
			}
			code, _, er := namedCLI(args...)
			if code != c.code || !strings.Contains(er, c.errHas) {
				t.Fatalf("exit %d, stderr %q; want %d with %q", code, er, c.code, c.errHas)
			}
			if c.code != 0 {
				return
			}
			d := runStarted(t, state)
			for k, v := range c.want {
				if d.Params[k] != v {
					t.Errorf("param %s = %q, want %q", k, d.Params[k], v)
				}
			}
			if string(d.Options) != `{"auto":true}` {
				t.Errorf("options %s, want the registration's default", d.Options)
			}
		})
	}
}

func TestRunHelpListsParams(t *testing.T) {
	for _, args := range [][]string{{"run", "--help", "second"}, {"run", "second", "--help"}} {
		code, out, er := namedCLI(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er)
		}
		for _, want := range []string{"super run second", "goal", "<text> | @<path>", "cmd", "required: never derived",
			"tag", "the slug", "fixed", "dot", "a fixed default", `default: {"auto":true}`} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: help lacks %q:\n%s", args, want, out)
			}
		}
	}
}

// A named binary with several registrations: run starts the default or the
// named entry, and next and the wf verbs find the run from the state dir
// whichever entry started it.
func TestNamedBinaryServesSeveralEntries(t *testing.T) {
	ws := t.TempDir()
	for _, c := range []struct{ entry, def string }{{"", "first"}, {"first", "first"}, {"second", "second"}} {
		state := filepath.Join(t.TempDir(), "state")
		args := []string{"run"}
		if c.entry != "" {
			args = append(args, c.entry)
		}
		args = append(args, "--state-dir", state, "--workspace", ws, "--param", "goal=g h", "--param", "cmd=c")
		code, out, er := namedCLI(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er)
		}
		run := strings.TrimSpace(out)
		if !strings.HasPrefix(run, c.def+"-") {
			t.Errorf("%v started %q, want a run of %s", args, run, c.def)
		}
		if raw, err := os.ReadFile(filepath.Join(state, "run-id")); err != nil || strings.TrimSpace(string(raw)) != run {
			t.Errorf("run-id sidecar = %q (%v), want %s", raw, err, run)
		}
		code, out, er = namedCLI("next", "--state-dir", state)
		if code != 0 || !strings.Contains(out, "coordinator of workflow run "+run) {
			t.Errorf("next for %s: exit %d: %s%s", c.def, code, out, er)
		}
		code, out, er = namedCLI("wf", "status", "--json", "--state-dir", state)
		var st wf.RunStatus
		if code != 0 || json.Unmarshal([]byte(out), &st) != nil || st.Def != c.def || string(st.Run) != run {
			t.Errorf("wf status for %s: exit %d, def %q run %q: %s", c.def, code, st.Def, st.Run, er)
		}
		code, out, er = namedCLI("wf", "next", "all", "--state-dir", state, "--json")
		var ins []wf.Instruction
		if code != 0 || json.Unmarshal([]byte(out), &ins) != nil || len(ins) != 1 {
			t.Errorf("wf next for %s: exit %d: %s%s", c.def, code, out, er)
		}
	}
	if code, _, er := namedCLI("run", "third", "--state-dir", t.TempDir()); code != 2 || !strings.Contains(er, "unknown definition third") {
		t.Errorf("run of an unknown entry: exit %d: %s", code, er)
	}
	if code, _, er := namedCLI("lint"); code != 0 {
		t.Errorf("lint over two entries: exit %d: %s", code, er)
	}
	if _, _, er := namedCLI("bogus"); !strings.Contains(er, "[first|second]") {
		t.Errorf("usage does not name the entries: %s", er)
	}
}

// Item 92: an AtCreation param is derived only when the call creates a run (a
// fresh run or a successor). A resume attaches to the journaled params, so
// deriving it there would be wasted work (a network probe, say).
func TestAtCreationParamDerivedOnlyAtCreation(t *testing.T) {
	probes := 0
	d := clAgentDef("probed")
	d.RunKey(d.Param("slug"))
	d.Param("fact")
	reg := wf.Registration{Def: d, Params: []wf.ParamSpec{
		{Name: "slug", Value: "s"},
		{Name: "fact", Value: "<probed>", AtCreation: true,
			Derive: func(wf.RunContext) (string, error) { probes++; return "probed", nil }},
	}}
	cfg := wf.MainConfig{Adapters: clConfig().Adapters, Binary: "super", Defs: []wf.Registration{reg}}
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	run := func() {
		t.Helper()
		var o, er bytes.Buffer
		if code := wf.CLI(cfg, []string{"run", "--state-dir", state, "--workspace", ws}, &o, &er); code != 0 {
			t.Fatalf("run exit %d: %s", code, er.String())
		}
	}
	run()
	if probes != 1 {
		t.Fatalf("probes after creation = %d, want 1", probes)
	}
	if got := runStarted(t, state).Params["fact"]; got != "probed" {
		t.Fatalf("journaled fact %q, want probed", got)
	}
	run() // resume: the live run attaches
	if probes != 1 {
		t.Errorf("probes after resume = %d, want still 1", probes)
	}
}

func TestResolveRunParamsUsesCLIDerivationOrder(t *testing.T) {
	d := wf.New("resolve-public")
	reg := wf.Registration{Def: d, Params: []wf.ParamSpec{
		{Name: "base", Value: "default"},
		{Name: "derived", Derive: func(c wf.RunContext) (string, error) { return c.Params["base"] + "-derived", nil }},
		{Name: "created", AtCreation: true, Derive: func(c wf.RunContext) (string, error) { return c.Params["derived"] + "-created", nil }},
	}}
	params := map[string]string{"base": "explicit"}
	if err := reg.ResolveRunParams(params, wf.RunContext{}); err != nil {
		t.Fatal(err)
	}
	if params["derived"] != "explicit-derived" || params["created"] != "explicit-derived-created" {
		t.Fatalf("wrong CLI derivation: %v", params)
	}
}
