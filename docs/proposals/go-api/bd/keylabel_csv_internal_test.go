package bd

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The tracker parses --label as CSV, so a key label must
// carry none of its special characters raw: a double quote, a comma, CR or
// LF. Each is percent-encoded; a label with none of them is as it was, so a
// journal's recorded label still matches.
func TestKeyLabelEscapesEveryCSVSpecialCharacter(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{`plain/key`, `key:plain/key`},
		{`a,b`, `key:a%2Cb`},
		{`say "hi"`, `key:say %22hi%22`},
		{"line\nbreak\r", `key:line%0Abreak%0D`},
	} {
		if got := keyLabel(wf.ExternalEffect{Key: wf.KeyOf(tc.key)}); got != tc.want {
			t.Errorf("keyLabel(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	// The escaped label decides the hash threshold: a key under it raw but
	// over it escaped is hashed, and no label passes the threshold.
	key := strings.Repeat(`"`, 60) + strings.Repeat("x", 120)
	got := keyLabel(wf.ExternalEffect{Key: wf.KeyOf(key)})
	if strings.ContainsAny(got, "\",\r\n") || len(got) > maxKeyLabel || !strings.HasPrefix(got, "key:sha256-") {
		t.Errorf("keyLabel of a key over the threshold once escaped = %q, want the sha256 form", got)
	}
}
