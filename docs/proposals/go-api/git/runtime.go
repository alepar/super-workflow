package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Config configures the git runtime.
type Config struct {
	// Bin is the git executable; "git" when empty.
	Bin string
	// Timeout bounds each call; wf.ToolTimeout when zero.
	Timeout time.Duration
}

// Runtime returns the runtime serving the git observations and effects through
// the git CLI. Relative paths resolve against the workspace of each call. A git
// missing from PATH is unavailable.
func Runtime(c Config) wf.AdapterRuntime {
	if c.Bin == "" {
		c.Bin = "git"
	}
	return runtime{bin: c.Bin, timeout: c.Timeout}
}

type runtime struct {
	snapshot bool
	bin      string
	timeout  time.Duration
}

func (runtime) Ops() []string {
	return []string{
		"git.head", "git.rev_parse", "git.resolve_commit", "git.is_ancestor", "git.merge_base", "git.status", "git.worktree_list",
		"git.worktree_add", "git.worktree_remove", "git.merge", "git.commit",
	}
}

type args struct {
	Worktree, Repo, Ref, A, B string
	Path, Branch, Base        string
	Commit                    string
	TaskRef                   string `json:"task_ref"`
	NoFF                      bool   `json:"no_ff"`
	Recut                     bool
	Into                      string
	Message                   string
	Paths                     []string
	Globs                     []string
}

func decode(m map[string]any) (a args, err error) {
	b, err := json.Marshal(m)
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal(b, &a)
}

func resolve(workspace, p string) string {
	switch {
	case p == "" || p == ".":
		return workspace
	case filepath.IsAbs(p):
		return p
	}
	return filepath.Join(workspace, p)
}

// git runs the CLI in dir, without the inherited variables that would send it
// to another repository (an engine started from a git hook inherits GIT_DIR)
// or change how it reads pathspecs. A non-zero exit is returned as exit with a nil
// error, so callers that read exit codes as data (merge-base --is-ancestor)
// can; the others use must.
func (r runtime) git(dir string, argv ...string) (out string, exit int, err error) {
	tool := wf.Tool{Bin: r.bin, Args: argv, Dir: dir, Timeout: r.timeout, UnsetEnv: append(probeUnset(), pathspecEnv...)}
	if r.snapshot {
		tool = r.snapshotTool(dir, argv, nil)
	}
	res, err := wf.RunTool(tool)
	switch {
	case err != nil:
		return "", 0, err
	case res.Exit != 0:
		return res.Stdout, res.Exit, fmt.Errorf("git %s: exit %d: %s", strings.Join(argv, " "), res.Exit, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, 0, nil
}

// pathspecEnv are the variables that change how git reads every pathspec.
// Inherited, GIT_LITERAL_PATHSPECS=1 would make :(literal) and :(glob) part
// of the path and GIT_GLOB_PATHSPECS=1 would glob a literal path, so the
// adapter's calls run without them; each pathspec says how it matches.
var pathspecEnv = []string{"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS"}

// must runs git and treats a non-zero exit as failure, returning trimmed stdout.
func (r runtime) must(dir string, argv ...string) (string, error) {
	out, _, err := r.git(dir, argv...)
	return strings.TrimSpace(out), err
}

func (r runtime) Read(workspace string, d wf.ObservationDesc) (json.RawMessage, error) {
	a, err := decode(d.Args)
	if err != nil {
		return nil, fmt.Errorf("git.%s: %w", d.Op, err)
	}
	var v any
	switch d.Op {
	case "head":
		v, err = r.must(resolve(workspace, a.Worktree), "rev-parse", "HEAD")
	// A ref is a value from the definition or model output: --end-of-options
	// makes git read it as a revision even when it starts with a dash.
	case "rev_parse":
		v, err = r.must(resolve(workspace, a.Worktree), "rev-parse", "--verify", "--end-of-options", a.Ref)
	case "resolve_commit":
		v, err = r.must(resolve(workspace, a.Worktree), "rev-parse", "--verify", "--end-of-options", a.Ref+"^{commit}")
	case "is_ancestor":
		var exit int
		_, exit, err = r.git(resolve(workspace, a.Worktree), "merge-base", "--is-ancestor", "--end-of-options", a.A, a.B)
		v = err == nil
		if exit == 1 {
			err = nil
		}
	case "merge_base":
		v, err = r.must(resolve(workspace, a.Worktree), "merge-base", "--end-of-options", a.A, a.B)
	case "status":
		v, err = r.status(resolve(workspace, a.Worktree))
	case "worktree_list":
		v, err = r.worktrees(resolve(workspace, a.Repo))
	default:
		return nil, fmt.Errorf("git.%s is not a read", d.Op)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func (r runtime) status(dir string) (StatusResult, error) {
	// -z prints each path as it is, NUL-terminated: without it git C-quotes a
	// path holding a quote, a control character or a non-ASCII byte. Not must:
	// trimming would eat the leading space of " M path".
	out, _, err := r.git(dir, "status", "--porcelain", "-z")
	if err != nil {
		return StatusResult{}, err
	}
	res := StatusResult{Paths: []string{}}
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		res.Paths = append(res.Paths, f[3:])
		// A rename or copy record is followed by its source path as a field
		// of its own; the source changed too, so it is listed after it.
		if strings.ContainsAny(f[:2], "RC") && i+1 < len(fields) {
			i++
			if fields[i] != "" {
				res.Paths = append(res.Paths, fields[i])
			}
		}
	}
	res.Clean = len(res.Paths) == 0
	return res, nil
}

func (r runtime) worktrees(dir string) ([]Worktree, error) {
	// -z ends each attribute with a NUL and each record with one more, so a
	// newline in a worktree path stays in its path instead of starting a
	// phantom record. Not must: trimming would eat a path's trailing space.
	out, _, err := r.git(dir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	list := []Worktree{}
	for _, rec := range strings.Split(out, "\x00\x00") {
		var w Worktree
		for _, line := range strings.Split(rec, "\x00") {
			key, val, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				w.Path = val
			case "HEAD":
				w.Head = val
			case "branch":
				w.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		}
		if w.Path != "" {
			list = append(list, w)
		}
	}
	return list, nil
}

// samePath compares paths through symlinks where they resolve.
func samePath(a, b string) bool {
	norm := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return norm(a) == norm(b)
}

// listed finds the worktree at path in the repo's worktree list.
func (r runtime) listed(workspace, path string) (Worktree, bool, error) {
	wts, err := r.worktrees(workspace)
	if err != nil {
		return Worktree{}, false, err
	}
	for _, w := range wts {
		if samePath(w.Path, path) {
			return w, true, nil
		}
	}
	return Worktree{}, false, nil
}

// addWorktree is the worktree_add effect. The integration worktree holds the
// run's own work and is added without Recut: an existing one is never reset.
func (r runtime) addWorktree(workspace, path string, a args) error {
	// A removed directory's stale registration must not block the add.
	if _, err := r.must(workspace, "worktree", "prune"); err != nil {
		return err
	}
	w, found, err := r.listed(workspace, path)
	if err != nil {
		return err
	}
	if found {
		if w.Branch != a.Branch {
			return fmt.Errorf("git.worktree_add: %s is a worktree on branch %q, not %q", path, w.Branch, a.Branch)
		}
		if !a.Recut {
			return nil // already there: never recut or reset it
		}
		out, err := r.must(path, "status", "--porcelain")
		if err != nil {
			return err
		}
		if out == "" {
			return nil
		}
		// Partial work of an earlier attempt: start over from base. No -x:
		// ignored build output is kept.
		if _, err := r.must(path, "reset", "--hard", a.Base); err != nil {
			return err
		}
		_, err = r.must(path, "clean", "-fd")
		return err
	}
	if _, serr := os.Lstat(path); serr == nil {
		if !a.Recut {
			return fmt.Errorf("git.worktree_add: %s exists and is not a worktree on %q", path, a.Branch)
		}
		// Only a stale worktree checkout (a .git file) may be removed.
		if fi, gerr := os.Lstat(filepath.Join(path, ".git")); gerr != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("git.worktree_add: %s exists and is not a worktree checkout; remove it first", path)
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	// Exit 1 means the branch does not exist; any other failure is real.
	_, exit, err := r.git(workspace, "rev-parse", "--verify", "--quiet", "refs/heads/"+a.Branch)
	if exit != 1 && err != nil {
		return err
	}
	switch {
	case exit == 1:
		_, err = r.must(workspace, "worktree", "add", "--relative-paths", "-b", a.Branch, path, a.Base)
	case a.Recut:
		_, err = r.must(workspace, "worktree", "add", "--relative-paths", "-B", a.Branch, path, a.Base)
	default:
		_, err = r.must(workspace, "worktree", "add", "--relative-paths", path, a.Branch)
	}
	return err
}

// ApplyAt and CheckAt are Apply and Check: every git op here acts on the
// local repository and worktrees only (none fetches or pushes), so a
// sandboxed run (wf.EffectContext.Sandboxed) performs them as any run does.
func (r runtime) ApplyAt(workspace string, _ wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, error) {
	return r.Apply(workspace, e)
}

func (r runtime) CheckAt(workspace string, _ wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, error) {
	return r.Check(workspace, e)
}

func (r runtime) Apply(workspace string, e wf.ExternalEffect) (json.RawMessage, error) {
	a, err := decode(e.Args)
	if err != nil {
		return nil, fmt.Errorf("git.%s: %w", e.Op, err)
	}
	var v any
	switch e.Op {
	case "worktree_add":
		path := resolve(workspace, a.Path)
		if err = r.addWorktree(workspace, path, a); err == nil {
			v = path
		}
	case "worktree_remove":
		path := resolve(workspace, a.Path)
		_, found, lerr := r.listed(workspace, path)
		if lerr != nil {
			return nil, lerr
		}
		if !found {
			return json.Marshal(path)
		}
		// Never --force: a worktree with uncommitted work fails instead.
		if _, err = r.must(workspace, "worktree", "remove", path); err == nil {
			v = path
		}
	case "merge":
		if err := mergeCommitErr(a); err != nil {
			return nil, err
		}
		dir := resolve(workspace, a.Worktree)
		// A merge already in progress is someone else's (Check aborts only
		// one carrying this effect's ownership marker): never start over it,
		// never abort it.
		if r.midMerge(dir) {
			return nil, fmt.Errorf("git.merge: %s already has a merge in progress (MERGE_HEAD); finish or abort it first", dir)
		}
		if err := r.onTarget(dir, a.Into); err != nil {
			return nil, err
		}
		// The marker proves the merge about to start is this effect's: Check
		// aborts a half-done merge only when it finds it (a crash between the
		// merge and its abort).
		// Compared before the merge, so a conflicting merge reports it too.
		pm, perr := r.postDoneMutation(dir, a.TaskRef, a.Commit)
		if perr != nil {
			return nil, perr
		}
		res := mergeResult{PostDoneMutationReport: wf.PostDoneMutationReport{PostDoneMutation: pm}}
		marker, merr := r.ownerMarker(dir)
		if merr != nil {
			return nil, merr
		}
		if err := os.WriteFile(marker, []byte(e.KeyID()+"\n"), 0o644); err != nil {
			return nil, fmt.Errorf("git.merge: record ownership: %w", err)
		}
		argv := []string{"merge", "--no-edit"}
		if a.NoFF {
			argv = append(argv, "--no-ff")
		}
		if _, err = r.must(dir, append(argv, a.Commit)...); err == nil {
			res.Head, err = r.must(dir, "rev-parse", "HEAD")
		} else if aerr := r.abortMerge(dir); aerr != nil {
			err = fmt.Errorf("%w; %w", err, aerr)
		}
		if !r.midMerge(dir) {
			_ = os.Remove(marker) // the merge is over; a failed abort keeps it for Check
		}
		if err != nil {
			if res.PostDoneMutation == nil {
				return nil, err
			}
			raw, merr := json.Marshal(res)
			if merr != nil {
				return nil, err
			}
			return raw, err
		}
		v = res
	case "commit":
		v, err = r.commit(resolve(workspace, a.Worktree), e.KeyID(), a)
	default:
		return nil, fmt.Errorf("git.%s is not an effect", e.Op)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// midMerge reports whether dir is in the middle of a merge (MERGE_HEAD exists).
func (r runtime) midMerge(dir string) bool {
	_, exit, err := r.git(dir, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	return exit == 0 && err == nil
}

// ownerMarkerName is the file, under the worktree's git dir, that a merge
// started by Apply carries while it runs: the effect key.
const ownerMarkerName = "workflow-merge-owner"

// ownerMarker is the path of dir's ownership marker.
func (r runtime) ownerMarker(dir string) (string, error) {
	p, err := r.must(dir, "rev-parse", "--git-path", ownerMarkerName)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}

// owns reports whether the merge in progress in dir carries the ownership
// marker of the effect keyed key.
func (r runtime) owns(dir, key string) bool {
	marker, err := r.ownerMarker(dir)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(marker)
	return err == nil && key != "" && strings.TrimSpace(string(b)) == key
}

// onTarget fails unless dir has into checked out; an empty into accepts any
// branch (a worktree the run cut, whose branch is its own).
func (r runtime) onTarget(dir, into string) error {
	if into == "" {
		return nil
	}
	cur, _, err := r.git(dir, "symbolic-ref", "-q", "--short", "HEAD")
	if cur = strings.TrimSpace(cur); err != nil || cur != into {
		if cur == "" {
			cur = "a detached HEAD"
		}
		return fmt.Errorf("git.merge: %s has %s checked out, not %s; check out %s first", dir, cur, into, into)
	}
	return nil
}

// abortMerge leaves dir out of a half-done merge: a conflicting merge must not
// wedge the worktree, so `git merge --abort` runs when MERGE_HEAD exists. Call
// it only for a merge this runtime started or can prove is its own.
func (r runtime) abortMerge(dir string) error {
	if _, exit, _ := r.git(dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); exit != 0 {
		return nil
	}
	if _, err := r.must(dir, "merge", "--abort"); err != nil {
		return fmt.Errorf("abort the failed merge: %w", err)
	}
	return nil
}

// commit stages the named paths and globs (all tracked changes when there
// are none), and commits them with the key trailer. Nothing staged is a
// no-op. It returns HEAD.
//
// A path names one file: the :(literal) magic keeps git from reading it as a
// glob or another magic pathspec, which would commit every file it matches.
// A glob is matched as one, by :(glob), only because the definition asked
// for it with CommitGlob. The message is recorded verbatim: git's default
// cleanup would trim its whitespace and collapse its blank lines.
func (r runtime) commit(dir, key string, a args) (string, error) {
	add := []string{"add", "-u"}
	var only []string
	if len(a.Paths)+len(a.Globs) > 0 {
		specs := make([]string, 0, len(a.Paths)+len(a.Globs))
		for _, p := range a.Paths {
			specs = append(specs, ":(literal)"+p)
		}
		for _, g := range a.Globs {
			specs = append(specs, ":(glob)"+g)
		}
		add = append([]string{"add", "--"}, specs...)
		only = append([]string{"--"}, specs...)
	}
	if _, err := r.must(dir, add...); err != nil {
		return "", err
	}
	_, exit, err := r.git(dir, append([]string{"diff", "--cached", "--quiet"}, only...)...)
	if exit == 0 && err == nil {
		return r.must(dir, "rev-parse", "HEAD")
	}
	if exit != 1 {
		return "", err
	}
	if _, err := r.must(dir, append([]string{"commit", "-q", "--cleanup=verbatim", "-m", commitMessage(a.Message, key)}, only...)...); err != nil {
		return "", err
	}
	return r.must(dir, "rev-parse", "HEAD")
}

const trailer = "Effect-Key: "

// commitMessage is the message a commit effect writes: the message, then,
// for a keyed effect, a blank line and its key trailer.
func commitMessage(message, key string) string {
	if key == "" {
		return message
	}
	return message + "\n\n" + trailer + key
}

// Check implements each effect's already_done from its descriptor comment:
//   - worktree_add: path is already listed by `git worktree list` on branch.
//   - worktree_remove: path is absent from `git worktree list`.
//   - merge: `git merge-base --is-ancestor <branch> HEAD` succeeds in worktree,
//     with into (when set) checked out: on another branch it is not done. A
//     worktree found mid-merge with this effect's ownership marker (a crash
//     between merge and abort) is aborted and answers not done; any other
//     merge in progress, even of the same branch, is left alone and answers
//     not done (Apply refuses).
//   - commit: a commit on HEAD (within the last 50) whose whole message is
//     the one this effect writes (commitMessage), so a message that only
//     quotes the trailer line is not it; an effect with no key cannot be
//     found, so it is unknown.
func (r runtime) Check(workspace string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	a, err := decode(e.Args)
	if err != nil {
		return wf.EffectUnknown, fmt.Errorf("git.%s: %w", e.Op, err)
	}
	switch e.Op {
	case "worktree_add":
		path := resolve(workspace, a.Path)
		w, found, err := r.listed(workspace, path)
		done := found && w.Branch == a.Branch
		if done && a.Recut {
			// A reissued unit's worktree is done only when usable: its
			// directory exists and holds no partial work.
			out, serr := r.must(path, "status", "--porcelain")
			done = serr == nil && out == ""
		}
		return doneIf(done, err)
	case "worktree_remove":
		_, found, err := r.listed(workspace, resolve(workspace, a.Path))
		return doneIf(!found, err)
	case "merge":
		dir := resolve(workspace, a.Worktree)
		if r.midMerge(dir) {
			if r.owns(dir, e.KeyID()) {
				if err := r.abortMerge(dir); err != nil {
					return wf.EffectUnknown, err
				}
				if marker, err := r.ownerMarker(dir); err == nil {
					_ = os.Remove(marker)
				}
			}
			return wf.EffectNotDone, nil
		}
		if mergeCommitErr(a) != nil || r.onTarget(dir, a.Into) != nil {
			return wf.EffectNotDone, nil // Apply refuses
		}
		_, exit, err := r.git(dir, "merge-base", "--is-ancestor", a.Commit, "HEAD")
		if exit == 1 {
			return wf.EffectNotDone, nil
		}
		return doneIf(err == nil, err)
	case "commit":
		if e.KeyID() == "" {
			return wf.EffectUnknown, nil
		}
		// -z ends each raw message with a NUL; git stores a -m message
		// with one newline added.
		out, _, err := r.git(resolve(workspace, a.Worktree), "log", "-z", "--format=%B", "-n", "50")
		if err != nil {
			return wf.EffectUnknown, err
		}
		for _, body := range strings.Split(out, "\x00") {
			for _, key := range e.KeyIDs() { // before part escaping too
				if body == commitMessage(a.Message, key)+"\n" {
					return wf.EffectDone, nil
				}
			}
		}
		return wf.EffectNotDone, nil
	}
	return wf.EffectUnknown, nil
}

func doneIf(done bool, err error) (wf.EffectStatus, error) {
	switch {
	case err != nil:
		return wf.EffectUnknown, err
	case done:
		return wf.EffectDone, nil
	}
	return wf.EffectNotDone, nil
}

// mergeResult is the merge's Apply result: the worktree's HEAD after the merge
// and the post_done_mutation report, when the task ref moved.
type mergeResult struct {
	Head string
	wf.PostDoneMutationReport
}

// mergeCommitErr refuses a merge commit git could read as an option, and a
// task merge (task_ref set) whose commit is not a full SHA: the merge lands
// exactly the commit that was checked and never re-resolves a name.
func mergeCommitErr(a args) error {
	switch {
	case a.Commit == "" || strings.HasPrefix(a.Commit, "-"):
		return fmt.Errorf("git.merge: commit %q is not a commit name", a.Commit)
	case a.TaskRef != "" && !isFullSHA(a.Commit):
		return fmt.Errorf("git.merge: commit %q is not a full commit SHA", a.Commit)
	}
	return nil
}

// postDoneMutation compares taskRef's target with commit in dir and returns
// the event to journal when they differ (a ref that no longer resolves, a
// plain exit 1 from rev-parse --quiet, has an empty RefTarget). It returns nil
// when taskRef is empty or commit names no commit (the merge then fails on its
// own). Any other failure to read the task ref is an error wrapping
// wf.ErrUnavailable: the comparison is never guessed.
func (r runtime) postDoneMutation(dir, taskRef, commit string) (*wf.PostDoneMutationData, error) {
	if taskRef == "" {
		return nil, nil
	}
	want, err := r.must(dir, "rev-parse", "--verify", "--end-of-options", commit+"^{commit}")
	if errors.Is(err, wf.ErrUnavailable) {
		return nil, err
	} else if err != nil {
		return nil, nil
	}
	out, exit, err := r.git(dir, "rev-parse", "--verify", "--quiet", "--end-of-options", taskRef+"^{commit}")
	got := strings.TrimSpace(out)
	switch {
	case exit == 1:
		got = ""
	case errors.Is(err, wf.ErrUnavailable):
		return nil, fmt.Errorf("git.merge: read task ref %s: %w", taskRef, err)
	case err != nil:
		return nil, fmt.Errorf("%w: git.merge: read task ref %s: %v", wf.ErrUnavailable, taskRef, err)
	}
	if got == want {
		return nil, nil
	}
	return &wf.PostDoneMutationData{TaskRef: taskRef, RefTarget: got, Commit: want}, nil
}

// RelocateWorkspace repairs legacy absolute worktree links and makes later
// moves independent of their original location. Only worktrees inside the
// relocated workspace belong to this move; external worktrees stay external.
func (r runtime) RelocateWorkspace(from, to string) error {
	info, err := os.Stat(filepath.Join(to, ".git"))
	if os.IsNotExist(err) {
		return nil
	} // No Git world in this workspace.
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		// A linked workspace cannot run Git until its own pointer is repaired.
		// Use the recorded link, rebased by the move's relative displacement
		// only if its old target disappeared; never scan for another repository.
		data, err := os.ReadFile(filepath.Join(to, ".git"))
		if err != nil {
			return err
		}
		link, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
		if !ok {
			return fmt.Errorf("invalid worktree gitfile in %s", to)
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(to, link)
		}
		if _, err := os.Stat(link); os.IsNotExist(err) {
			rel, err := filepath.Rel(from, link)
			if err != nil {
				return err
			}
			link = filepath.Join(to, rel)
		}
		data, err = os.ReadFile(filepath.Join(link, "commondir"))
		if err != nil {
			return fmt.Errorf("locate moved worktree repository: %w", err)
		}
		common := strings.TrimSpace(string(data))
		if !filepath.IsAbs(common) {
			common = filepath.Join(link, common)
		}
		if _, err := r.must(to, "--git-dir", common, "worktree", "repair", "--relative-paths", to); err != nil {
			return err
		}
	}
	worktrees, err := r.worktrees(to)
	if err != nil {
		return err
	}
	argv := []string{"worktree", "repair", "--relative-paths"}
	for _, wt := range worktrees {
		rel, err := filepath.Rel(from, wt.Path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		path := filepath.Join(to, rel)
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			return fmt.Errorf("repair relocated worktree %s: %w", path, err)
		}
		argv = append(argv, path)
	}
	_, err = r.must(to, argv...)
	return err
}
