package wf

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCoordinatorStopUsesPublicMainEvent(t *testing.T) {
	for _, tc := range []struct {
		name, harness, input, session, turn string
		active, invalid                     bool
	}{
		{"codex", "codex", `{"session_id":"parent","turn_id":"turn","hook_event_name":"Stop","stop_hook_active":true,"cwd":"/somewhere","last_assistant_message":"ignore"}`, "parent", "turn", true, false},
		{"claude", "claude", `{"session_id":"parent","hook_event_name":"Stop","stop_hook_active":false,"cwd":"/somewhere","background_tasks":[]}`, "parent", "", true, false},
		{"codex child carries parent session", "codex", `{"session_id":"parent","turn_id":"turn","hook_event_name":"SubagentStop","agent_id":"child"}`, "", "", false, false},
		{"claude child fields on Stop", "claude", `{"session_id":"parent","hook_event_name":"Stop","agent_id":"child"}`, "", "", false, false},
		{"interrupt", "codex", `{"session_id":"parent","hook_event_name":"Interrupt"}`, "", "", false, false},
		{"no native session", "codex", `{"hook_event_name":"Stop"}`, "", "", false, true},
		{"trailing payload", "claude", `{"session_id":"parent","hook_event_name":"Stop"} {}`, "", "", false, true},
		{"ambiguous event", "codex", `{"session_id":"parent","hook_event_name":"SubagentStop","hook_event_name":"Stop"}`, "", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, turn, active, err := parseCoordinatorStop(tc.harness, strings.NewReader(tc.input))
			if session != tc.session || turn != tc.turn || active != tc.active || (err != nil) != tc.invalid {
				t.Fatalf("Stop payload returned (%q, %q, %t, %v)", session, turn, active, err)
			}
		})
	}
}

func TestRenderCoordinatorStopNeverVetoesOtherHooks(t *testing.T) {
	for _, tc := range []struct {
		command, diagnostic, want string
	}{
		{"", "", ""},
		{"", "workflow paused", `{"systemMessage":"workflow paused"}`},
		{"cd '/work tree' && /opt/client wf next", "", `{"decision":"block","reason":"cd '/work tree' && /opt/client wf next"}`},
	} {
		got, err := renderCoordinatorStop(tc.command, tc.diagnostic)
		if err != nil {
			t.Fatal(err)
		}
		if string(bytes.TrimSpace(got)) != tc.want {
			t.Errorf("native output = %s, want %s", got, tc.want)
		}
	}
}

func TestInstallCoordinatorStopPreservesExistingHooks(t *testing.T) {
	for _, tc := range []struct {
		harness, config string
		named           bool
	}{
		{"codex", ".codex/hooks.json", true},
		{"claude", ".claude/settings.local.json", false},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "work ' tree $USER ${CLAUDE_PROJECT_DIR}")
			path := filepath.Join(workspace, tc.config)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(t.TempDir(), "approved ' client")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			const existing = `{"permissions":{"allow":["Read"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"old-stop"}]}],"SessionStart":[{"hooks":[{"type":"command","command":"old-start"}]}]}}`
			if err := os.WriteFile(path, []byte(existing), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := installCoordinatorStop(tc.harness, workspace, executable, tc.named); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Permissions map[string][]string
				Hooks       map[string][]struct {
					Hooks []struct {
						Type, Command string
						Async         bool
						Timeout       int
					}
				}
			}
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			stop := config.Hooks["Stop"]
			start := config.Hooks["SessionStart"]
			if len(stop) != 2 || len(start) != 1 || stop[0].Hooks[0].Command != "old-stop" || start[0].Hooks[0].Command != "old-start" || strings.Join(config.Permissions["allow"], ",") != "Read" {
				t.Fatalf("existing settings changed or hook duplicated: %s", data)
			}
			hook := stop[1].Hooks[0]
			if hook.Async || hook.Type != "command" || hook.Timeout != 2 {
				t.Fatalf("expected a synchronous command with a two-second deadline: %+v", hook)
			}
			command := hook.Command
			if tc.harness == "claude" {
				command = strings.ReplaceAll(command, "${CLAUDE_PROJECT_DIR}", "/changed")
			}
			out, err := exec.Command("sh", "-c", command).CombinedOutput()
			want := "coordinator-stop\n--hook\n" + tc.harness + "\n--workspace\n" + workspace + "\n"
			if tc.named {
				want = "wf\n" + want
			}
			if err != nil || string(out) != want {
				t.Fatalf("installed command returned (%q, %v), want %q", out, err, want)
			}
			if err := installCoordinatorStop(tc.harness, workspace, executable, tc.named); err != nil {
				t.Fatal(err)
			}
			again, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(again, data) {
				t.Fatalf("install changed existing registration: %v\n%s", err, again)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("existing config file permissions changed: %v %v", info, err)
			}
		})
	}
}

func TestInstallCoordinatorStopRefusesAmbiguousConfig(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"duplicate key", `{"hooks":{},"hooks":{}}`},
		{"malformed Stop", `{"hooks":{"Stop":{}}}`},
		{"different workflow command", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/old/client wf coordinator-stop --hook codex --workspace /old"}]}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, ".codex", "hooks.json")
			if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := installCoordinatorStop("codex", workspace, executable, true); err == nil {
				t.Fatal("installed hook in ambiguous configuration")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.content {
				t.Fatalf("rejected configuration changed: %v\n%s", err, data)
			}
		})
	}
}

func TestInstallCoordinatorStopRequiresExecutableOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	client := filepath.Join(workspace, "client")
	if err := os.WriteFile(client, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "client")
	if err := os.Symlink(client, alias); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{client, alias, "client"} {
		if err := installCoordinatorStop("codex", workspace, executable, true); err == nil {
			t.Errorf("installed hook with unpinned executable %q", executable)
		}
	}
}

func TestInstallCoordinatorStopRefusesConfigSymlink(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	path := filepath.Join(workspace, ".codex")
	if err := os.Symlink(external, path); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := installCoordinatorStop("codex", workspace, executable, true); err == nil {
		t.Fatal("installed hook through a symlink to external settings")
	}
	if _, err := os.Stat(filepath.Join(external, "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("external settings were written: %v", err)
	}
}

func TestInstallCoordinatorStopRefusesTwoExistingRegistrations(t *testing.T) {
	workspace := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := installCoordinatorStop("codex", workspace, executable, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, ".codex", "hooks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	stop := config.Hooks["Stop"]
	if len(stop) != 1 {
		t.Fatalf("expected one registration, got %s", data)
	}
	config.Hooks["Stop"] = append(stop, stop[0])
	duplicated, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, duplicated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installCoordinatorStop("codex", workspace, executable, false); err == nil {
		t.Fatal("reinstall accepted ambiguous duplicate registrations")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, duplicated) {
		t.Fatalf("refused config changed: %v\n%s", err, after)
	}
}

func TestCoordinatorHookCLIFlags(t *testing.T) {
	workspace := t.TempDir()
	var out, stderr bytes.Buffer
	env := func(string) string { return "" }
	for _, verb := range []string{coordinatorStopVerb, "install-coordinator-stop"} {
		for _, args := range [][]string{
			{verb, "--hook", "generic", "--workspace", workspace},
			{verb, "--hook", "claude", "--workspace", "relative"},
		} {
			stderr.Reset()
			if code := CLIWithEnv(MainConfig{}, args, &out, &stderr, env); code != exitUsage ||
				!strings.Contains(stderr.String(), "absolute --workspace") {
				t.Fatalf("%v exited %d: %s", args, code, stderr.String())
			}
		}
	}
	stderr.Reset()
	args := []string{"install-coordinator-stop", "--hook", "claude", "--workspace", workspace}
	if code := CLIWithEnv(MainConfig{}, args, &out, &stderr, env); code != exitOK {
		t.Fatalf("%v exited %d: %s", args, code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, ".claude", "settings.local.json")); err != nil {
		t.Fatal(err)
	}
}

func TestNativeTurnIgnoresUnrelatedEventsAndMissingBinding(t *testing.T) {
	state := t.TempDir()
	c := &cli{env: func(string) string { return state }}
	workspace := t.TempDir()
	for _, payload := range []string{
		`{"hook_event_name":"SubagentStop","session_id":"main","agent_id":"child"}`,
		`{"hook_event_name":"Interrupt","session_id":"main"}`,
		`{"hook_event_name":"Stop","session_id":"main"}`,
	} {
		continuation, message := c.nativeTurnResult("codex", workspace, strings.NewReader(payload))
		if continuation != "" || message != "" {
			t.Fatalf("%s returned (%q, %q), want a neutral response", payload, continuation, message)
		}
	}
}
