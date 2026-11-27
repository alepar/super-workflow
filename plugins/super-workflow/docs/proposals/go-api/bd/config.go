package bd

import (
	"bytes"
	"fmt"
	"strings"
)

// storageSettings are the config.yaml keys (bd's global flags and its Dolt
// connection keys) that change where the database lives, each with the
// values that leave it in place. A key not listed is carried as data.
var storageSettings = map[string][]string{
	"db":                 nil,
	"database":           nil,
	"global":             {"false"},
	"no-db":              {"false"},
	"dolt.host":          nil,
	"dolt.port":          {"0"},
	"dolt.socket":        nil,
	"dolt.user":          nil,
	"dolt.mode":          {"embedded"},
	"dolt.shared-server": {"false"},
	"dolt.server":        {"false"},
	"dolt.data-dir":      nil,
}

// configSetting is one key of a config.yaml as the line reader resolved it:
// a dotted, lower-cased path ("dolt.host" for host under dolt:), its value
// with quotes and a trailing comment removed, and its line.
type configSetting struct {
	path, value string
	line        int
}

// readConfig resolves a bd config.yaml without running bd (which would
// open the candidate store): YAML block mappings, a key per line, nested by
// indentation, comments and blank lines skipped, keys lower-cased as bd's
// viper does. Syntax it cannot resolve (flow collections, anchors, aliases,
// merge keys, tags, complex keys, lists, several documents) is refused when
// it sits where a storage or remote setting could be (configSensitive), and
// is carried as data elsewhere.
func readConfig(data []byte) ([]configSetting, error) {
	type level struct {
		indent int
		path   string
	}
	var stack []level
	var out []configSetting
	last := ""
	// A leading byte order mark is not part of the first key.
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "---" || trimmed == "..." || strings.HasPrefix(trimmed, "--- ") || strings.HasPrefix(trimmed, "%") {
			return nil, fmt.Errorf("line %d: several YAML documents or a directive", n+1)
		}
		indent := len(line) - len(trimmed)
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := ""
		if len(stack) > 0 {
			parent = stack[len(stack)-1].path
		}
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" || strings.HasPrefix(trimmed, "? ") || trimmed == "?" ||
			strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "&") ||
			strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "!") {
			if parent == "" || configSensitive(parent) {
				return nil, fmt.Errorf("line %d: YAML the snapshot cannot check under %q", n+1, parent)
			}
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			// A continued plain scalar: refused under a setting it could
			// extend, since blanking the setting would leave it behind.
			if last != "" && configSensitive(last) {
				return nil, fmt.Errorf("line %d: %s continues over several lines", n+1, last)
			}
			continue
		}
		key = strings.TrimSpace(key)
		// A quoted key with an escape (\ in double quotes, '' in single)
		// is refused rather than decoded: fail closed.
		if (strings.HasPrefix(key, `"`) && strings.Contains(key, `\`)) || (len(key) > 1 && strings.HasPrefix(key, "'") && strings.Contains(key[1:len(key)-1], "''")) {
			return nil, fmt.Errorf("line %d: a quoted key with an escape, which the snapshot does not decode", n+1)
		}
		key = strings.ToLower(strings.Trim(key, `"'`))
		value = strings.TrimSpace(value)
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		path := key
		if parent != "" {
			path = parent + "." + key
		}
		if key == "<<" || strings.ContainsAny(value[:min(len(value), 1)], "{[&*!|>") {
			if key == "<<" || configSensitive(path) {
				return nil, fmt.Errorf("line %d: %s uses YAML the snapshot cannot check (flow, anchor, alias, tag, merge or block scalar)", n+1, path)
			}
		}
		last = path
		value = strings.Trim(value, `"'`)
		stack = append(stack, level{indent: indent, path: path})
		out = append(out, configSetting{path: path, value: value, line: n})
	}
	return out, nil
}

// configSensitive is a path a storage or remote setting can be at or under.
func configSensitive(path string) bool {
	first, _, _ := strings.Cut(path, ".")
	switch first {
	case "db", "database", "global", "no-db", "dolt", "sync":
		return true
	}
	return configRemote(path)
}

// configRemote is a key naming a remote: any segment mentions remote or url.
func configRemote(path string) bool {
	return strings.Contains(path, "remote") || strings.Contains(path, "url")
}

// configStorageSetting is the first setting in a bd config.yaml that
// changes where the database lives, or "" when there is none; an error is
// YAML the line reader cannot check where such a setting could be.
func configStorageSetting(data []byte) (string, error) {
	settings, err := readConfig(data)
	if err != nil {
		return "", err
	}
	for _, s := range settings {
		harmless, listed := storageSettings[s.path]
		if !listed || s.value == "" {
			continue
		}
		allowed := false
		for _, h := range harmless {
			allowed = allowed || strings.EqualFold(s.value, h)
		}
		if !allowed {
			return s.path, nil
		}
	}
	return "", nil
}

// configWithoutRemotes is data with every remote setting (sync.remote, a
// remote URL) blanked to "", for a fork's restored tracker, and the paths
// it blanked.
func configWithoutRemotes(data []byte) ([]byte, []string, error) {
	settings, err := readConfig(data)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(string(data), "\n")
	var blanked []string
	for _, s := range settings {
		if !configRemote(s.path) || s.value == "" {
			continue
		}
		raw := lines[s.line]
		colon := strings.Index(raw, ":")
		lines[s.line] = raw[:colon+1] + ` ""`
		blanked = append(blanked, s.path)
	}
	return []byte(strings.Join(lines, "\n")), blanked, nil
}
