package gh

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Config configures the gh runtime.
type Config struct {
	// Bin is the gh executable; "" means "gh".
	Bin string
	// Timeout bounds each call; wf.ToolTimeout when zero.
	Timeout time.Duration
}

// Runtime returns the wf.AdapterRuntime that performs every op this package
// declares through the gh CLI, in the run workspace.
//
// Owns: gh's argv templating, the effect-key marker in created issues and
// decoding gh output into Plugin and Issue.
// Does not: journal, retry or route (the Engine does), or call gh on behalf of
// evals: they use a fake world.
// Extend by: a new op is a constructor in gh.go plus one case in each method.
func Runtime(cfg Config) wf.AdapterRuntime {
	if cfg.Bin == "" {
		cfg.Bin = "gh"
	}
	return runtime{bin: cfg.Bin, timeout: cfg.Timeout}
}

type runtime struct {
	bin     string
	timeout time.Duration
}

func (runtime) Ops() []string {
	return []string{"gh.plugin_json", "gh.issue_view", "gh.issue_create"}
}

// run executes gh with args in workspace and returns its stdout. A missing gh
// and a timeout are wf.ErrUnavailable; a non-zero exit is an ordinary error.
func (r runtime) run(workspace string, args ...string) ([]byte, error) {
	res, err := wf.RunTool(wf.Tool{Bin: r.bin, Args: args, Dir: workspace, Timeout: r.timeout})
	if err != nil {
		return nil, err
	}
	if res.Exit != 0 {
		return nil, fmt.Errorf("gh %s: exit %d: %s", strings.Join(args, " "), res.Exit, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (r runtime) Read(workspace string, d wf.ObservationDesc) (json.RawMessage, error) {
	repo := str(d.Args, "repo")
	switch d.Op {
	case "plugin_json":
		out, err := r.run(workspace, "api", "repos/"+repo+"/contents/.claude-plugin/plugin.json?ref="+str(d.Args, "ref"),
			"-H", "Accept: application/vnd.github.raw")
		if err != nil {
			return nil, err
		}
		var p struct{ Name, Version string }
		if err := json.Unmarshal(out, &p); err != nil {
			return nil, fmt.Errorf("gh plugin_json: decode: %w", err)
		}
		return json.Marshal(Plugin(p))
	case "issue_view":
		out, err := r.run(workspace, "issue", "view", fmt.Sprint(d.Args["number"]), "--repo", repo,
			"--json", "number,title,body,state")
		if err != nil {
			return nil, err
		}
		var i Issue
		if err := json.Unmarshal(out, &i); err != nil {
			return nil, fmt.Errorf("gh issue_view: decode: %w", err)
		}
		return json.Marshal(i)
	}
	return nil, fmt.Errorf("gh: unknown observation %q", d.Op)
}

// marker is the body text that lets Check find an issue created for a key.
func marker(e wf.ExternalEffect) string {
	return "<!-- effect-key: " + e.KeyID() + " -->"
}

func (r runtime) Apply(workspace string, e wf.ExternalEffect) (json.RawMessage, error) {
	if e.Op != "issue_create" {
		return nil, fmt.Errorf("gh: unknown effect %q", e.Op)
	}
	body := str(e.Args, "body")
	if e.KeyID() != "" {
		body += "\n\n" + marker(e)
	}
	out, err := r.run(workspace, "issue", "create", "--repo", str(e.Args, "repo"),
		"--title", str(e.Args, "title"), "--body", body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSpace(string(out))
	n, err := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	if err != nil {
		return nil, fmt.Errorf("gh issue_create: no issue number in %q", url)
	}
	return json.Marshal(Issue{Number: n, Title: str(e.Args, "title"), Body: body, State: "OPEN"})
}

// Check searches the repo's issues, open or closed, for the effect's marker.
// GitHub's search matches terms, not the exact text, so a hit counts only when
// its body holds the exact marker: another key whose terms overlap this one's
// (run tag 2026-11-04-foo against 2026-11-04-foo-bar) is not done.
func (r runtime) Check(workspace string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	if e.Op != "issue_create" {
		return wf.EffectUnknown, fmt.Errorf("gh: unknown effect %q", e.Op)
	}
	if e.KeyID() == "" {
		return wf.EffectUnknown, fmt.Errorf("gh issue_create: effect has no idempotency key to search for")
	}
	m := marker(e)
	out, err := r.run(workspace, "issue", "list", "--repo", str(e.Args, "repo"),
		"--search", `"`+m+`" in:body`, "--json", "number,body", "--state", "all")
	if err != nil {
		return wf.EffectUnknown, err
	}
	var found []struct {
		Number int
		Body   string
	}
	if err := json.Unmarshal(out, &found); err != nil {
		return wf.EffectUnknown, fmt.Errorf("gh issue_create: decode search: %w", err)
	}
	for _, f := range found {
		if strings.Contains(f.Body, m) {
			return wf.EffectDone, nil
		}
	}
	return wf.EffectNotDone, nil
}
