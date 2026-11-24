package wf

import "testing"

func TestProseApostrophes(t *testing.T) {
	for in, want := range map[string]string{
		"don't":         "don’t",
		"the user's":    "the user’s",
		"grep 'x'":      "grep 'x'",
		"'quoted'":      "'quoted'",
		"it's 'x'":      "it’s 'x'",
		"'start":        "'start",
		"end'":          "end'",
		"é'é":           "é’é",
		"a 'b' c":       "a 'b' c",
		"":              "",
		"no apostrophe": "no apostrophe",
	} {
		if got := proseApostrophes(in); got != want {
			t.Errorf("proseApostrophes(%q) = %q, want %q", in, got, want)
		}
	}
}
