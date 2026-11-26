package git

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// HeadCheck is a wf.Check constructor: done-time form check of the head a
// typed result reports, read through git in worktree. head takes the reported
// commit from the decoded submission.
//
// Owns: the form rule a later merge relies on. The head is a full commit SHA
// (40 or 64 lower hex digits) that resolves to itself, so the merge lands
// exactly the commit that was reported and never whatever a name (a branch,
// HEAD, an abbreviated SHA, a revision expression, a tag object) later
// resolves to. The read is declared as a submission-scoped observation
// (Check.ObservesFrom).
// Does not: judge the commit's content or ancestry (whether it descends from a
// base, sits on an integration branch or carries a change): the agent's
// result attests that. It does not decide which results carry a head (the
// client composes it into the Spec of each state whose result does), merge or
// route. A failure is an ordinary check refusal naming the condition (not a
// full commit SHA, missing); an unavailable git read is returned as
// wf.ErrUnavailable, not a refusal.
func HeadCheck[O any](worktree string, head func(out O) string) wf.Check[O] {
	return wf.Checkf("head", func(out O, c wf.Ctx) error {
		h := head(out)
		if !isFullSHA(h) {
			return fmt.Errorf("head %q is not a full commit SHA", h)
		}
		got, err := wf.Observed(c, ResolveCommit(worktree, h))
		switch {
		case errors.Is(err, wf.ErrUnavailable):
			return err
		case err != nil:
			return fmt.Errorf("head %s is missing: %v", h, err)
		case got != h:
			return fmt.Errorf("head %s is not a full commit SHA: it resolves to %s", h, got)
		}
		return nil
	}).ObservesFrom(func(out O) []wf.Reader {
		if h := head(out); isFullSHA(h) {
			return []wf.Reader{ResolveCommit(worktree, h)}
		}
		return nil
	})
}

// fullSHA matches a full SHA-1 or SHA-256 object name.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// isFullSHA reports whether s is a full commit object name (40 or 64 lower
// hex digits), the only commit form HeadCheck accepts and a task Merge takes.
func isFullSHA(s string) bool { return fullSHA.MatchString(s) }
