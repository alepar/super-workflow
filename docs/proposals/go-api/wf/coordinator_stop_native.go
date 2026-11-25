package wf

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
)

const coordinatorStopVerb = "coordinator-stop"

// parseCoordinatorStop examines only the public event metadata. Codex's turn ID
// identifies the turn, not an individual Stop attempt. stop_hook_active is
// descriptive and cannot gate or deduplicate a workflow continuation.
func parseCoordinatorStop(harness string, input io.Reader) (session, nativeTurn string, active bool, err error) {
	if harness != "codex" && harness != "claude" {
		return "", "", false, fmt.Errorf("unsupported coordinator Stop harness %q", harness)
	}
	fields, err := coordinatorHookObject(input)
	if err != nil {
		return "", "", false, fmt.Errorf("decode %s Stop payload: %w", harness, err)
	}
	event, err := coordinatorHookString(fields, "hook_event_name")
	if err != nil {
		return "", "", false, err
	}
	if event != "Stop" {
		return "", "", false, nil
	}
	for _, key := range []string{"agent_id", "agent_type", "agent_transcript_path"} {
		if raw := fields[key]; len(raw) != 0 && !bytes.Equal(raw, []byte("null")) {
			return "", "", false, nil
		}
	}
	session, err = coordinatorHookString(fields, "session_id")
	if err != nil {
		return "", "", false, err
	}
	if strings.TrimSpace(session) == "" {
		return "", "", false, errors.New("Stop payload is missing the native session ID")
	}
	if harness == "codex" {
		nativeTurn, err = coordinatorHookString(fields, "turn_id")
		if err != nil {
			return "", "", false, err
		}
	}
	return session, nativeTurn, true, nil
}

func renderCoordinatorStop(continuation, diagnostic string) ([]byte, error) {
	if continuation == "" && diagnostic == "" {
		return nil, nil
	}
	output := struct {
		Decision      string `json:"decision,omitempty"`
		Reason        string `json:"reason,omitempty"`
		SystemMessage string `json:"systemMessage,omitempty"`
	}{Reason: continuation, SystemMessage: diagnostic}
	if continuation != "" {
		output.Decision = "block"
	}
	var result bytes.Buffer
	encoder := json.NewEncoder(&result)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(output); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

// installCoordinatorStop edits only the explicitly selected project config.
// Native permission and hook-trust review still apply to the installed entry.
func installCoordinatorStop(harness, workspace, executable string, named bool) error {
	var configDir, configName string
	switch harness {
	case "codex":
		configDir, configName = ".codex", "hooks.json"
	case "claude":
		configDir, configName = ".claude", "settings.local.json"
	default:
		return fmt.Errorf("unsupported coordinator Stop harness %q", harness)
	}
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(executable) {
		return errors.New("Stop hook requires absolute workspace and client executable paths")
	}
	workspace = filepath.Clean(workspace)
	executable = filepath.Clean(executable)
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	realExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	if coordinatorHookWithin(workspace, executable) || coordinatorHookWithin(realWorkspace, realExecutable) {
		return errors.New("Stop hook client executable must be installed outside the workspace")
	}
	info, err := os.Stat(realExecutable)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("Stop hook client is not an executable file: %s", realExecutable)
	}
	root, err := os.OpenRoot(realWorkspace)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir(configDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err = root.Lstat(configDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("Stop hook config directory must be a real directory: %s", configDir)
	}
	config, err := root.OpenRoot(configDir)
	if err != nil {
		return err
	}
	defer config.Close()

	// Serialize cooperating installers without leaving another config artifact.
	lock, err := config.Open(".")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock Stop hook config: %w", err)
	}
	if harness == "codex" {
		// A project-layer inline hook can coexist with hooks.json. Refuse a
		// second coordinator registration across those two formats.
		inline, _, err := readCoordinatorHookConfig(config, "config.toml")
		if err != nil {
			return err
		}
		if bytes.Contains(inline, []byte(coordinatorStopVerb)) {
			return errors.New("check .codex/config.toml for an existing coordinator Stop hook before installing")
		}
	}
	if harness == "claude" {
		shared, _, err := readCoordinatorHookConfig(config, "settings.json")
		if err != nil {
			return err
		}
		if bytes.Contains(shared, []byte(coordinatorStopVerb)) {
			return errors.New("check .claude/settings.json for an existing coordinator Stop hook before installing")
		}
	}

	argv := []string{realExecutable}
	if named {
		argv = append(argv, "wf")
	}
	argv = append(argv, coordinatorStopVerb, "--hook", harness, "--workspace", workspace)
	for i, arg := range argv {
		argv[i] = sink.Quote(arg)
		if harness == "claude" {
			// Claude expands its path placeholders before the shell sees
			// quotes. Keep literal placeholder text across itself in quotes.
			argv[i] = strings.ReplaceAll(argv[i], "${CLAUDE_", "$''{CLAUDE_")
		}
	}
	data, mode, err := readCoordinatorHookConfig(config, configName)
	if err != nil {
		return err
	}
	data, err = mergeCoordinatorHookConfig(data, strings.Join(argv, " "))
	if err != nil {
		return fmt.Errorf("merge %s: %w", filepath.Join(configDir, configName), err)
	}
	if data == nil {
		return nil // Already installed.
	}
	tempName := configName + "." + rand.Text()
	temp, err := config.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer config.Remove(tempName)
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return config.Rename(tempName, configName)
}

func coordinatorHookWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// readCoordinatorHookConfig treats a missing config as absent. Existing files
// must be regular and their permissions are retained when replaced.
func readCoordinatorHookConfig(config *os.Root, name string) ([]byte, os.FileMode, error) {
	info, err := config.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("Stop hook config must be a regular file: %s", name)
	}
	data, err := config.ReadFile(name)
	return data, info.Mode().Perm(), err
}

func mergeCoordinatorHookConfig(data []byte, command string) ([]byte, error) {
	// encoding/json writes each invalid UTF-8 byte as U+FFFD, so a command
	// naming a path that is not UTF-8 would be stored as a different path.
	if !utf8.ValidString(command) {
		return nil, fmt.Errorf("Stop hook command %q is not valid UTF-8 (a path in it, such as the workspace, cannot be stored in JSON)", command)
	}
	settings := make(map[string]json.RawMessage)
	if data != nil {
		var err error
		if settings, err = coordinatorHookObject(bytes.NewReader(data)); err != nil {
			return nil, err
		}
	}
	hooks := make(map[string]json.RawMessage)
	if raw, exists := settings["hooks"]; exists {
		var err error
		if hooks, err = coordinatorHookObject(bytes.NewReader(raw)); err != nil {
			return nil, err
		}
	}
	var groups []json.RawMessage
	if raw, exists := hooks["Stop"]; exists {
		if err := json.Unmarshal(raw, &groups); err != nil {
			return nil, err
		}
		if groups == nil {
			return nil, errors.New("Stop hook groups must be an array")
		}
	}
	installed := false
	for _, raw := range groups {
		group, err := coordinatorHookObject(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		var handlers []json.RawMessage
		if err := json.Unmarshal(group["hooks"], &handlers); err != nil {
			return nil, err
		}
		if handlers == nil {
			return nil, errors.New("Stop hook handlers must be an array")
		}
		for _, raw := range handlers {
			handler, err := coordinatorHookObject(bytes.NewReader(raw))
			if err != nil {
				return nil, err
			}
			belongs, err := isCoordinatorStopHandler(handler)
			if err != nil {
				return nil, err
			}
			if !belongs {
				continue
			}
			// Exact existing registration is idempotent. Any other variant
			// needs explicit review before adding a competing command.
			if installed || len(group) != 1 || len(handlers) != 1 || len(handler) != 3 ||
				string(handler["timeout"]) != "2" || string(handler["type"]) != `"command"` {
				return nil, errors.New("ambiguous existing coordinator Stop hook")
			}
			existingCommand, err := coordinatorHookString(handler, "command")
			if err != nil || existingCommand != command {
				return nil, errors.New("existing coordinator Stop command differs from the requested installation")
			}
			installed = true
		}
	}
	if installed {
		return nil, nil
	}
	handler := struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}{"command", command, 2}
	group, err := json.Marshal(map[string]any{"hooks": []any{handler}})
	if err != nil {
		return nil, err
	}
	hooks["Stop"], err = json.Marshal(append(groups, group))
	if err != nil {
		return nil, err
	}
	settings["hooks"], err = json.Marshal(hooks)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(settings, "", "  ")
}

var coordinatorStopCommandWord = regexp.MustCompile(`(^|[[:space:]'"&;|])coordinator-stop($|[[:space:]'"&;|])`)

func isCoordinatorStopHandler(handler map[string]json.RawMessage) (bool, error) {
	for _, key := range []string{"command", "commandWindows", "command_windows"} {
		command, err := coordinatorHookString(handler, key)
		if err != nil {
			return false, err
		}
		if coordinatorStopCommandWord.MatchString(command) {
			return true, nil
		}
	}
	var args []string
	if raw := handler["args"]; raw != nil {
		if err := json.Unmarshal(raw, &args); err != nil {
			return false, err
		}
	}
	for _, arg := range args {
		if arg == coordinatorStopVerb {
			return true, nil
		}
	}
	return false, nil
}

// Decode only the object being inspected so untouched config entries survive.
// Duplicate keys in the inspected object make the merge or event ambiguous.
func coordinatorHookObject(input io.Reader) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(input)
	open, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if open != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name := key.(string) // The decoder enforces object keys here.
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate JSON field %q", name)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields[name] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after JSON object")
	}
	return fields, nil
}

func coordinatorHookString(fields map[string]json.RawMessage, key string) (string, error) {
	var value string
	if raw, exists := fields[key]; exists {
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("decode %q: %w", key, err)
		}
	}
	return value, nil
}
