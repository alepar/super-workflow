package bd

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The tracker parses --label as CSV and trims each field, so a key label
// must carry none of what that would split, unquote or trim raw: %, a double
// quote, a comma, CR, LF or trailing whitespace of any kind. Each is percent-encoded; a
// label with none of them is as it was, so a journal's recorded label still
// matches.
func TestKeyLabelEscapesEveryCSVSpecialCharacter(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{`plain/key`, `key:plain/key`},
		{`a,b`, `key:a%2Cb`},
		{`say "hi"`, `key:say %22hi%22`},
		{"line\nbreak\r", `key:line%0Abreak%0D`},
		{`100% %2C`, `key:100%25 %252C`},
		{`padded `, `key:padded%20`},
		{"tab\t", `key:tab%09`},
		{"nbsp\u00a0", `key:nbsp%C2%A0`},
		{"ls\u2028", `key:ls%E2%80%A8`},
		{"nel\u0085\v\f", `key:nel%C2%85%0B%0C`},
		{" lead", `key: lead`},
	} {
		if got, err := keyLabel(wf.ExternalEffect{Key: wf.KeyOf(tc.key)}); err != nil || got.String() != tc.want {
			t.Errorf("keyLabel(%q) = %q, %v; want %q", tc.key, got, err, tc.want)
		}
	}
	// The escaped label decides the hash threshold: a key under it raw but
	// over it escaped is hashed, and no label passes the threshold.
	key := strings.Repeat(`"`, 60) + strings.Repeat("x", 120)
	got, err := keyLabel(wf.ExternalEffect{Key: wf.KeyOf(key)})
	if err != nil || strings.ContainsAny(got.String(), "\",\r\n") || len(got.String()) > maxKeyLabel || !strings.HasPrefix(got.String(), "key:sha256-") {
		t.Errorf("keyLabel of a key over the threshold once escaped = %q, %v; want the sha256 form", got, err)
	}
	if _, err := keyLabel(wf.ExternalEffect{Key: wf.KeyOf("a\x00b")}); err == nil {
		t.Error("keyLabel of a key holding NUL: no error")
	}
}

// The legacy label differs only for a key the old escaping left alone but
// sink.Label encodes: one holding % or ending in whitespace. Check looks up
// both, so it must not differ for any other key.
func TestLegacyKeyLabelDiffersOnlyForPercentAndEdgeSpace(t *testing.T) {
	for _, tc := range []struct {
		key    string
		differ bool
	}{
		{`plain/key`, false},
		{`a,b "q"` + "\r\n", false},
		{strings.Repeat("x", 300), false},
		{`100%`, true},
		{`padded `, true},
		{"padded\t", true},
		{"x\u00a0", true},
		{" lead", false},
	} {
		e := wf.ExternalEffect{Key: wf.KeyOf(tc.key)}
		l, _ := keyLabel(e)
		if differ := l.String() != legacyKeyLabel(e); differ != tc.differ {
			t.Errorf("key %q: label %q, legacy %q; differ = %v, want %v", tc.key, l, legacyKeyLabel(e), differ, tc.differ)
		}
	}
}

// Check consults the legacy label only where no other key's bead can carry
// it: not where bd trimmed it to another key's stamp, nor where it is the
// current label of another key.
func TestLegacyKeyLookup(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string // "" for no lookup
	}{
		{`plain`, ""},                  // legacy is the current label
		{`100% done`, `key:100% done`}, // no other key's stamp
		{`50%`, `key:50%`},             // ends in %: never a current label
		{"x\t", ""},                    // bd stored it as key:x
		{`padded `, ""},                // bd stored it as key:padded
		{"100%\t", ""},                 // bd stored it as key:100%, the legacy of "100%"
		{`a%2Cb`, ""},                  // the current label of key "a,b"
		{`100%25`, ""},                 // the current label of key "100%"
		{`a%2cb`, `key:a%2cb`},         // decodes, but no key's current label
		// Two old keys can share a legacy stamp: the old escaper wrote a
		// comma as %2C, so "a,%" and "a%2C%" were both stamped key:a%2C%.
		{`a%2C%`, ""},
		{"a,%", ""},
		{`say %22hi%22`, ""}, // also the legacy stamp of say "hi"
		{"x%0Ay %", ""},
	} {
		e := wf.ExternalEffect{Key: wf.KeyOf(tc.key)}
		l, _ := keyLabel(e)
		got, ok := legacyKeyLookup(e, l.String())
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("key %q: legacy lookup %q, %v; want %q", tc.key, got, ok, tc.want)
		}
	}
}

// cutToken works on any argument, invalid UTF-8 included, and cuts only
// whole tokens.
func TestCutToken(t *testing.T) {
	for _, tc := range []struct{ s, tok, want string }{
		{"issue a not found", "a", "issue  not found"},
		{"a banana a", "a", " banana "},
		{"bad \xff byte timed out", "bad \xff byte", " timed out"},
		{"x-a a.b", "a", "x-a a.b"},
		{"tc-1 tc-12", "tc-1", " tc-12"},
		// A match that is not whole must not make the next one look as if
		// it stood at a boundary: the boundary is judged in s itself.
		{"limit 100 exceeded", "0", "limit 100 exceeded"},
		{"2026-11-22", "2", "2026-11-22"},
		{"aaa", "a", "aaa"},
		{"aaa a", "a", "aaa "},
		{"xx x", "x", "xx "},
		{"ababab ab", "ab", "ababab "},
		{"00 0", "0", "00 "},
		{"tc-11 tc-1", "1", "tc-11 tc-1"},
		{"a a", "a", " "},
	} {
		if got := cutToken(tc.s, tc.tok); got != tc.want {
			t.Errorf("cutToken(%q, %q) = %q, want %q", tc.s, tc.tok, got, tc.want)
		}
	}
}

// A multi-line reason is found as a block; a partial block is not.
func TestHasReasonMultiLine(t *testing.T) {
	desc := "intro\nline one\r\nline two\nend"
	if !hasReason(desc, "line one\r\nline two") {
		t.Error("multi-line reason not found as a block")
	}
	if hasReason(desc, "line one\nline three") {
		t.Error("partial block found")
	}
	if !hasReason(desc, "  end ") {
		t.Error("single line not found")
	}
}
