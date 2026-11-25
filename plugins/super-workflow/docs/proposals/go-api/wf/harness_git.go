package wf

// GitWritableRemedy is the one shared fix text for an agent sandbox whose repo
// git dir is not writable. git.WritableCheck returns it as its Remedy and
// Bootstrap prints it in the Codex notes, so neither restates it.
// It only describes the setting; no code path writes any profile or sandbox
// configuration.
//
// Codex config reference documents sandbox_mode = "workspace-write" and the
// key sandbox_workspace_write.writable_roots (array<string>, "Additional
// writable roots when sandbox_mode = workspace-write"):
// https://learn.chatgpt.com/docs/config-file/config-reference
// (developers.openai.com/codex/config-reference redirects there).
// Codex keeps .git read-only under workspace-write. The run repository can
// grant access by naming the .git directory itself in writable_roots.
func GitWritableRemedy(harness string) string {
	if normalizeHarness(harness) == "codex" {
		return "under Codex sandbox_mode = \"workspace-write\" the agent sandbox keeps .git read-only; " +
			"make the repo's git dir writable by adding it as an extra writable root in the run repo's " +
			".codex/config.toml: [sandbox_workspace_write] writable_roots = [\"<repo>/.git\"]. " +
			"For a linked worktree add the common git dir instead (`git rev-parse --git-common-dir`). " +
			gitWritableCaveat
	}
	return "make the repo's git dir writable for the agent (its sandbox or file permissions currently deny writes to it); " +
		"for a linked worktree that is the common git dir (`git rev-parse --git-common-dir`). " +
		gitWritableCaveat
}

// gitWritableCaveat warns what a writable git dir costs: hooks and config run
// on the host, outside the agent sandbox.
const gitWritableCaveat = "Caveat: a writable git dir also lets the sandboxed agent write .git/hooks and .git/config, " +
	"which run outside the sandbox on the host's next git command"
