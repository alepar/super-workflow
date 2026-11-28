package wf

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

func (c *cycle) lastPromptFetch(in *instance) PromptFetchedData {
	for i := len(c.recs) - 1; i >= 0; i-- {
		r := c.recs[i]
		if r.Instance != in.ID || r.State != in.node().name || r.Attempt != in.Attempt {
			continue
		}
		if r.Kind == JournalPromptFetched {
			var d PromptFetchedData
			_ = json.Unmarshal(r.Data, &d)
			return d
		}
	}
	return PromptFetchedData{}
}

// promptShapeHash removes only engine-owned locations and identities. Content
// and user inputs remain significant. Longest replacements win for nested roots.
func (c *cycle) promptShapeHash(in *instance, body string) string {
	replacements := map[string]string{}
	add := func(value, token string) {
		if value != "" && value != "." {
			replacements[value] = token
		}
	}
	add(c.workspace(), "{workspace}")
	add(c.workdir(in), "{workdir}")
	if root, ok := c.scope(in).lookup("assets"); ok {
		add(root, "{assets}")
	}
	if dir := c.resultDir(); dir != "" {
		abs, err := filepath.Abs(dir)
		if err == nil {
			add(abs, "{run-dir}")
		}
	}
	add(string(c.run), "{run}")
	if in.Unit != nil {
		name := in.Unit.Name
		// Canonicalize only the exact commands the engine inserted. Authored text
		// remains significant; different binary locations and CLI spellings do not.
		for _, command := range []struct {
			verb string
			args []string
		}{
			{"done", []string{name}}, {"done", []string{name, "--output", "<file>"}},
			{"validate", []string{name}}, {"validate", []string{name, "--output", "<file>"}},
			{"failed", []string{name, "--reason", "<text>"}},
			{"failed", []string{name, "--class", "blocked", "--reason", "<cause>"}},
		} {
			actual := c.verbLine(command.verb, command.args...)
			canonical := "{command:" + command.verb + " " + strings.Join(command.args, " ") + "}"
			add(actual, canonical)
			for _, claim := range in.Unit.Offered {
				args := append([]string{"--claim", claim}, command.args[1:]...)
				add(c.verbLine(command.verb, args...), canonical)
			}
		}
		if in.Token != "" {
			add(c.commandLine(c.e.opts.SubmitLine(c.run, in.Token)), "{command:submit}")
		}
	}

	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) == len(keys[j]) {
			return keys[i] < keys[j]
		}
		return len(keys[i]) > len(keys[j])
	})
	pairs := make([]string, 0, 2*len(keys))
	for _, key := range keys {
		pairs = append(pairs, key, replacements[key])
	}
	return "sha256:" + hashBytes([]byte(strings.NewReplacer(pairs...).Replace(body)))
}
