package wf

import "testing"

// an earlier revision review round 1: the run's current and recorded roots are the same
// for a run never moved, and a fork's root may lie below the source root
// (sourceRootIn allows it): the rewrite is one pass, so a rewritten path is
// never rewritten again.
func TestRelocateParamRootsRewritesOnce(t *testing.T) {
	src := PathRoots{Workspace: "/repo", State: "/repo/.superpowers/state"}
	to := PathRoots{Workspace: "/repo/.scratch/fork/ws", State: "/repo/.scratch/fork/state"}
	for _, tc := range []struct{ in, want string }{
		{"cat > /repo/.superpowers/h.md", "cat > /repo/.scratch/fork/ws/.superpowers/h.md"},
		{"cat > /repo/.superpowers/state/x", "cat > /repo/.scratch/fork/state/x"},
		{"/repo", "/repo/.scratch/fork/ws"},
		{"ls /repo-other/x", "ls /repo-other/x"},
	} {
		if got := relocateParamRoots(tc.in, []PathRoots{src, src}, to); got != tc.want {
			t.Errorf("relocateParamRoots(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	moved := PathRoots{Workspace: "/old", State: "/old/state"}
	if got := relocateParamRoots("cat > /old/h.md; cat /repo/x", []PathRoots{src, moved}, to); got != "cat > /repo/.scratch/fork/ws/h.md; cat /repo/.scratch/fork/ws/x" {
		t.Errorf("moved run: %q", got)
	}
}
