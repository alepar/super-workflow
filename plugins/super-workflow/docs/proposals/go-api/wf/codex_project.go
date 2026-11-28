package wf

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// CodexProjectRule is the Codex execpolicy rule for the binary a client
// installs into repo at InstalledBinary: file is where Codex loads it as
// repo's project rules (repo/.codex/rules), and rule lets that binary's spawn
// verb run outside Codex's sandbox. The rule names the binary's absolute
// path, so it holds for this checkout only, and Codex loads it only for a
// session inside repo while Codex trusts repo. ok is false when cfg names no
// InstalledBinary.
func (cfg MainConfig) CodexProjectRule(repo string) (file, rule string, ok bool, err error) {
	if cfg.InstalledBinary == "" {
		return "", "", false, nil
	}
	repo, err = filepath.Abs(repo)
	if err != nil {
		return "", "", false, err
	}
	bin := filepath.Join(repo, filepath.FromSlash(cfg.InstalledBinary))
	return filepath.Join(repo, ".codex", "rules", codexRulesName), codexRule(spawnPrefixFor(cfg, bin)), true, nil
}

// codexProjectLayers are the project rules directories Codex loads for a
// session in cwd, as Codex documents its project config layers: the
// .codex/rules of every directory from cwd's project root (its nearest
// ancestor holding .git, a directory or a linked worktree's file; cwd itself
// when none does) down to cwd. trusted is whether Codex trusts root: it
// skips every project layer of a project it does not trust.
func codexProjectLayers(env harness.Env, cwd string) (dirs []string, root string, trusted bool) {
	if cwd == "" {
		return nil, "", false
	}
	root = cwd
	for d := cwd; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			root = d
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	for d := cwd; ; d = filepath.Dir(d) {
		dirs = append([]string{filepath.Join(d, ".codex", "rules")}, dirs...)
		if d == root || filepath.Dir(d) == d {
			break
		}
	}
	return dirs, root, codexTrusts(env, root)
}

// codexTrusts is whether the Codex config marks root trusted
// ([projects."<root>"] trust_level = "trusted"). Codex keys a linked git
// worktree's trust by its main checkout too, so a trusted main checkout
// trusts its worktrees. Trust is per project root: a trusted parent
// directory does not trust the projects below it.
func codexTrusts(env harness.Env, root string) bool {
	raw, err := os.ReadFile(filepath.Join(codexHome(env), "config.toml"))
	if err != nil {
		return false
	}
	trusted := map[string]bool{}
	for _, p := range trustedProjects(string(raw)) {
		trusted[canonicalPath(p)] = true
	}
	for _, cand := range []string{root, mainCheckout(root)} {
		if cand != "" && trusted[canonicalPath(cand)] {
			return true
		}
	}
	return false
}

var (
	projectsHeader = regexp.MustCompile(`^\[\s*projects\s*\.\s*("(?:[^"\\]|\\.)*"|'[^']*')\s*\]\s*(?:#.*)?$`)
	trustedLine    = regexp.MustCompile(`^trust_level\s*=\s*("trusted"|'trusted')\s*(?:#.*)?$`)
)

// trustedProjects are the paths config, a Codex config.toml, marks trusted,
// in the table form Codex writes: a [projects."<path>"] header followed by
// trust_level = "trusted".
func trustedProjects(config string) []string {
	var out []string
	cur := ""
	for _, l := range strings.Split(config, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			cur = ""
			if m := projectsHeader.FindStringSubmatch(l); m != nil {
				cur = unquoteTOMLKey(m[1])
			}
			continue
		}
		if cur != "" && trustedLine.MatchString(l) {
			out = append(out, cur)
		}
	}
	return out
}

// unquoteTOMLKey unquotes a TOML basic ("...") or literal ('...') key.
func unquoteTOMLKey(k string) string {
	if strings.HasPrefix(k, "'") {
		return strings.Trim(k, "'")
	}
	if s, err := strconv.Unquote(k); err == nil {
		return s
	}
	return ""
}

// mainCheckout is the main checkout of the linked git worktree at root (its
// .git a file naming the worktree's git directory), "" when root is none.
func mainCheckout(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	common, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(gitdir, dir)
	}
	if dir = filepath.Clean(dir); filepath.Base(dir) != ".git" {
		return ""
	}
	return filepath.Dir(dir)
}

// canonicalPath is p with its symlinks resolved, p itself when it cannot be.
func canonicalPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
