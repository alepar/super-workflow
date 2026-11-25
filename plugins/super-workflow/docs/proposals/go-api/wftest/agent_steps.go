package wftest

import (
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"os"
	"strings"
)

// WithArtifacts scripts this submission writing the named allocated artifacts
// before done. Names must be declared by this step; paths come from its real
// prompt, never from a test's reconstruction of the allocation algorithm.
func (s Step) WithArtifacts(files map[string]string) Step {
	s.files = make(map[string]string, len(files))
	for name, content := range files {
		s.files[name] = content
	}
	return s
}

// Fail scripts a fake agent reporting a failed unit, using the same failure
// classes as wf failed (transient retries; blocked follows its declared route).
func Fail[S, O any](a *wf.Agent[S, O], class wf.FailureClass, reason string) Step {
	name := markerName[S]()
	return Step{kind: stepFail, path: name, desc: fmt.Sprintf("Fail at %q (%s: %s)", name, class, reason), times: 1, failure: class, reason: reason}
}

func (r *runner) writeArtifacts(st Step, in wf.Instruction) error {
	if len(st.files) == 0 {
		return nil
	}
	// The engine appends its allocation after the user-authored prompt, which
	// may itself contain an Artifacts example. Use that final engine section.
	marker := "\n## Artifacts\n"
	index := strings.LastIndex(in.Prompt, marker)
	if index < 0 {
		return fmt.Errorf("%s has no declared allocated artifacts", in.State)
	}
	block, _, _ := strings.Cut(in.Prompt[index+len(marker):], "\n## ")
	for name, content := range st.files {
		path := ""
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "- "+name+": "); ok {
				path = strings.TrimSuffix(p, " (optional: leave it unwritten when there is nothing to record)")
				break
			}
		}
		if path == "" {
			return fmt.Errorf("%s does not declare allocated artifact %q", in.State, name)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// String renders the actual journal with named kinds and outcome classes.
func (t *Trace) String() string {
	var b strings.Builder
	for _, rec := range t.journal {
		class := "-"
		if rec.Class != 0 {
			class = rec.Class.String()
		}
		fmt.Fprintf(&b, "%d %s %s %s/%s %s\n", rec.Seq, rec.Kind, class, rec.Instance, rec.State, rec.Data)
	}
	return b.String()
}
