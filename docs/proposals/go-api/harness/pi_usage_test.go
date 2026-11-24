package harness

import (
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is built from the documented pi 1.5.2 session format, not
// recorded from a live session. It holds one entry of each kind that records
// usage: assistant message 100+5+20, tool-result message 7+1+3, usage entry
// 11+2+13, compaction 300+0+40, branch_summary 17+23+19. Cache reads and
// reasoning are left out of every figure.
const piUsageHeader = `{"type":"session","version":3,"id":"usage-fixture","timestamp":"2026-11-19T00:00:00.000Z","cwd":"/work"}`

func TestPiUsageCountsEveryKind(t *testing.T) {
	n, ok := For("pi").(Telemetry).Usage("testdata/usage/pi-all-kinds.jsonl")
	if !ok || n != 561 {
		t.Fatalf("Usage = (%d, %v), want (561, true); the old assistant-only rule gives 125", n, ok)
	}
}

func TestPiUsageEachKindAlone(t *testing.T) {
	lines := []struct {
		kind string
		line string
		want int
	}{
		{"assistant message", `{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"x"}],"usage":{"input":100,"output":20,"cacheRead":1000,"cacheWrite":5,"reasoning":7,"totalTokens":1125}}}`, 125},
		{"tool-result message", `{"type":"message","id":"m3","message":{"role":"toolResult","toolCallId":"c1","content":[{"type":"text","text":"a"}],"usage":{"input":7,"output":3,"cacheRead":50,"cacheWrite":1}}}`, 11},
		{"usage entry", `{"type":"usage","id":"u1","kind":"auxiliary","usage":{"input":11,"output":13,"cacheRead":900,"cacheWrite":2}}`, 26},
		{"compaction", `{"type":"compaction","id":"k1","summary":"s","usage":{"input":300,"output":40,"cacheRead":0,"cacheWrite":0}}`, 340},
		{"branch_summary", `{"type":"branch_summary","id":"b1","summary":"s","usage":{"input":17,"output":19,"cacheRead":400,"cacheWrite":23}}`, 59},
	}
	for _, c := range lines {
		t.Run(c.kind, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "s.jsonl")
			writeFile(t, p, strings.Join([]string{piUsageHeader, c.line}, "\n")+"\n")
			n, ok := For("pi").(Telemetry).Usage(p)
			if !ok || n != c.want {
				t.Fatalf("Usage = (%d, %v), want (%d, true)", n, ok, c.want)
			}
		})
	}
}

func TestPiUsageNothingRecorded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, p, strings.Join([]string{
		piUsageHeader,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"compaction","id":"k1","summary":"s"}`,
	}, "\n")+"\n")
	if n, ok := For("pi").(Telemetry).Usage(p); ok || n != 0 {
		t.Fatalf("Usage = (%d, %v), want (0, false)", n, ok)
	}
	if n, ok := For("pi").(Telemetry).Usage(filepath.Join(t.TempDir(), "missing.jsonl")); ok || n != 0 {
		t.Fatalf("missing file: Usage = (%d, %v), want (0, false)", n, ok)
	}
}
