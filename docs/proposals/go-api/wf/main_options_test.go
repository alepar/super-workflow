package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type overlayOpts struct {
	Autonomous bool
	Mode       string
}

// overlayCfg registers keyedAgentDef with two options fields and, when
// defaults is non-nil, those registration defaults.
func overlayCfg(name string, defaults any) (wf.MainConfig, *wf.Def) {
	d := keyedAgentDef(name)
	wf.Options[overlayOpts](d)
	return wf.MainConfig{Adapters: clConfig().Adapters, Binary: "super",
		Defs: []wf.Registration{{Def: d, DefaultOptions: defaults}}}, d
}

func optionsFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "o.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func statusOptions(t *testing.T, cfg wf.MainConfig, state string) (overlayOpts, wf.RunStatus) {
	t.Helper()
	code, out, er := cliCall(cfg, "wf", "status", "--json", "--state-dir", state)
	if code != 0 {
		t.Fatalf("status: %d %s", code, er)
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	var o overlayOpts
	if err := json.Unmarshal(st.Options, &o); err != nil {
		t.Fatalf("status options %s: %v", st.Options, err)
	}
	return o, st
}

// sw-a5s6: --options is decoded over the registration's default options,
// so a field it leaves out keeps the default and one it gives, false
// included, wins. The start line prints the effective options.
func TestOptionsOverlayTheRegistrationDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       overlayOpts
	}{
		{"partial file keeps the defaults", `{"Mode":"fast"}`, overlayOpts{Autonomous: true, Mode: "fast"}},
		{"explicit false overrides a true default", `{"Autonomous":false}`, overlayOpts{Autonomous: false, Mode: "default"}},
		{"empty object is the defaults", `{}`, overlayOpts{Autonomous: true, Mode: "default"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := overlayCfg("overlay-defaults", overlayOpts{Autonomous: true, Mode: "default"})
			state := filepath.Join(t.TempDir(), "state")
			code, _, er := cliCall(cfg, "run", "--state-dir", state, "--workspace", t.TempDir(), "--param", "topic=x", "--options", optionsFile(t, tc.body))
			if code != 0 {
				t.Fatalf("run: %d %s", code, er)
			}
			got, _ := statusOptions(t, cfg, state)
			if got != tc.want {
				t.Errorf("options %+v, want %+v", got, tc.want)
			}
			raw, _ := json.Marshal(tc.want)
			if !strings.Contains(er, "options: "+string(raw)) {
				t.Errorf("the start line does not print the effective options %s:\n%s", raw, er)
			}
		})
	}
}

// A definition with no DefaultOptions decodes --options over the zero
// value, as before.
func TestOptionsWithoutDefaultsDecodeOverZero(t *testing.T) {
	cfg, _ := overlayCfg("overlay-zero", nil)
	state := filepath.Join(t.TempDir(), "state")
	if code, _, er := cliCall(cfg, "run", "--state-dir", state, "--workspace", t.TempDir(), "--param", "topic=x", "--options", optionsFile(t, `{"Mode":"fast"}`)); code != 0 {
		t.Fatalf("run: %d %s", code, er)
	}
	if got, _ := statusOptions(t, cfg, state); got != (overlayOpts{Mode: "fast"}) {
		t.Errorf("options %+v, want only Mode fast", got)
	}
	if code, _, er := cliCall(cfg, "run", "--state-dir", filepath.Join(t.TempDir(), "s2"), "--workspace", t.TempDir(), "--param", "topic=x", "--options", optionsFile(t, `{"Mood":"fast"}`)); code != 2 || !strings.Contains(er, "unknown field") {
		t.Errorf("an undeclared field = %d %q, want refused", code, er)
	}
}

// A successor decodes --options over its predecessor's options, not the
// registration's defaults; a resume decodes them over the journaled ones,
// so a partial file that agrees with them attaches and one that differs is
// refused.
func TestOptionsOverlayThePredecessorAndJournal(t *testing.T) {
	cfg, def := overlayCfg("overlay-chain", overlayOpts{Autonomous: true, Mode: "default"})
	state := filepath.Join(t.TempDir(), "state")
	ws := t.TempDir()
	run := func(body string) (int, string) {
		code, _, er := cliCall(cfg, "run", "--state-dir", state, "--workspace", ws, "--param", "topic=x", "--options", optionsFile(t, body))
		return code, er
	}
	if code, er := run(`{"Autonomous":false,"Mode":"interactive"}`); code != 0 {
		t.Fatalf("first run: %d %s", code, er)
	}
	if code, er := run(`{"Mode":"interactive"}`); code != 0 {
		t.Fatalf("a partial file agreeing with the journal must attach: %d %s", code, er)
	}
	if code, er := run(`{"Autonomous":true}`); code != 6 || !strings.Contains(er, "different options") {
		t.Fatalf("a partial file differing from the journal = %d %q, want exit 6", code, er)
	}
	finishOne(t, def, state)
	if code, er := run(`{"Mode":"fast"}`); code != 0 {
		t.Fatalf("successor: %d %s", code, er)
	}
	got, st := statusOptions(t, cfg, state)
	if st.Predecessor == "" {
		t.Fatalf("no successor started: %+v", st)
	}
	if want := (overlayOpts{Autonomous: false, Mode: "fast"}); got != want {
		t.Errorf("successor options %+v, want %+v (the predecessor's Autonomous kept)", got, want)
	}
}
