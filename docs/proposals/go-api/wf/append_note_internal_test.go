package wf

import "testing"

func TestAppendNote(t *testing.T) {
	for _, c := range []struct{ note, add, want string }{
		{"(attempt 1 lost)", "ran a, asked for b", "(attempt 1 lost) ran a, asked for b"},
		{"", "ran a, asked for b", "ran a, asked for b"},
		{"(attempt 1 lost)", "", "(attempt 1 lost)"},
		{"", "", ""},
	} {
		if got := appendNote(c.note, c.add); got != c.want {
			t.Errorf("appendNote(%q, %q) = %q, want %q", c.note, c.add, got, c.want)
		}
	}
}
