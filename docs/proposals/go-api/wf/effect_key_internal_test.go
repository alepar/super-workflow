package wf

import (
	"slices"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// splitKeyParts reads back every key joinKeyParts writes, whatever its parts
// hold, and refuses a % that is not an escape.
func TestEffectKeyPartsRoundTrip(t *testing.T) {
	var all []string
	for _, in := range hostile.Corpus() {
		all = append(all, in.Value)
		for _, parts := range [][]string{{in.Value}, {"fix", in.Value}, {in.Value, "/", "%2F", ""}} {
			got, err := splitKeyParts(joinKeyParts(parts))
			if err != nil || !slices.Equal(got, parts) {
				t.Errorf("%s: %q read back as %q, %v", in.Name, parts, got, err)
			}
		}
	}
	if got, err := splitKeyParts(joinKeyParts(all)); err != nil || !slices.Equal(got, all) {
		t.Errorf("the whole corpus as one key: %v", err)
	}
	for _, bad := range []string{"a%", "a%2", "a%2f", "%41"} {
		if _, err := splitKeyParts(bad); err == nil {
			t.Errorf("splitKeyParts(%q) accepted a stray %%", bad)
		}
	}
}

// A key of one part keeps its spelling; one of several shows, and hashes,
// as its unescaped join.
func TestEffectKeySpellings(t *testing.T) {
	if got := KeyOf("fix/a/b").key(); got != "fix/a/b" {
		t.Errorf("one part: %q", got)
	}
	if got := KeyOf("fix", "a/b").key(); got != "fix/a/b" {
		t.Errorf("shown as %q, want the unescaped join", got)
	}
	s := scope{}
	one, err := s.bindEffect(EffectData{Kind: "external", Key: "50%/x"})
	if err != nil || one.Key != "50%/x" || one.LegacyKey != "" {
		t.Errorf("one-part bound key %q legacy %q, %v", one.Key, one.LegacyKey, err)
	}
	k := KeyWith("fix", TextOf("a/b")).(textKey)
	var parts [][]TextSpan
	for _, p := range k.parts {
		parts = append(parts, p.spans)
	}
	e, err := s.bindEffect(EffectData{Kind: "external", Key: k.key(), KeyTemplate: k.text.spans, KeyPartTemplates: parts, BindingVersion: effectBindingVersion})
	if err != nil || e.Key != "fix/a%2Fb" || e.LegacyKey != "fix/a/b" || e.KeyPartTemplates != nil {
		t.Errorf("KeyWith bound to %+v, %v", e, err)
	}
}
