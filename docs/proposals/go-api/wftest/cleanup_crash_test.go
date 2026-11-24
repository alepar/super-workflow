package wftest_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type cleanupCrashItem struct{ ID, Workdir string }

func init() {
	for _, kind := range []wf.JournalKind{wf.JournalCleanupDeferred, wf.JournalCleanupApplied} {
		crashPoints[kind] = scenario(kind, func(t *testing.T, crash bool) *drv {
			root := t.TempDir()
			target := filepath.Join(root, "task")
			child := wf.New("cleanup-crash-child")
			work := wf.NewAgent[crFWork, pgOut](child)
			done := wf.NewTerminal[pgOK](child, wf.Success)
			work.Spec(wf.PromptText("work {{item}}", wf.ChildInput()), wf.Retry(wf.Const(1), work.To(done, "stopped")))
			work.Then(work.To(done, "done").With(wf.ExternalEffect{Adapter: "git", Op: "worktree_remove", Args: map[string]any{"path": target}, Key: wf.KeyOf("remove", "{id}")}))
			wf.Yield(done, func(wf.Ctx) pgRes { return pgRes{} })
			d := wf.New("cleanup-crash")
			fan := wf.NewMap[crFan, cleanupCrashItem, string, pgRes](d, wf.FromValues(cleanupCrashItem{"one", target}, cleanupCrashItem{"two", filepath.Join(target, "subdir")}), func(i cleanupCrashItem) string { return i.ID }, wf.Typed[cleanupCrashItem, pgRes](child))
			end := wf.NewTerminal[crFMerge](d, wf.Success)
			fan.Parallel(2).Join(wf.Quorum(1), fan.To(end, "met"), fan.To(end, "partial"))
			return newDrv(t, crash, d, nil, func(o *wf.EngineOptions) { o.Workspace = root })
		}, func(h *drv) {
			units, err := h.e.HandOut(h.id, 2)
			h.must(err)
			if len(units) != 2 {
				h.t.Fatalf("handout: %+v", units)
			}
			h.must(h.e.Started(h.id, units[0].Name, wf.Claim{Harness: "wftest", Session: "one", Agent: "one"}))
			h.at(kind, func() {
				_, err := h.e.Done(h.id, units[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
				h.must(err)
			})
			h.next()
			st, err := h.e.Status(h.id)
			h.must(err)
			if st.Phase != wf.RunEnded || len(st.PendingCleanups) != 0 {
				h.t.Fatalf("cleanup did not settle: %+v", st)
			}
		})
	}
}
