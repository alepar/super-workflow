package gh_test

import (
	"net/url"
	"path"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/gh"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// markerKey reads the key out of a marker "<!-- effect-key: K -->" whose K
// is percent-encoded, reporting whether the marker has that shape.
func markerKey(m string) (string, bool) {
	k, ok := strings.CutPrefix(m, "<!-- effect-key: ")
	k, ok2 := strings.CutSuffix(k, " -->")
	if !ok || !ok2 {
		return "", false
	}
	key, err := url.PathUnescape(k)
	return key, err == nil
}

func hostileFlag(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// A title and body reach gh as the values of their flags, and the key
// marker appended to the body stays one HTML comment that holds the key,
// percent-encoded where its characters could end the comment or break the
// search phrase.
func TestHostileIssueCreateRoundTrip(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			if strings.ContainsRune(in.Value, 0) {
				t.Skip("argv cannot carry NUL; exec refuses it")
			}
			log := stubGh(t, "https://github.com/o/r/issues/7\n", 0)
			rt := gh.Runtime(gh.Config{})
			e := gh.IssueCreate(wf.KeyOf("k", in.Value), "o/r", in.Value, in.Value)
			if _, err := rt.Apply(t.TempDir(), e); err != nil {
				t.Skipf("refused: %v", err)
			}
			_, args := argv(t, log)
			if v, _ := hostileFlag(args, "--title"); v != in.Value {
				t.Errorf("--title %q, want %q", v, in.Value)
			}
			body, _ := hostileFlag(args, "--body")
			marker, ok := strings.CutPrefix(body, in.Value+"\n\n")
			if !ok {
				t.Fatalf("--body %q, want the body then the marker", body)
			}
			if key, ok := markerKey(marker); !ok || key != e.KeyID() {
				t.Errorf("marker %q holds key %q, want %q", marker, key, e.KeyID())
			}
			// Markdown renders the marker as an HTML comment, which ends at
			// the first "-->": the marker must be wholly that one comment.
			c, _ := strings.CutPrefix(marker, "<!--")
			if end := strings.Index(c, "-->"); end < 0 || end+3 != len(c) || strings.Contains(c, "<!--") {
				t.Errorf("marker %q does not parse as one HTML comment", marker)
			}
			// A key with nothing to encode keeps the marker issues were
			// created with before the encoding, so Check still finds them.
			if in.Name == "plain" && marker != "<!-- effect-key: "+e.KeyID()+" -->" {
				t.Errorf("plain key marker %q changed form", marker)
			}
		})
	}
}

// Check searches for the marker as one quoted phrase: the phrase GitHub
// reads must be the whole marker, and the marker must hold the key.
func TestHostileIssueCheckSearchRoundTrip(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			if strings.ContainsRune(in.Value, 0) {
				t.Skip("argv cannot carry NUL; exec refuses it")
			}
			log := stubGh(t, "[]", 0)
			rt := gh.Runtime(gh.Config{})
			e := gh.IssueCreate(wf.KeyOf("k", in.Value), "o/r", "t", "b")
			if _, err := rt.Check(t.TempDir(), e); err != nil {
				t.Skipf("refused: %v", err)
			}
			_, args := argv(t, log)
			q, _ := hostileFlag(args, "--search")
			rest, ok := strings.CutPrefix(q, `"`)
			end := strings.Index(rest, `"`)
			if !ok || end < 0 || rest[end+1:] != " in:body" || strings.Contains(rest[:end], `\`) {
				t.Fatalf("--search %q does not read as one quoted phrase then in:body", q)
			}
			if key, ok := markerKey(rest[:end]); !ok || key != e.KeyID() {
				t.Errorf("--search phrase %q is not the marker of key %q", rest[:end], e.KeyID())
			}
		})
	}
}

// PluginJSON's ref reaches GitHub as the ref query parameter, and a repo as
// the path segments between repos/ and /contents.
func TestHostilePluginJSONEndpointRoundTrip(t *testing.T) {
	const tail = "/contents/.claude-plugin/plugin.json"
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			if strings.ContainsRune(in.Value, 0) {
				t.Skip("argv cannot carry NUL; exec refuses it")
			}
			for _, c := range []struct{ what, repo, ref string }{{"ref", "o/r", in.Value}, {"repo", in.Value, "main"}} {
				log := stubGh(t, `{"Name":"n","Version":"v"}`, 0)
				if _, err := gh.Runtime(gh.Config{}).Read(t.TempDir(), gh.PluginJSON(c.repo, c.ref).Desc()); err != nil {
					t.Logf("%s refused: %v", c.what, err)
					continue
				}
				_, args := argv(t, log)
				u, err := url.Parse(args[1])
				if err != nil {
					t.Errorf("%s: endpoint %q does not parse: %v", c.what, args[1], err)
					continue
				}
				// The API resolves dot segments: a path that cleans to another
				// is another endpoint.
				if path.Clean("/"+u.Path) != "/"+u.Path {
					t.Errorf("%s: endpoint path %q resolves to %q", c.what, u.Path, path.Clean("/"+u.Path))
				}
				repo, ok := strings.CutPrefix(u.Path, "repos/")
				repo, ok2 := strings.CutSuffix(repo, tail)
				if got := u.Query()["ref"]; !ok || !ok2 || repo != c.repo || len(got) != 1 || got[0] != c.ref || len(u.Query()) != 1 || u.Fragment != "" {
					t.Errorf("%s: endpoint %q reads as repo %q ref %q (query %v, fragment %q); want repo %q ref %q", c.what, args[1], repo, got, u.Query(), u.Fragment, c.repo, c.ref)
				}
			}
		})
	}
}
