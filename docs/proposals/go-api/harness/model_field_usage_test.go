package harness

import (
	"path/filepath"
	"testing"
)

// The adapters no longer read which model a child ran on, so the shape of a
// record's model fields must not decide whether its token usage counts: an
// object where a string was expected still leaves the usage read.
func TestUsageIgnoresTheShapeOfModelFields(t *testing.T) {
	for _, c := range []struct {
		name, adapter, line string
		want                int
	}{
		{"agy model string", "agy", `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","model":"m","input_tokens":3,"output_tokens":4}`, 7},
		{"agy model object", "agy", `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","model":{"id":"m"},"input_tokens":3,"output_tokens":4}`, 7},
		{"pi responseModel string", "pi", `{"type":"message","message":{"role":"assistant","provider":"p","model":"m","responseModel":"m","usage":{"input":3,"output":4}}}`, 7},
		{"pi responseModel object", "pi", `{"type":"message","message":{"role":"assistant","provider":"p","model":"m","responseModel":{"id":"m"},"usage":{"input":3,"output":4}}}`, 7},
		{"pi model and provider objects", "pi", `{"type":"message","message":{"role":"assistant","provider":{"id":"p"},"model":{"id":"m"},"usage":{"input":3,"output":4}}}`, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.jsonl")
			writeFile(t, path, c.line+"\n")
			if n, ok := For(c.adapter).(Telemetry).Usage(path); !ok || n != c.want {
				t.Fatalf("Usage = %d, %v; want %d, true", n, ok, c.want)
			}
		})
	}
}
