package git

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type snapshotTree struct {
	Root                           string `json:",omitempty"`
	Path, Head, Branch, Index, WIP string
	// Included are the declared files (SnapshotRequest.Include) this tree's
	// WIP carries as raw blobs; a restore writes them back byte for byte.
	Included []string `json:",omitempty"`
}
type snapshotMetadata struct {
	Version     int
	Trees       []snapshotTree
	LocalConfig map[string]string `json:",omitempty"`
}

// Carry only the local identity a resumed run needs to create commits.
// Config files and includes can contain credentials or source-specific paths.
var snapshotConfigKeys = [...]string{"user.name", "user.email"}

func (r runtime) snapshotLocalConfig(dir string) (map[string]string, error) {
	config := make(map[string]string)
	for _, key := range snapshotConfigKeys {
		value, exit, err := r.git(dir, "config", "--local", "--no-includes", "--null", "--get", key)
		if exit == 1 { // the local key is absent
			continue
		}
		if err != nil {
			return nil, err
		}
		config[key] = strings.TrimSuffix(value, "\x00")
	}
	return config, nil
}

func snapshotBelow(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// SnapshotPreflight deliberately rejects Git features whose extra state cannot
// be represented by the HEAD/index/worktree trees stored in this checkpoint.
func (r runtime) SnapshotPreflight(req wf.SnapshotRequest) error {
	r.snapshot = true
	fail := func(s string) error { return fmt.Errorf("git snapshot: unsupported layout: %s", s) }
	if !filepath.IsAbs(req.Workspace) || !filepath.IsAbs(req.Directory) {
		return fail("paths must be absolute")
	}
	root, err := filepath.EvalSymlinks(req.Workspace)
	if err != nil {
		return err
	}
	if root != filepath.Clean(req.Workspace) {
		return fail("symlink workspace")
	}
	if filepath.Clean(req.Directory) == root || snapshotBelow(filepath.Join(root, ".git"), req.Directory) {
		return fail("checkpoint overlaps repository metadata")
	}
	for _, rel := range req.Exclude {
		if rel == "." || rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || !snapshotBelow(root, filepath.Join(root, rel)) {
			return fail("unsafe exclusion")
		}
	}
	for _, rel := range req.Include {
		if !snapshotIncludablePath(rel) {
			return fail("unsafe inclusion " + rel)
		}
	}
	if err := snapshotSafeAncestor(req.Directory); err != nil {
		return fail(err.Error())
	}
	if err := snapshotStateRoot(root, req.StateRoot); err != nil {
		return fail(err.Error())
	}
	fi, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil || !fi.IsDir() {
		return fail("workspace must be a primary repository")
	}
	shallow, err := r.must(root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if shallow != "false" {
		return fail("shallow repository")
	}
	trees, err := snapshotRequestTrees(req)
	if err != nil {
		return fail(err.Error())
	}
	// Every requested checkout must be a safe target; one since removed is
	// then left out of the capture (liveRequest).
	removed := r.removedTrees(req)
	allowed, requested := map[string]bool{root: true}, map[string]bool{root: true}
	for _, tree := range trees[1:] {
		p, _ := snapshotTreePath(root, req.StateRoot, tree)
		if err := snapshotLinkedTarget(root, req.StateRoot, req.Directory, p, requested); err != nil {
			return fail(err.Error())
		}
		requested[p] = true
		if removed[p] {
			continue
		}
		resolved, e := filepath.EvalSymlinks(p)
		if e != nil || resolved != p {
			return fail("missing or symlink worktree")
		}
		allowed[p] = true
	}
	listed, err := r.worktrees(root)
	if err != nil {
		return err
	}
	registered := map[string]bool{}
	for _, w := range listed {
		registered[filepath.Clean(w.Path)] = true
	}
	for p := range allowed {
		if !registered[p] {
			return fail("unregistered worktree")
		}
		common, e := r.must(p, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if e != nil {
			return e
		}
		if !samePath(common, filepath.Join(root, ".git")) {
			return fail("foreign worktree")
		}
		if _, e = r.must(p, "rev-parse", "--verify", "HEAD"); e != nil {
			return fail("unborn HEAD")
		}
		for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer", "info/sparse-checkout"} {
			q, e := r.must(p, "rev-parse", "--path-format=absolute", "--git-path", name)
			if e != nil {
				return e
			}
			if _, e = os.Stat(q); e == nil {
				return fail(name)
			} else if !os.IsNotExist(e) {
				return e
			}
		}
		config, code, e := r.git(p, "config", "--get-regexp", `^(filter\.|core\.sparsecheckout|core\.splitindex|extensions\.worktreeconfig)`)
		if e != nil && code != 1 {
			return e
		}
		if strings.TrimSpace(config) != "" {
			return fail("custom filters or index configuration")
		}
		debug, e := r.must(p, "ls-files", "--debug")
		if e != nil {
			return e
		}
		for _, line := range strings.Split(debug, "\n") {
			if _, bits, ok := strings.Cut(line, "\tflags: "); ok && bits != "0" {
				return fail("special index flags (including intent-to-add)")
			}
		}
		flags, e := r.must(p, "ls-files", "-v")
		if e != nil {
			return e
		}
		for _, line := range strings.Split(flags, "\n") {
			if line != "" && line[0] != 'H' {
				return fail("special index flags or conflicts")
			}
		}
		entries, e := r.must(p, "ls-files", "--stage")
		if e != nil {
			return e
		}
		for _, line := range strings.Split(entries, "\n") {
			if strings.HasPrefix(line, "160000 ") {
				return fail("submodules")
			}
		}
		split, e := r.must(p, "rev-parse", "--shared-index-path")
		if e != nil {
			return e
		}
		if split != "" {
			return fail("split index")
		}
	}
	for walkRoot := range allowed {
		// Workspace-owned checkouts keep their relative layout. State-owned
		// checkouts may move to an entirely different root, so their links
		// must remain within that checkout.
		linkRoot := root
		stateOwned := req.StateRoot != "" && snapshotBelow(req.StateRoot, walkRoot)
		if stateOwned {
			linkRoot = walkRoot
		}
		if err := filepath.WalkDir(walkRoot, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			// Requested linked checkouts are scanned separately, including those
			// below an excluded state root.
			if p != walkRoot && allowed[p] && d.IsDir() {
				return filepath.SkipDir
			}
			// A linked checkout's .beads is ordinary content (bd restores only
			// the primary's tracker), so its links are checked like any other.
			var exclusions []string
			if walkRoot == root {
				exclusions = snapshotExclusions(req)
			}
			for _, x := range exclusions {
				if p == filepath.Join(walkRoot, x) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			if d.Name() == ".git" {
				if !allowed[filepath.Dir(p)] {
					return fail("unrequested nested repository")
				}
				if d.IsDir() {
					return filepath.SkipDir
				}
			}
			if d.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(p)
				if err != nil {
					return err
				}
				if filepath.IsAbs(target) || !snapshotBelow(linkRoot, filepath.Join(filepath.Dir(p), target)) {
					return fail("symlink escapes captured workspace: " + p)
				}
				// Resolve chains as well: a lexical in-workspace target may itself
				// point outside. Unresolvable links cannot establish isolation.
				resolved, err := filepath.EvalSymlinks(p)
				if err != nil || !snapshotBelow(linkRoot, resolved) {
					return fail("unresolved or escaping symlink: " + p)
				}
				if !stateOwned && req.StateRoot != "" && snapshotBelow(req.StateRoot, resolved) {
					return fail("symlink crosses into relocatable state root: " + p)
				}
			}
			if d.Name() == ".gitmodules" {
				return fail("submodules")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot commands select only the requested checkout. Inherited Git routing,
// config and object-store overrides must never reconnect a fork to its source.
// Remove all GIT_* variables, then apply only our own per-call overrides.
func (r runtime) snapshotTool(dir string, args, env []string) wf.Tool {
	var unset []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_") {
			unset = append(unset, name)
		}
	}
	return wf.Tool{Bin: r.bin, Args: append([]string{"-C", dir}, args...), Dir: dir, Timeout: r.timeout, UnsetEnv: unset, Env: env}
}

func (r runtime) snapshotGit(dir, index string, args ...string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + index, "GIT_OPTIONAL_LOCKS=0", "GIT_AUTHOR_NAME=Workflow snapshot", "GIT_AUTHOR_EMAIL=snapshot@localhost", "GIT_COMMITTER_NAME=Workflow snapshot", "GIT_COMMITTER_EMAIL=snapshot@localhost"}
	res, err := wf.RunTool(r.snapshotTool(dir, args, env))
	if err != nil {
		return "", err
	}
	if res.Exit != 0 {
		return "", fmt.Errorf("git snapshot %s: %s", strings.Join(args, " "), res.Stderr)
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (r runtime) Snapshot(req wf.SnapshotRequest) (json.RawMessage, error) {
	r.snapshot = true
	// The request as declared is checked first, so every requested target's
	// layout holds; only then are the removed checkouts left out.
	if err := r.SnapshotPreflight(req); err != nil {
		return nil, err
	}
	req = r.liveRequest(req)
	if err := os.MkdirAll(req.Directory, 0700); err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	prefix := "refs/workflow/snapshots/" + hex.EncodeToString(nonce[:])
	config, err := r.snapshotLocalConfig(req.Workspace)
	if err != nil {
		return nil, err
	}
	meta := snapshotMetadata{Version: 2, LocalConfig: config}
	bundleArgs := []string{"bundle", "create", filepath.Join(req.Directory, "repository.bundle"), "--branches", "--tags"}
	trees, _ := snapshotRequestTrees(req) // preflight validated every root and path
	for i, location := range trees {
		dir, _ := snapshotTreePath(req.Workspace, req.StateRoot, location)
		head, err := r.must(dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		branch, exit, err := r.git(dir, "symbolic-ref", "-q", "HEAD")
		if err != nil && exit != 1 {
			return nil, err
		}
		original, err := r.must(dir, "rev-parse", "--path-format=absolute", "--git-path", "index")
		if err != nil {
			return nil, err
		}
		index := filepath.Join(req.Directory, fmt.Sprintf("index-%d", i))
		body, err := os.ReadFile(original)
		if err != nil {
			return nil, err
		}
		if err = os.WriteFile(index, body, 0600); err != nil {
			return nil, err
		}
		call := func(args ...string) (string, error) { return r.snapshotGit(dir, index, args...) }
		staged, err := call("write-tree")
		if err != nil {
			return nil, err
		}
		stagedCommit, err := call("commit-tree", staged, "-p", head, "-m", "Workflow staged snapshot")
		if err != nil {
			return nil, err
		}
		paths, err := r.wipFiles(dir, i, req)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			// A linked checkout's tracked .beads files are staged as tracked
			// only: one replaced by a directory records a deletion and never
			// imports the directory's untracked contents (tracker state).
			kept, tracker := paths[:0], false
			for _, p := range paths {
				if p == ".beads" || strings.HasPrefix(p, ".beads/") {
					tracker = true
				} else {
					kept = append(kept, p)
				}
			}
			paths = kept
			if tracker {
				if _, err = call("--literal-pathspecs", "add", "-u", "--", ".beads"); err != nil {
					return nil, err
				}
			}
		}
		// The client's declared files ride the primary tree even where git
		// ignores them, as raw blobs staged after the ordinary paths; the
		// restore writes them back untracked, byte for byte.
		var included []snapshotIncludedFile
		if i == 0 {
			if included, err = snapshotIncluded(dir, req); err != nil {
				return nil, err
			}
			declared := map[string]bool{}
			for _, f := range included {
				declared[filepath.ToSlash(f.rel)] = true
			}
			kept := paths[:0]
			for _, p := range paths {
				if !declared[p] {
					kept = append(kept, p)
				}
			}
			paths = kept
		}
		if len(paths) > 0 {
			for start := 0; start < len(paths); start += 100 {
				end := start + 100
				if end > len(paths) {
					end = len(paths)
				}
				if _, err = call(append([]string{"--literal-pathspecs", "add", "-A", "--"}, paths[start:end]...)...); err != nil {
					return nil, err
				}
			}
		}

		for _, f := range included {
			raw := filepath.Join(req.Directory, "included-blob")
			if err = os.WriteFile(raw, f.body, 0600); err != nil {
				return nil, err
			}
			blob, err := call("hash-object", "-w", "--no-filters", "--", raw)
			if err != nil {
				return nil, err
			}
			if err = os.Remove(raw); err != nil {
				return nil, err
			}
			mode := "100644"
			if f.exec {
				mode = "100755"
			}
			if _, err = call("update-index", "--add", "--cacheinfo", mode+","+blob+","+filepath.ToSlash(f.rel)); err != nil {
				return nil, err
			}
		}

		// Adapter state is restored by its owner, while the saved staged tree
		// still records its original index entries. bd owns only the
		// primary's tracker: a linked checkout keeps its tracked .beads files.
		if i == 0 {
			if _, err = call(append([]string{"--literal-pathspecs", "rm", "--cached", "-r", "-f", "--ignore-unmatch", "--"}, snapshotExclusions(req)...)...); err != nil {
				return nil, err
			}
		}
		tree, err := call("write-tree")
		if err != nil {
			return nil, err
		}
		wip, err := call("commit-tree", tree, "-p", stagedCommit, "-m", "Workflow worktree snapshot")
		if err != nil {
			return nil, err
		}
		snapshotRef := fmt.Sprintf("%s/%d", prefix, i)
		bundleArgs = append(bundleArgs, snapshotRef)
		if _, err = call("update-ref", snapshotRef, wip); err != nil {
			return nil, err
		}
		if err = os.Remove(index); err != nil {
			return nil, err
		}
		if h := snapshotTreeHook; h != nil {
			h(i)
		}
		entry := snapshotTree{Root: location.Root, Path: location.Path, Head: head, Branch: strings.TrimSpace(branch), Index: staged, WIP: wip}
		for _, f := range included {
			entry.Included = append(entry.Included, filepath.ToSlash(f.rel))
		}
		meta.Trees = append(meta.Trees, entry)
	}
	if _, err := r.must(req.Workspace, bundleArgs...); err != nil {
		return nil, err
	}
	return json.Marshal(meta)
}

func (r runtime) Restore(req wf.RestoreRequest, raw json.RawMessage) error {
	r.snapshot = true
	var meta snapshotMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return err
	}
	if (meta.Version != 1 && meta.Version != 2) || len(meta.Trees) == 0 || meta.Trees[0].Path != "." {
		return fmt.Errorf("git restore: invalid metadata")
	}
	if !filepath.IsAbs(req.Workspace) || !filepath.IsAbs(req.Directory) {
		return fmt.Errorf("git restore: paths must be absolute")
	}
	if err := snapshotStateRoot(req.Workspace, req.StateRoot); err != nil {
		return fmt.Errorf("git restore: %w", err)
	}
	if err := snapshotSafeAncestor(req.Directory); err != nil {
		return fmt.Errorf("git restore: %w", err)
	}
	for _, root := range []string{req.Workspace, req.StateRoot} {
		if root != "" && (snapshotBelow(root, req.Directory) || snapshotBelow(req.Directory, root)) {
			return fmt.Errorf("git restore: destination overlaps checkpoint")
		}
	}
	targets := make([]string, len(meta.Trees))
	allowed := map[string]bool{}
	for i := range meta.Trees {
		t := &meta.Trees[i]
		if meta.Version == 1 {
			if t.Root != "" {
				return fmt.Errorf("git restore: v1 metadata cannot declare roots")
			}
			t.Root = "workspace"
		}
		if i == 0 && t.Root != "workspace" {
			return fmt.Errorf("git restore: primary tree must use workspace root")
		}
		if !isFullSHA(t.Head) || !isFullSHA(t.Index) || !isFullSHA(t.WIP) {
			return fmt.Errorf("git restore: invalid object metadata")
		}
		if t.Branch != "" && !strings.HasPrefix(t.Branch, "refs/heads/") {
			return fmt.Errorf("git restore: invalid branch")
		}
		dir, err := snapshotTreePath(req.Workspace, req.StateRoot, *t)
		if err != nil {
			return fmt.Errorf("git restore: %w", err)
		}
		if i > 0 {
			if t.Path == "." {
				return fmt.Errorf("git restore: linked worktree cannot replace its root")
			}
			if err = snapshotLinkedTarget(req.Workspace, req.StateRoot, req.Directory, dir, allowed); err != nil {
				return fmt.Errorf("git restore: %w", err)
			}
		}
		if err = snapshotNewTarget(dir); err != nil {
			return fmt.Errorf("git restore: %w", err)
		}
		targets[i] = dir
		allowed[dir] = true
	}
	if err := os.MkdirAll(req.Workspace, 0700); err != nil {
		return err
	}
	// A host template can install local config or hooks that were never in
	// the checkpoint. Restore starts from Git's own minimal repository.
	if _, err := r.must(req.Workspace, "init", "-q", "--template="); err != nil {
		return err
	}
	if _, err := r.must(req.Workspace, "fetch", "--no-tags", filepath.Join(req.Directory, "repository.bundle"), "+refs/*:refs/*"); err != nil {
		return err
	}
	for _, key := range snapshotConfigKeys {
		if value, ok := meta.LocalConfig[key]; ok {
			if _, err := r.must(req.Workspace, "config", "--local", "--replace-all", "--", key, value); err != nil {
				return err
			}
		}
	}
	for i, t := range meta.Trees {
		dir := targets[i]
		if i > 0 {
			// The main checkout has now been populated: reject any path it
			// materialized where metadata requested a linked checkout.
			if err := snapshotNewTarget(dir); err != nil {
				return fmt.Errorf("git restore: %w", err)
			}
			if _, err := r.must(req.Workspace, "worktree", "add", "--relative-paths", "--detach", "--no-checkout", dir, t.Head); err != nil {
				return err
			}
		}
		if _, err := r.must(dir, "update-ref", "--no-deref", "HEAD", t.Head); err != nil {
			return err
		}
		if t.Branch != "" {
			if _, err := r.must(dir, "symbolic-ref", "HEAD", t.Branch); err != nil {
				return err
			}
		}
		if _, err := r.must(dir, "read-tree", t.WIP); err != nil {
			return err
		}
		if _, err := r.must(dir, "checkout-index", "--all", "--force"); err != nil {
			return err
		}
		// Declared files are written from their raw blobs: checkout applied
		// the destination's attributes (eol, filters) to them.
		for _, rel := range t.Included {
			if !snapshotIncludablePath(filepath.FromSlash(rel)) {
				return fmt.Errorf("git restore: unsafe included path %q", rel)
			}
			if err := r.restoreIncluded(dir, t.WIP, rel); err != nil {
				return fmt.Errorf("git restore: %s: %w", rel, err)
			}
		}
		if _, err := r.must(dir, "read-tree", t.Index); err != nil {
			return err
		}
	}
	return nil
}

func snapshotExclusions(req wf.SnapshotRequest) []string {
	out := append([]string{".beads"}, req.Exclude...)
	if req.StateRoot != "" && req.StateRoot != req.Workspace && snapshotBelow(req.Workspace, req.StateRoot) {
		rel, _ := filepath.Rel(req.Workspace, req.StateRoot)
		out = append(out, rel)
	}
	if snapshotBelow(req.Workspace, req.Directory) {
		rel, _ := filepath.Rel(req.Workspace, req.Directory)
		out = append(out, rel)
	}
	return out
}

// snapshotSafeAncestor checks existing components even when the destination
// itself does not exist. No path through a symlink is safe to mutate.
func snapshotSafeAncestor(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("path must be clean and absolute")
	}
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err == nil {
			resolved, e := filepath.EvalSymlinks(p)
			if e != nil || resolved != p {
				return fmt.Errorf("symlink path ancestor")
			}
			if p != path && !fi.IsDir() {
				return fmt.Errorf("ancestor is not a directory")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		if filepath.Dir(p) == p {
			return fmt.Errorf("missing path ancestor")
		}
	}
}

func snapshotStateRoot(workspace, state string) error {
	if state == "" {
		return nil
	}
	if err := snapshotSafeAncestor(state); err != nil {
		return fmt.Errorf("state root: %w", err)
	}
	if snapshotBelow(state, workspace) || snapshotBelow(filepath.Join(workspace, ".git"), state) {
		return fmt.Errorf("state root overlaps workspace or repository metadata")
	}
	if fi, err := os.Lstat(state); err == nil && !fi.IsDir() {
		return fmt.Errorf("state root is not a directory")
	}
	return nil
}

func snapshotTreePath(workspace, state string, t snapshotTree) (string, error) {
	root := workspace
	switch t.Root {
	case "workspace":
	case "state":
		root = state
	default:
		return "", fmt.Errorf("unknown tree root %q", t.Root)
	}
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("missing absolute tree root")
	}
	if t.Path == "" || filepath.IsAbs(t.Path) || filepath.Clean(t.Path) != t.Path || !snapshotBelow(root, filepath.Join(root, t.Path)) {
		return "", fmt.Errorf("unsafe worktree path")
	}
	for _, part := range strings.Split(t.Path, string(filepath.Separator)) {
		if part == ".git" {
			return "", fmt.Errorf("worktree path overlaps Git metadata")
		}
	}
	return filepath.Join(root, t.Path), nil
}

// liveRequest is req with the linked worktrees that were removed since the
// engine declared them left out: a capture follows the live worktree list,
// and a checkout whose directory is gone and which git no longer registers
// (a sweep's git worktree remove) is not part of the world. A requested
// checkout that git still registers stays requested, so one missing for any
// other reason still fails the preflight. When the list cannot be read, req
// is returned as it is and the preflight reports why.
func (r runtime) liveRequest(req wf.SnapshotRequest) wf.SnapshotRequest {
	removed := r.removedTrees(req)
	keep := func(root string, paths []string) []string {
		var out []string
		for _, path := range paths {
			p, err := snapshotTreePath(req.Workspace, req.StateRoot, snapshotTree{Root: root, Path: path})
			if err != nil || !removed[p] {
				out = append(out, path)
			}
		}
		return out
	}
	req.Worktrees = keep("workspace", req.Worktrees)
	req.StateWorktrees = keep("state", req.StateWorktrees)
	return req
}

// removedTrees is the set of requested linked checkouts, by path, whose
// directory is gone and which git no longer registers.
func (r runtime) removedTrees(req wf.SnapshotRequest) map[string]bool {
	removed := map[string]bool{}
	listed, err := r.worktrees(req.Workspace)
	if err != nil {
		return removed
	}
	registered := map[string]bool{}
	for _, w := range listed {
		registered[filepath.Clean(w.Path)] = true
	}
	for _, set := range []struct {
		root  string
		paths []string
	}{{"workspace", req.Worktrees}, {"state", req.StateWorktrees}} {
		for _, path := range set.paths {
			p, err := snapshotTreePath(req.Workspace, req.StateRoot, snapshotTree{Root: set.root, Path: path})
			if err != nil || registered[p] {
				continue
			}
			if _, e := os.Lstat(p); errors.Is(e, fs.ErrNotExist) {
				removed[p] = true
			}
		}
	}
	return removed
}

func snapshotRequestTrees(req wf.SnapshotRequest) ([]snapshotTree, error) {
	trees := []snapshotTree{{Root: "workspace", Path: "."}}
	for _, set := range []struct {
		root  string
		paths []string
	}{{"workspace", req.Worktrees}, {"state", req.StateWorktrees}} {
		for _, path := range set.paths {
			t := snapshotTree{Root: set.root, Path: path}
			if path == "." {
				return nil, fmt.Errorf("linked worktree cannot replace its root")
			}
			if _, err := snapshotTreePath(req.Workspace, req.StateRoot, t); err != nil {
				return nil, err
			}
			trees = append(trees, t)
		}
	}
	return trees, nil
}

func snapshotLinkedTarget(workspace, state, checkpoint, path string, allowed map[string]bool) error {
	if allowed[path] {
		return fmt.Errorf("duplicate worktree")
	}
	if snapshotBelow(path, workspace) || snapshotBelow(filepath.Join(workspace, ".git"), path) {
		return fmt.Errorf("worktree overlaps workspace or repository metadata")
	}
	if state != "" && snapshotBelow(path, state) {
		return fmt.Errorf("worktree contains state root")
	}
	if snapshotBelow(path, checkpoint) || snapshotBelow(checkpoint, path) {
		return fmt.Errorf("worktree overlaps checkpoint")
	}
	for other := range allowed {
		if other != workspace && (snapshotBelow(other, path) || snapshotBelow(path, other)) {
			return fmt.Errorf("nested requested worktrees")
		}
	}
	return nil
}

func snapshotNewTarget(path string) error {
	if err := snapshotSafeAncestor(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("worktree target already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SnapshotStamp names everything a capture reads, so a live capture can be
// checked to be one instant of the world: the branch and tag refs (never the
// capture's own snapshot refs), and each captured tree's HEAD, symbolic
// branch, staged entries and the content of every file its work-in-progress
// capture reads (wipFiles). It reads only; the index is not refreshed. A
// change undone within the capture (A to B to A) is not seen.
// wipFiles are the files the capture of tree i reads into its work-in-progress
// tree: tracked and untracked non-ignored ones, without the tracker, and for
// the primary tree without the linked worktrees and the state and checkpoint
// directories. A linked tree's tracked .beads files are its own content (bd
// restores only the primary's tracker); its untracked ones stay out. The
// stamp digests the same set.
func (r runtime) wipFiles(dir string, i int, req wf.SnapshotRequest) ([]string, error) {
	specs := []string{".", ":(top,exclude,literal).beads"}
	if i == 0 {
		for _, w := range append(append([]string{}, req.Worktrees...), snapshotExclusions(req)...) {
			specs = append(specs, ":(top,exclude,literal)"+filepath.ToSlash(w))
		}
	}
	files, _, err := r.git(dir, append([]string{"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	if i > 0 {
		tracked, _, err := r.git(dir, "ls-files", "-z", "--cached", "--", ":(top,literal).beads")
		if err != nil {
			return nil, err
		}
		files += tracked
	}
	if files == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(files, "\x00"), "\x00"), nil
}

// snapshotTreeHook, when set by a test, runs after each tree's capture.
var snapshotTreeHook func(tree int)

func (r runtime) SnapshotStamp(req wf.SnapshotRequest) (string, error) {
	r.snapshot = true
	req = r.liveRequest(req)
	trees, err := snapshotRequestTrees(req)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	config, err := r.snapshotLocalConfig(req.Workspace)
	if err != nil {
		return "", err
	}
	for _, key := range snapshotConfigKeys {
		if value, ok := config[key]; ok {
			fmt.Fprintf(h, "config %s %q\n", key, value)
		}
	}
	refs, err := r.must(req.Workspace, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "refs\x00%s\n", refs)
	for i, location := range trees {
		dir, err := snapshotTreePath(req.Workspace, req.StateRoot, location)
		if err != nil {
			return "", err
		}
		head, err := r.must(dir, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		branch, exit, err := r.git(dir, "symbolic-ref", "-q", "HEAD")
		if err != nil && exit != 1 {
			return "", err
		}
		staged, err := r.must(dir, "ls-files", "--stage", "-z")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s/%s\x00%s\x00%s\x00%x\n", location.Root, location.Path, head, strings.TrimSpace(branch), sha256.Sum256([]byte(staged)))
		files, err := r.wipFiles(dir, i, req)
		if err != nil {
			return "", err
		}
		if i == 0 {
			// The declared files by the capture's own rule: read through no
			// link, regular files only; anything else stamps as ineligible.
			for _, rel := range req.Include {
				body, exec, ok, err := readIncluded(dir, rel)
				if err != nil {
					return "", err
				}
				if ok {
					fmt.Fprintf(h, "include %q %v %x\n", rel, exec, sha256.Sum256(body))
				} else {
					fmt.Fprintf(h, "include %q ineligible\n", rel)
				}
			}
		}
		for _, rel := range files {
			sum, err := snapshotFileDigest(filepath.Join(dir, rel))
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "%q %s\n", rel, sum)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// snapshotIncludedFile is one declared file as the capture read it.
type snapshotIncludedFile struct {
	rel  string
	body []byte
	exec bool
}

// snapshotIncluded reads the request's declared files (SnapshotRequest.Include)
// under dir by readIncluded: one absent, not a regular file, or reached
// through a link is not carried.
func snapshotIncluded(dir string, req wf.SnapshotRequest) ([]snapshotIncludedFile, error) {
	var out []snapshotIncludedFile
	for _, rel := range req.Include {
		body, exec, ok, err := readIncluded(dir, rel)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, snapshotIncludedFile{rel, body, exec})
		}
	}
	return out, nil
}

// readIncluded reads rel under dir as the very regular file at that path
// reached through no link: it is opened within the workspace root without
// following its final component, and then every parent must be a real
// directory (Lstat) and the path must name the opened file (os.SameFile), so a
// component swapped for a link around the open is caught. ok is false when it
// is absent, a link, behind a link, or not a regular file.
func readIncluded(dir, rel string) (body []byte, exec, ok bool, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false, false, err
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, false, false, err
		}
		return nil, false, false, nil // absent, a link, or escaping the root
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, false, err
	}
	if !info.Mode().IsRegular() || !snapshotRealParents(root, rel) {
		return nil, false, false, nil
	}
	if at, err := root.Lstat(rel); err != nil || !os.SameFile(info, at) {
		return nil, false, false, nil
	}
	if body, err = io.ReadAll(f); err != nil {
		return nil, false, false, err
	}
	return body, info.Mode().Perm()&0o111 != 0, true, nil
}

// snapshotRealParents reports whether every parent directory of rel under
// root is a real directory, not a link.
func snapshotRealParents(root *os.Root, rel string) bool {
	parts := strings.Split(rel, string(filepath.Separator))
	cur := ""
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := root.Lstat(cur)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

// restoreIncluded writes the declared file rel from wip's raw blob into the
// restored tree at dir, with the blob's mode, through no link: the restore
// destination is fresh, so a link there is refused, not followed.
func (r runtime) restoreIncluded(dir, wip, rel string) error {
	entry, err := r.must(dir, "ls-tree", "-z", wip, "--", rel)
	if err != nil {
		return err
	}
	meta, _, _ := strings.Cut(strings.TrimSuffix(entry, "\x00"), "\t")
	fields := strings.Fields(meta)
	if len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return fmt.Errorf("not a file blob in the snapshot (%q)", entry)
	}
	res, err := wf.RunTool(r.snapshotTool(dir, []string{"cat-file", "blob", fields[2]}, nil))
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return fmt.Errorf("cat-file: %s", res.Stderr)
	}
	return writeIncluded(dir, rel, []byte(res.Stdout), fields[0] == "100755")
}

// writeIncluded writes body at rel under dir through real directories only:
// each parent is checked (Lstat) and made one level at a time when absent, a
// link or other file there is refused; the bytes go to a private file in that
// parent, which is renamed over rel, so whatever was at rel is replaced whole,
// never truncated or followed; the parents and the result are checked again
// after the rename.
func writeIncluded(dir, rel string, body []byte, exec bool) error {
	perm := os.FileMode(0o644)
	if exec {
		perm = 0o755
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	parts := strings.Split(rel, string(filepath.Separator))
	cur := ""
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := root.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			if err = root.Mkdir(cur, 0o755); err != nil {
				return err
			}
			info, err = root.Lstat(cur)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a %v, not a directory", cur, info.Mode().Type())
		}
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+".restoring-"+hex.EncodeToString(nonce[:]))
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err = out.Write(body); err == nil {
		if err = out.Chmod(perm); err == nil {
			err = out.Sync()
		}
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && !snapshotRealParents(root, rel) {
		err = fmt.Errorf("a parent of %s changed into a link", rel)
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if info, err := root.Lstat(rel); err != nil || !info.Mode().IsRegular() || !snapshotRealParents(root, rel) {
		return fmt.Errorf("%s did not land as a regular file under real directories", rel)
	}
	return nil
}

// snapshotIncludablePath is git's verify_path for a declared file: a clean,
// local, relative path whose every component is a real name (not empty, "."
// or ".."), none naming .git under any filesystem alias (any case, trailing
// dots or spaces, the 8.3 name git~1), and none holding a backslash or NUL.
func snapshotIncludablePath(rel string) bool {
	if rel == "" || !filepath.IsLocal(rel) || filepath.Clean(rel) != rel || strings.ContainsAny(rel, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return false
		}
		// git's is_hfs_dotgit ignores these code points; is_ntfs_dotgit
		// ignores trailing dots and spaces and knows the 8.3 name and streams.
		bare := strings.ToLower(strings.TrimRight(strings.Map(func(r rune) rune {
			if r >= 0x200c && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x206a && r <= 0x206f || r == 0xfeff {
				return -1
			}
			return r
		}, part), ". "))
		if bare == ".git" || bare == "git~1" || strings.HasPrefix(bare, ".git:") {
			return false
		}
	}
	return true
}

// snapshotFileDigest is one captured path's mode and content: a regular
// file's bytes, a link's target, or absent.
func snapshotFileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return "absent", nil
	case err != nil:
		return "", err
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		return "link " + target, err
	case !info.Mode().IsRegular():
		return info.Mode().Type().String(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%v %x", info.Mode().Perm(), h.Sum(nil)), nil
}
