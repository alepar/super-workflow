// Package formtest runs every registered form grammar against the shared
// hostile corpus. A module that registers grammars calls CorpusTest from
// one of its tests, so each new grammar is tested by existing.
package formtest

import (
	"errors"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/form"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// CorpusTest checks every grammar registered in this test binary: each of
// its samples and each corpus input it accepts reads back as the same text,
// and each input it refuses is refused with a *form.Error, never turned into
// a different value. It fails when no grammar is registered.
func CorpusTest(t *testing.T) {
	t.Helper()
	grammars := form.Grammars()
	if len(grammars) == 0 {
		t.Fatal("no form grammar is registered in this test binary")
	}
	for _, r := range grammars {
		t.Run(r.Grammar.Name(), func(t *testing.T) {
			for _, s := range r.Grammar.Samples() {
				got, err := r.Probe(s)
				if err != nil {
					t.Errorf("sample %q refused: %v", s, err)
				} else if got != s {
					t.Errorf("sample %q read back as %q", s, got)
				}
			}
			accepted := 0
			for _, in := range hostile.Corpus() {
				got, err := r.Probe(in.Value)
				switch {
				case err == nil && got != in.Value:
					t.Errorf("%s: %q read back as %q", in.Name, in.Value, got)
				case err == nil:
					accepted++
				default:
					var fe *form.Error
					if !errors.As(err, &fe) {
						t.Errorf("%s: refusal %v is not a *form.Error", in.Name, err)
					}
				}
			}
			t.Logf("%d samples; %d of %d corpus inputs accepted", len(r.Grammar.Samples()), accepted, len(hostile.Corpus()))
		})
	}
}
