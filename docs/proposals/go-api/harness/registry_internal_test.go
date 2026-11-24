package harness

import (
	"slices"
	"testing"
)

func envMap(kv map[string]string) Env { return func(k string) string { return kv[k] } }

// The built-in detection order and marker order are what the hard-coded
// switch had: Hermes, then pi, then agy, then Codex, then Claude.
func TestRegistryKeepsBuiltinOrder(t *testing.T) {
	all := envMap(map[string]string{
		"HERMES_SESSION_ID": "h", "CODEX_THREAD_ID": "c", "CLAUDECODE": "1",
		"CLAUDE_CODE_ENTRYPOINT": "cli", "CLAUDE_CODE_SESSION_ID": "s",
	})
	if got := Detect(all); got != "hermes" {
		t.Fatalf("Detect(all) = %q, want hermes", got)
	}
	if got := Detect(envMap(map[string]string{"CODEX_THREAD_ID": "c", "CLAUDECODE": "1"})); got != "codex" {
		t.Fatalf("Detect(codex+claude) = %q, want codex", got)
	}
	if got := Detect(envMap(map[string]string{"CLAUDE_CODE_SESSION_ID": "s"})); got != "claude" {
		t.Fatalf("Detect(claude session only) = %q, want claude", got)
	}
	want := []string{"HERMES_SESSION_ID", "CODEX_THREAD_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID"}
	if got := Markers(all); !slices.Equal(got, want) {
		t.Fatalf("Markers(all) = %v, want %v", got, want)
	}
	if got, want := Names(), []string{"hermes", "pi", "agy", "codex", "claude"}; !slices.Equal(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	withAgy := envMap(map[string]string{"ANTIGRAVITY_CONVERSATION_ID": "a", "CODEX_THREAD_ID": "c", "CLAUDECODE": "1"})
	if got := Markers(withAgy); !slices.Equal(got, []string{"ANTIGRAVITY_CONVERSATION_ID", "CODEX_THREAD_ID", "CLAUDECODE"}) {
		t.Fatalf("Markers(agy+codex+claude) = %v", got)
	}
	if !otherAgent(all) || otherAgent(envMap(map[string]string{"HERMES_SESSION_ID": "h"})) {
		t.Fatal("otherAgent must ignore Hermes's own marker only")
	}
	if !Known("codex") || Known("generic") || Known("nope") {
		t.Fatal("Known must name registered adapters only")
	}
}

type fakeAdapter struct {
	Generic
	markers []string
}

func (f fakeAdapter) Markers() []string { return f.markers }

func (fakeAdapter) SameChild(owner, caller Caller) bool { return owner.Agent == caller.Agent }

// withFreshRegistry runs f against the built-in registry, not yet read, and
// restores the registry afterwards.
func withFreshRegistry(t *testing.T, f func()) {
	t.Helper()
	saved, read := slices.Clone(registry), registryRead.Load()
	t.Cleanup(func() { registry = saved; registryRead.Store(read) })
	registryRead.Store(false)
	f()
}

func TestRegisterAddsANewAdapterFirst(t *testing.T) {
	withFreshRegistry(t, func() {
		if err := Register(fakeAdapter{Generic{name: "fake"}, []string{"FAKE_SESSION"}}); err != nil {
			t.Fatal(err)
		}
		env := envMap(map[string]string{"FAKE_SESSION": "f", "CODEX_THREAD_ID": "c"})
		if got := Detect(env); got != "fake" {
			t.Fatalf("a registered adapter is detected first; Detect = %q", got)
		}
		if got := Markers(env); !slices.Equal(got, []string{"FAKE_SESSION", "CODEX_THREAD_ID"}) {
			t.Fatalf("Markers = %v", got)
		}
		if !Known("fake") || For("fake").Name() != "fake" {
			t.Fatal("For must return the registered adapter")
		}
	})
}

func TestRegisterRefusesAndKeepsTheRegistry(t *testing.T) {
	withFreshRegistry(t, func() {
		before := Names()
		registryRead.Store(false)
		for _, a := range []Adapter{
			fakeAdapter{Generic{name: "codex"}, []string{"OTHER"}},                             // a built-in's name
			fakeAdapter{Generic{name: "antigravity"}, []string{"ANTIGRAVITY_CONVERSATION_ID"}}, // agy's marker
			fakeAdapter{Generic{name: "agy"}, []string{"AGY_OTHER"}},                           // a built-in's name
			fakeAdapter{Generic{name: "generic"}, nil},                                         // reserved
			fakeAdapter{Generic{name: "scripted"}, nil},                                        // reserved
			fakeAdapter{Generic{name: "wrapper"}, []string{"CODEX_THREAD_ID"}},                 // a built-in's marker
			fakeAdapter{Generic{name: "codex-cli"}, []string{"ALIAS_A"}},                       // an alias of a built-in
			fakeAdapter{Generic{name: "Codex"}, []string{"ALIAS_B"}},                           // a case variant
			fakeAdapter{Generic{name: " fresh"}, []string{"ALIAS_C"}},                          // not trimmed
			fakeAdapter{Generic{name: "Generic"}, nil},                                         // a reserved name's variant
			fakeAdapter{Generic{name: "SCRIPTED"}, nil},                                        // a reserved name's variant
		} {
			if err := Register(a); err == nil {
				t.Errorf("Register(%q, %v) must be refused", a.Name(), a.(fakeAdapter).markers)
			}
		}
		if got := Names(); !slices.Equal(got, before) {
			t.Fatalf("a refused registration changed the registry: %v, was %v", got, before)
		}
		if Detect(envMap(map[string]string{"CODEX_THREAD_ID": "c"})) != "codex" {
			t.Fatal("codex detection must survive a refused registration")
		}
		if _, ok := For("codex").(ChildMatcher); !ok {
			t.Fatal("codex keeps its own same-child rule")
		}
		if err := Register(fakeAdapter{Generic{name: "late"}, []string{"LATE"}}); err == nil {
			t.Fatal("Register after the registry was read must be refused")
		}
		if Known("late") {
			t.Fatal("a late registration must not be added")
		}
	})
}

func TestSameChildRules(t *testing.T) {
	cases := []struct {
		harness       string
		owner, caller Caller
		want          bool
	}{
		{"hermes", Caller{Session: "s1"}, Caller{Session: "s1", Agent: "x"}, true},
		{"hermes", Caller{Session: "s1"}, Caller{Session: "s2"}, false},
		{"codex", Caller{Session: "s", Agent: "a"}, Caller{Session: "s", Agent: "a"}, true},
		{"codex", Caller{Session: "s", Agent: ""}, Caller{Session: "s", Agent: "b"}, false},
		{"claude", Caller{Session: "s"}, Caller{Session: "s", Agent: "b"}, true},
		{"claude", Caller{Session: "s", Agent: "a"}, Caller{Session: "s", Agent: "b"}, false},
		{"generic", Caller{Session: "s"}, Caller{Session: "t"}, false},
	}
	for _, c := range cases {
		c.owner.Harness, c.caller.Harness = c.harness, c.harness
		if got := SameChild(c.owner, c.caller); got != c.want {
			t.Errorf("%s SameChild(%+v, %+v) = %v, want %v", c.harness, c.owner, c.caller, got, c.want)
		}
	}
	same := Caller{Harness: "hermes", Session: "s", Root: "/r"}
	if SameChild(same, Caller{Harness: "codex", Session: "s", Root: "/r"}) {
		t.Error("SameChild must be false across harnesses")
	}
	if SameChild(same, Caller{Harness: "hermes", Session: "s", Root: "/other"}) {
		t.Error("SameChild must be false across roots")
	}
	if !SameChild(same, same) {
		t.Error("SameChild of the same caller")
	}
}

func TestCanonicalNames(t *testing.T) {
	for in, want := range map[string]string{
		"claude": "claude", " Claude-Code ": "claude", "claude_code": "claude", "ClaudeCode": "claude",
		"codex-cli": "codex", "CODEX_CLI": "codex", "Hermes": "hermes", "generic": "generic",
	} {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}
