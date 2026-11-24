package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectCoordinatorCustomExecutableAndChildVerbs(t *testing.T) {
	for _, verb := range []string{"prompt", "started", "validate", "done", "failed"} {
		args, _ := json.Marshal(map[string]any{"args": []string{"bash", "-lc", "cd /workspace && /tools/client --state-dir /state " + verb + " unit"}})
		row, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "arguments": string(args)}})
		path := filepath.Join(t.TempDir(), "transcript")
		if err := os.WriteFile(path, row, 0600); err != nil {
			t.Fatal(err)
		}
		got := InspectCoordinator(path, "codex", "/tools/client")
		if !got.Known || got.Calls != 1 || got.ChildVerbs[verb] != 1 {
			t.Fatalf("%s: %+v", verb, got)
		}
	}
}

func TestInspectCoordinator(t *testing.T) {
	for _, tc := range []struct {
		name, harness, body string
		known               bool
		calls               int
		verb                string
	}{
		{"codex", "codex", `{"type":"session_meta","payload":{"id":"session"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"one","name":"exec_command","arguments":"{\"cmd\":\"wf prompt --claim token\"}"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"one","name":"exec_command","arguments":"{\"cmd\":\"wf prompt --claim token\"}"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"wf done"}]}}`, true, 1, "prompt"},
		{"claude", "claude", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"one","name":"Bash","input":{"command":"./super validate unit"}},{"type":"text","text":"super done"}]}}`, true, 1, "validate"},
		{"echo", "claude", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"one","name":"Bash","input":{"command":"echo 'wf failed'"}}]}}`, true, 1, ""},
		{"quoted js", "codex", `{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"text('tools.exec_command({cmd: wf done})')"}}`, false, 1, ""},
		{"dynamic", "claude", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"one","name":"Bash","input":{"command":"$BIN done"}}]}}`, false, 1, ""},
		{"zero", "codex", `{"type":"session_meta","payload":{"id":"session"}}`, true, 0, ""},
		{"empty", "codex", "", false, 0, ""},
		{"wrong schema", "claude", `{"other":"hello"}`, false, 0, ""},
		{"truncated", "codex", "{\"type\":", false, 0, ""},
		{"hermes unsupported", "hermes", "2026 tool | -> terminal(wf done)", false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transcript")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			got := InspectCoordinator(path, tc.harness)
			if got.Known != tc.known || got.Calls != tc.calls {
				t.Fatalf("got %+v", got)
			}
			if tc.verb != "" && got.ChildVerbs[tc.verb] != 1 {
				t.Fatalf("missing verb: %+v", got)
			}
			if tc.verb == "" && len(got.ChildVerbs) != 0 {
				t.Fatalf("invented verb: %+v", got)
			}
		})
	}
	if got := InspectCoordinator(filepath.Join(t.TempDir(), "missing"), "codex"); got.Known {
		t.Fatal("missing transcript known")
	}
}

func TestInspectCoordinatorRecordedFixtures(t *testing.T) {
	for _, name := range []string{"codex-v2-child-1.jsonl", "codex-terra-child-1.jsonl"} {
		got := InspectCoordinator(filepath.Join("testdata", "started", name), "codex")
		if !got.Known || got.Calls != 1 || got.ChildVerbs["prompt"] != 1 {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	got := InspectCoordinator(filepath.Join("testdata", "read", "claude-bash-cat.jsonl"), "claude")
	if !got.Known || got.Calls != 1 || len(got.ChildVerbs) != 0 {
		t.Fatalf("Claude: %+v", got)
	}
}
