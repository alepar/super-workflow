package bd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// This version's workspace and physical-root gates outlive every CLI store.
// See https://github.com/steveyegge/beads/blob/c1c4b642a/cmd/bd/workspace_gate.go
// and internal/workspacegate/gate.go at that revision. Exclusively holding both gates
// makes the embedded database quiescent; a live database is never copied.
const snapshotBDVersion = "bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)"

type snapshotMetadata struct {
	Version int    `json:"version"`
	Tracker string `json:"tracker"`
}

func snapshotEnvironment() error {
	for _, pair := range os.Environ() {
		key, value, _ := strings.Cut(pair, "=")
		if value != "" && wf.SandboxStorageOverride(key) {
			return fmt.Errorf("bd snapshot: environment override %s is unsupported", key)
		}
	}
	return nil
}

// snapshotTracker checks that workspace's tracker can be snapshotted: present
// is false when there is none. config is its config.yaml as checked. bd
// resolves the config's storage settings, which it cannot do under the
// tracker's gates (its database is gated too): a call under the gates passes
// the bytes an earlier call checked, and the file must still hold them.
func (r runtime) snapshotTracker(workspace string, checked []byte) (present bool, config []byte, err error) {
	return r.trackerChecks(workspace, checked)
}

// version is bd's version, asked sandboxed, with a clean environment, in
// an empty directory: a snapshot's probe never starts bd in the candidate
// tracker, where it could open or initialise its store.
func (r runtime) version() (string, error) {
	dir, err := os.MkdirTemp("", "wf-bd-probe-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	r.sandbox = true
	out, err := r.run(dir, "version")
	return strings.TrimSpace(string(out)), err
}

// envStorageOverride is the first storage override (BEADS_*, BD_*, DOLT_*,
// the actor names aside) a .env file sets, or "". It reads KEY=VALUE and
// KEY: VALUE lines, an export prefix followed by any whitespace allowed, as
// gotenv does. A line it cannot parse unambiguously that mentions one of
// those prefixes is refused too (named by its line): fail closed.
func envStorageOverride(data []byte) string {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && strings.ContainsRune(" \t", rune(rest[0])) {
			line = strings.TrimSpace(rest)
		}
		if i := strings.IndexAny(line, "=:"); i >= 0 && envKey.MatchString(strings.TrimSpace(line[:i])) {
			if key := strings.TrimSpace(line[:i]); wf.SandboxStorageOverride(key) {
				return key
			}
			continue
		}
		if envMentionsBD.MatchString(line) {
			return fmt.Sprintf("line %d (unparsed, names a bd setting)", n+1)
		}
	}
	return ""
}

// envKey is a .env key the reader takes as written.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// envMentionsBD is an unparsed .env line that could name a bd setting.
var envMentionsBD = regexp.MustCompile(`(?i)\b(beads|bd|dolt)`)

func (r runtime) trackerChecks(workspace string, checked []byte) (bool, []byte, error) {
	if err := snapshotEnvironment(); err != nil {
		return false, nil, err
	}
	root := filepath.Join(workspace, ".beads")
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil, fmt.Errorf("bd snapshot: tracker must be a local directory")
	}
	if info, err := os.Lstat(filepath.Join(root, "config.yaml")); err != nil || !info.Mode().IsRegular() {
		return false, nil, fmt.Errorf("bd snapshot: regular local config.yaml required to prevent inherited configuration")
	}
	// The tracker's config is data the snapshot carries with the rest of
	// .beads (sync.remote, export, lint, ...). Only a setting that changes
	// where the database lives would attach a restored tracker to storage
	// outside its workspace: it is refused, and named. bd resolves the file
	// (YAML, flow mappings included, keys as bd normalises them), so the
	// check reads the settings bd itself would apply. bd 1.3.1 reads
	// config.yaml only.
	config, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		return false, nil, err
	}
	if checked != nil && !bytes.Equal(config, checked) {
		return false, nil, fmt.Errorf("bd snapshot: config.yaml changed during the snapshot")
	}
	switch key, err := configStorageSetting(config); {
	case err != nil:
		return false, nil, fmt.Errorf("bd snapshot: config.yaml: %v", err)
	case key != "":
		return false, nil, fmt.Errorf("bd snapshot: config.yaml sets %s, which changes where the database lives; snapshots support the local embedded database only", key)
	}
	// bd loads .beads/.env into its environment at startup: a storage
	// override there moves the database as one in the caller's would.
	if env, err := os.ReadFile(filepath.Join(root, ".env")); err == nil {
		if key := envStorageOverride(env); key != "" {
			return false, nil, fmt.Errorf("bd snapshot: .beads/.env sets %s, which changes where the database lives; snapshots support the local embedded database only", key)
		}
	} else if !os.IsNotExist(err) {
		return false, nil, err
	}
	raw, err := os.ReadFile(filepath.Join(root, "metadata.json"))
	if err != nil {
		return false, nil, err
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false, nil, err
	}
	if meta["backend"] != "dolt" || meta["dolt_mode"] != "embedded" {
		return false, nil, fmt.Errorf("bd snapshot: only local embedded Dolt is supported")
	}
	for _, key := range []string{"dolt_server_host", "dolt_server_port", "dolt_data_dir", "dolt_server_database", "dolt_server_user", "dolt_server_socket", "dolt_server_tls", "dolt_team_server", "dolt_remotesapi_port"} {
		if value, ok := meta[key]; ok && value != "" && value != float64(0) && value != false {
			return false, nil, fmt.Errorf("bd snapshot: unsupported storage override %s", key)
		}
	}
	for _, name := range []string{"redirect", "config.json", "dolt-backup.json"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			return false, nil, fmt.Errorf("bd snapshot: unsupported tracker configuration %s", name)
		} else if !os.IsNotExist(err) {
			return false, nil, err
		}
	}
	if info, err := os.Lstat(filepath.Join(root, "embeddeddolt")); err != nil || !info.IsDir() {
		return false, nil, fmt.Errorf("bd snapshot: missing local embedded database")
	}
	v, err := r.version()
	if err != nil {
		return false, nil, err
	}
	if v != snapshotBDVersion {
		return false, nil, fmt.Errorf("bd snapshot: unsupported bd version %q", v)
	}
	// Reject links and special files before creating any artifact. Copying them
	// would leave a restored tracker attached to storage outside its workspace.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("bd snapshot: unsupported tracker file %s", path)
		}
		if d.Name() == "config.json" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var config map[string]any
			if err := json.Unmarshal(data, &config); err != nil {
				return err
			}
			for _, key := range []string{"remotes", "backups"} {
				if value, ok := config[key]; ok && value != nil {
					object, ok := value.(map[string]any)
					if !ok || len(object) != 0 {
						return fmt.Errorf("bd snapshot: configured database %s unsupported", key)
					}
				}
			}
		}
		return nil
	})
	return true, config, err
}

func trackerGates(workspace string) (func(), error) {
	var files []*os.File
	release := func() {
		for i := len(files) - 1; i >= 0; i-- {
			_ = files[i].Close()
		}
	}
	for _, name := range []string{".beads.gate.lock", ".beads/embeddeddolt.gate.lock"} {
		path := filepath.Join(workspace, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			// Snapshots omit native lock files. Recreate the empty gate without
			// replacing a gate another tracker process may already be using.
			f, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if createErr != nil && !os.IsExist(createErr) {
				release()
				return nil, createErr
			}
			if createErr == nil {
				_ = f.Close()
			}
			info, err = os.Lstat(path)
		}
		if err != nil || !info.Mode().IsRegular() {
			release()
			return nil, fmt.Errorf("bd snapshot: native gate unavailable: %s", name)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			release()
			return nil, fmt.Errorf("bd snapshot: gate must belong to current user: %s", name)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			release()
			return nil, err
		}
		files = append(files, f)
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			release()
			return nil, fmt.Errorf("bd snapshot: tracker busy or native locking unavailable: %w", err)
		}
	}
	return release, nil
}

func (r runtime) SnapshotPreflight(req wf.SnapshotRequest) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %v", wf.ErrUnavailable, err)
		}
	}()
	present, _, err := r.snapshotTracker(req.Workspace, nil)
	if err != nil || !present {
		return err
	}
	release, err := trackerGates(req.Workspace)
	if err != nil {
		return err
	}
	release()
	return nil
}

func (r runtime) Snapshot(req wf.SnapshotRequest) (result json.RawMessage, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %v", wf.ErrUnavailable, err)
		}
	}()
	present, config, err := r.snapshotTracker(req.Workspace, nil)
	if err != nil {
		return nil, err
	}
	meta := snapshotMetadata{Version: 1, Tracker: "absent"}
	if present {
		release, err := trackerGates(req.Workspace)
		if err != nil {
			return nil, err
		}
		defer release()
		// Revalidate after gating, closing the preflight-to-capture race.
		if _, _, err := r.snapshotTracker(req.Workspace, config); err != nil {
			return nil, err
		}
		// With a store, the tracker is read in place while the gates are
		// held; without one it is copied into the checkpoint directory.
		if req.Store != nil {
			err = req.Store.PutDir("tracker", filepath.Join(req.Workspace, ".beads"), nativeGateFile)
		} else {
			err = copyTracker(filepath.Join(req.Workspace, ".beads"), filepath.Join(req.Directory, "tracker"))
		}
		if err != nil {
			return nil, err
		}
		meta.Tracker = "embedded"
	}
	return json.Marshal(meta)
}

func (r runtime) Restore(req wf.RestoreRequest, raw json.RawMessage) error {
	if err := snapshotEnvironment(); err != nil {
		return err
	}
	var meta snapshotMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return err
	}
	if meta.Version != 1 || (meta.Tracker != "absent" && meta.Tracker != "embedded") {
		return fmt.Errorf("bd restore: unsupported snapshot metadata")
	}
	if _, err := os.Lstat(filepath.Join(req.Workspace, ".beads")); !os.IsNotExist(err) {
		return fmt.Errorf("bd restore: destination tracker already exists or is inaccessible")
	}
	if meta.Tracker == "absent" {
		return nil
	}
	v, err := r.version()
	if err != nil {
		return err
	}
	if v != snapshotBDVersion {
		return fmt.Errorf("bd restore: unsupported bd version")
	}
	if err := copyTracker(filepath.Join(req.Directory, "tracker"), filepath.Join(req.Workspace, ".beads")); err != nil {
		return err
	}
	return stripRemotes(filepath.Join(req.Workspace, ".beads"), req.Omit)
}

// stripRemotes leaves a fork's restored tracker with no remote, so no bd
// call in the fork, however it is launched, can reach the source's: each
// Dolt database's remotes, backups and branch upstreams are dropped, and
// every remote setting in config.yaml is blanked. Each is passed to omit.
// The source is untouched.
func stripRemotes(root string, omit func(string)) error {
	if omit == nil {
		omit = func(string) {}
	}
	states, err := filepath.Glob(filepath.Join(root, "embeddeddolt", "*", ".dolt", "repo_state.json"))
	if err != nil {
		return err
	}
	for _, path := range states {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var state map[string]json.RawMessage
		if err := json.Unmarshal(raw, &state); err != nil {
			return fmt.Errorf("bd restore: %s: %w", path, err)
		}
		db := filepath.Base(filepath.Dir(filepath.Dir(path)))
		changed := false
		for _, field := range []string{"remotes", "backups", "branches"} {
			var entries map[string]struct {
				URL    string `json:"url"`
				Remote string `json:"remote"`
			}
			if len(state[field]) == 0 || json.Unmarshal(state[field], &entries) != nil || len(entries) == 0 {
				continue
			}
			names := make([]string, 0, len(entries))
			for name := range entries {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				switch e := entries[name]; field {
				case "branches":
					omit(fmt.Sprintf("dolt %s: branch %s upstream %s", db, name, e.Remote))
				default:
					omit(fmt.Sprintf("dolt %s: %s %s (%s)", db, strings.TrimSuffix(field, "s"), name, e.URL))
				}
			}
			state[field] = json.RawMessage("{}")
			changed = true
		}
		if changed {
			out, err := json.MarshalIndent(state, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, out, 0o644); err != nil {
				return err
			}
		}
	}
	config := filepath.Join(root, "config.yaml")
	data, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	stripped, blanked, err := configWithoutRemotes(data)
	if err != nil {
		return fmt.Errorf("bd restore: config.yaml: %v", err)
	}
	for _, key := range blanked {
		omit("config.yaml " + key)
	}
	if len(blanked) > 0 {
		return os.WriteFile(config, stripped, 0o644)
	}
	return nil
}

// nativeGateFile names a tracker's own lock files, which a checkpoint omits.
func nativeGateFile(name string) bool {
	return strings.HasSuffix(name, ".gate.lock") || strings.HasSuffix(name, ".gate.lock.info")
}

// copyTracker copies a tracker tree with every file's and directory's
// permission bits, less group and world write: a tracked hook keeps its exec
// bit, so a restored world's git status matches its source's, and no copy is
// shared-writable. Directories get their mode last, after everything below
// them is written.
func copyTracker(src, dst string) error {
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if nativeGateFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			dirs = append(dirs, dirMode{target, info.Mode().Perm() &^ 0o022})
			return os.MkdirAll(target, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("bd snapshot: unsupported tracker file %s", rel)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(output, input)
		closeErr := output.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return os.Chmod(target, info.Mode().Perm()&^0o022)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}

// SnapshotStamp names the tracker's state as a live capture must find it
// again after it: every tracker file's path, mode and content digest (the
// native gate files left out), read under the tracker's gates. A committed
// write and a working-set write both move it; a read, which rewrites the
// store's modification times but not its bytes, does not. A workspace
// without a tracker stamps as absent.
func (r runtime) SnapshotStamp(req wf.SnapshotRequest) (string, error) {
	present, _, err := r.snapshotTracker(req.Workspace, nil)
	if err != nil || !present {
		return "absent", err
	}
	release, err := trackerGates(req.Workspace)
	if err != nil {
		return "", err
	}
	defer release()
	root := filepath.Join(req.Workspace, ".beads")
	h := sha256.New()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || nativeGateFile(d.Name()) {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := ""
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			fh := sha256.New()
			if _, err := io.Copy(fh, f); err != nil {
				return err
			}
			sum = hex.EncodeToString(fh.Sum(nil))
		}
		fmt.Fprintf(h, "%q %v %s\n", rel, info.Mode(), sum)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
