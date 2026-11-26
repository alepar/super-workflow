package gh

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
		ep, err := pluginEndpoint(repo, str(d.Args, "ref"))
		if err != nil {
			return nil, err
		}
		out, err := r.run(workspace, "api", ep, "-H", "Accept: application/vnd.github.raw")
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

// pluginEndpoint is the API path of repo's plugin.json at ref. The repo must
// be exactly owner/name, so it fills the two path segments it names and no
// other (a "..", a "?" or a "#" in it would reach another endpoint); the ref
// is query-escaped, so it is the whole value of the ref parameter.
func pluginEndpoint(repo, ref string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !repoComponent(owner) || !repoComponent(name) {
		return "", fmt.Errorf("gh plugin_json: repo %q is not owner/name", repo)
	}
	return "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) +
		"/contents/.claude-plugin/plugin.json?ref=" + url.QueryEscape(ref), nil
}

// repoComponent reports whether s can be a GitHub owner or repository name:
// letters, digits, '-', '_' and '.', and not a dot segment.
func repoComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) < 0
}

// marker is the body text that lets Check find an issue created for a key.
func marker(e wf.ExternalEffect) string {
	return "<!-- effect-key: " + markerKey(e.KeyID()) + " -->"
}

// markerKey is key as it is written in the marker. The marker must stay one
// HTML comment ('>' could close it) and one quoted search phrase ('"' or '\'
// would break the phrase, so Check would miss the issue and create a
// duplicate), and GitHub must store it byte for byte (it rewrites control
// characters and invalid UTF-8). Each such byte is percent-encoded, '%' too so
// the form decodes back to the key. Only those bytes change: a key without
// them keeps the marker issues were created with before the encoding.
func markerKey(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); {
		r, size := utf8.DecodeRuneInString(key[i:])
		if r == utf8.RuneError && size == 1 || r < 0x20 || r == 0x7f || strings.ContainsRune(`%"\<>`, r) {
			for _, c := range []byte(key[i : i+size]) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		} else {
			b.WriteString(key[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Apply creates the issue. A keyed one is searched for first, by its marker,
// and the issue found is returned instead of creating a second: an earlier
// create may have reached GitHub although its result never came back.
func (r runtime) Apply(workspace string, e wf.ExternalEffect) (json.RawMessage, error) {
	if e.Op != "issue_create" {
		return nil, fmt.Errorf("gh: unknown effect %q", e.Op)
	}
	body := str(e.Args, "body")
	if e.KeyID() != "" {
		found, err := r.find(workspace, e)
		if err != nil {
			return nil, err
		}
		if found != nil {
			return json.Marshal(found)
		}
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

// Check reports whether the effect's issue exists (CheckResultAt).
func (r runtime) Check(workspace string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	st, _, err := r.check(workspace, e)
	return st, err
}

// ApplyAt, CheckAt and CheckResultAt refuse a sandboxed run's call: gh
// reaches the real GitHub and has no sandbox, and a run made by a rewind or
// a snapshot restore must never create an issue there. A sandboxed run's
// observations (Read) are not refused: they change nothing.
func (r runtime) ApplyAt(workspace string, at wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, error) {
	if at.Sandboxed {
		return nil, wf.SandboxRefusal(e, sandboxWhy)
	}
	return r.Apply(workspace, e)
}

func (r runtime) CheckAt(workspace string, at wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, error) {
	st, _, err := r.CheckResultAt(workspace, at, e)
	return st, err
}

// CheckResultAt searches for the effect's issue and, when it finds it,
// returns it as Apply would have, so the Engine journals it on the "done"
// result.
func (r runtime) CheckResultAt(workspace string, at wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, json.RawMessage, error) {
	if at.Sandboxed {
		return wf.EffectUnknown, nil, wf.SandboxRefusal(e, sandboxWhy)
	}
	return r.check(workspace, e)
}

const sandboxWhy = "gh reaches the real GitHub and cannot be sandboxed, so a rewound or restored run never performs or checks a gh effect"

func (r runtime) check(workspace string, e wf.ExternalEffect) (wf.EffectStatus, json.RawMessage, error) {
	if e.Op != "issue_create" {
		return wf.EffectUnknown, nil, fmt.Errorf("gh: unknown effect %q", e.Op)
	}
	if e.KeyID() == "" {
		return wf.EffectUnknown, nil, fmt.Errorf("gh issue_create: effect has no idempotency key to search for")
	}
	found, err := r.find(workspace, e)
	if err != nil {
		return wf.EffectUnknown, nil, err
	}
	if found == nil {
		return wf.EffectNotDone, nil, nil
	}
	res, err := json.Marshal(found)
	if err != nil {
		return wf.EffectUnknown, nil, err
	}
	return wf.EffectDone, res, nil
}

// find searches the repo's issues, open or closed, for the effect's marker.
// GitHub's search matches terms, not the exact text, so a hit counts only when
// its body holds the exact marker: another key whose terms overlap this one's
// (run tag 2026-11-04-foo against 2026-11-04-foo-bar) is not a match. nil
// means no issue holds it.
func (r runtime) find(workspace string, e wf.ExternalEffect) (*Issue, error) {
	m := marker(e)
	out, err := r.run(workspace, "issue", "list", "--repo", str(e.Args, "repo"),
		"--search", `"`+m+`" in:body`, "--json", "number,title,body,state", "--state", "all")
	if err != nil {
		return nil, err
	}
	var found []Issue
	if err := json.Unmarshal(out, &found); err != nil {
		return nil, fmt.Errorf("gh issue_create: decode search: %w", err)
	}
	for _, f := range found {
		if strings.Contains(f.Body, m) {
			return &f, nil
		}
	}
	return nil, nil
}
