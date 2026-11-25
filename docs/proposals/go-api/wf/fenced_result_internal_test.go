package wf

import (
	"encoding/json"
	"testing"
)

type fenceAsk struct{}
type fenceOut struct{ OK bool }

// agentNode builds a one-agent definition whose output type is O and returns
// the agent's node.
func agentNode[O any](t *testing.T) *node {
	t.Helper()
	d := New("fence")
	NewAgent[fenceAsk, O](d)
	for _, n := range d.nodes {
		if _, ok := n.kind.(*agentKind); ok {
			return n
		}
	}
	t.Fatal("no agent node")
	return nil
}

func wrappedText(t *testing.T, text string) Submission {
	t.Helper()
	raw, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return Submission{Output: raw, textWrapped: true}
}

// TestUnfencedForOutputType: the fence is unwrapped only where the file's text
// does not already decode into the state's output type.
func TestUnfencedForOutputType(t *testing.T) {
	fenced := "```json\n{\"OK\":true}\n```"

	got := unfencedFor(agentNode[fenceOut](t), wrappedText(t, fenced))
	if string(got.Output) != `{"OK":true}` || got.Normalized != fenceNote {
		t.Errorf("struct output: %s %q, want the fence's contents with the note", got.Output, got.Normalized)
	}

	for name, n := range map[string]*node{
		"string":          agentNode[string](t),
		"any":             agentNode[any](t),
		"json.RawMessage": agentNode[json.RawMessage](t),
	} {
		in := wrappedText(t, fenced)
		got := unfencedFor(n, in)
		if string(got.Output) != string(in.Output) || got.Normalized != "" {
			t.Errorf("%s output: %s %q, want the file's text unchanged", name, got.Output, got.Normalized)
		}
	}

	// Output that was valid JSON as written (here a JSON string whose value
	// happens to be a fenced block) is never unwrapped.
	plain := wrappedText(t, fenced)
	plain.textWrapped = false
	if got := unfencedFor(agentNode[fenceOut](t), plain); string(got.Output) != string(plain.Output) || got.Normalized != "" {
		t.Errorf("valid JSON as written: %s %q, want it unchanged", got.Output, got.Normalized)
	}
}
