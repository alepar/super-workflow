package wf

import "testing"

// A root read from the filesystem can hold invalid UTF-8, which the
// instruction's JSON would deliver as another path: refused.
func TestShellWordRefusesInvalidUTF8(t *testing.T) {
	for _, mode := range []string{shellWord, shellWordList} {
		if _, err := quoteBound(mode, "/ws/a\xffb"); err == nil {
			t.Errorf("%s: invalid UTF-8 quoted, want a refusal", mode)
		}
	}
}

// Reference has no effect on a shell input: it is presented as bound.
func TestReferenceLeavesAShellInputAsBound(t *testing.T) {
	if d := Reference(ShellBound("W", "{dir}"), true).inputRef(); d.reference != "" || d.shell != shellWord {
		t.Errorf("reference %q, shell %q", d.reference, d.shell)
	}
}
