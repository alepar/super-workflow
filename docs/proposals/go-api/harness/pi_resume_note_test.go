package harness

import (
	"strings"
	"testing"
)

func TestPiResumeNoteNamesTheDirectoryAndTheFallback(t *testing.T) {
	n, ok := For("pi").(ResumeNote)
	if !ok {
		t.Fatal("pi does not implement ResumeNote")
	}
	got := n.ResumeNote()
	for _, want := range []string{"own working directory", "new session", "restart"} {
		if !strings.Contains(got, want) {
			t.Errorf("ResumeNote = %q, want it to say %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("ResumeNote = %q, want one line", got)
	}
	for _, h := range []string{"claude", "generic"} {
		if _, ok := For(h).(ResumeNote); ok {
			t.Errorf("%s must not implement ResumeNote", h)
		}
	}
}
