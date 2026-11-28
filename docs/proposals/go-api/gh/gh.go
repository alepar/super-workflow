// Package gh is the GitHub adapter. Its constructors return descriptors that
// run nothing; Runtime (runtime.go) performs them through the gh CLI. It covers
// a plugin manifest read and issue view and create.
package gh

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// Plugin is the part of .claude-plugin/plugin.json the adapter reads.
type Plugin struct{ Name, Version string }

// Issue is a GitHub issue.
type Issue struct {
	Number             int
	Title, Body, State string
	// Author is the login of the gh identity that created it: the identity
	// the effect ran as, recorded at Apply.
	Author string `json:",omitempty"`
}

// PluginJSON observes plugin.json of repo at ref.
func PluginJSON(repo, ref string) wf.Observation[Plugin] {
	return wf.Observe[Plugin]("gh", "plugin_json", map[string]any{"repo": repo, "ref": ref})
}

// IssueView observes `gh issue view <n>` in repo.
func IssueView(repo string, n int) wf.Observation[Issue] {
	return wf.Observe[Issue]("gh", "issue_view", map[string]any{"repo": repo, "number": n})
}

// IssueCreate files an issue in repo.
//
// Idempotent by key. already_done: the adapter appends a key marker to body,
// searches the repo's issues for that marker before creating, and returns the
// match instead of creating a duplicate.
func IssueCreate(key wf.Key, repo, title, body string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "gh", Op: "issue_create", Args: map[string]any{"repo": repo, "title": title, "body": body}, Key: key}
}
