package wf_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestSnapshotRewindRestoresLocalGitIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if !strings.HasPrefix(name, "GIT_") && name != "EMAIL" {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", args[0], err, out)
		}
		return strings.TrimSpace(string(out))
	}

	ws, state := t.TempDir(), t.TempDir()
	runGit(ws, "init", "-q", "-b", "main")
	runGit(ws, "config", "--local", "user.name", "Rewind Fixture")
	runGit(ws, "config", "--local", "user.email", "rewind@example.invalid")
	runGit(ws, "commit", "--allow-empty", "-qm", "base")
	runGit(ws, "switch", "-qc", "topic")
	runGit(ws, "commit", "--allow-empty", "-qm", "topic")
	runGit(ws, "switch", "-q", "main")

	// Use fictitious values and an include outside the workspace. None of
	// these settings belongs in the restored repo or snapshot metadata.
	const canary = "forbidden-snapshot-fixture"
	included := filepath.Join(t.TempDir(), canary)
	if err := os.WriteFile(included, []byte("[credential]\n\tusername = "+canary+"\n[user]\n\tname = "+canary+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(ws, "config", "--local", "include.path", included)
	runGit(ws, "config", "--local", "credential.helper", canary)
	runGit(ws, "config", "--local", "url.https://"+canary+"@example.invalid/.insteadOf", "https://example.invalid/")
	runGit(ws, "config", "--local", "user.signingkey", canary)

	adapters := wf.NewAdapters()
	if err := adapters.Register(gitadapter.Runtime(gitadapter.Config{})); err != nil {
		t.Fatal(err)
	}
	def := snapshotDef()
	engine := wf.NewEngine(wf.FileJournal(state), adapters, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
	}).(*wf.Engine)
	run, err := engine.Run(def, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	checkpoints, seqs := snapshotSeqs(t, state, run)
	if len(checkpoints) == 0 {
		t.Fatal("no recorded checkpoint")
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.Unavailable != "" {
			t.Fatalf("checkpoint unavailable: %s", checkpoint.Unavailable)
		}
		metadata := checkpoint.Adapters["git"]
		if len(metadata) == 0 {
			t.Fatal("checkpoint has no Git metadata")
		}
		if bytes.Contains(metadata, []byte(canary)) || bytes.Contains(metadata, []byte("credential")) {
			t.Fatal("checkpoint captured forbidden Git configuration")
		}
	}
	runGit(ws, "config", "--local", "user.name", "Changed after checkpoint")
	runGit(ws, "config", "--local", "user.email", "changed@example.invalid")
	fork, err := engine.RewindInto(run, strconv.FormatInt(seqs[len(seqs)-1], 10), filepath.Join(t.TempDir(), "rewind"))
	if err != nil {
		t.Fatal(err)
	}
	local := runGit(fork.Workspace, "config", "--local", "--no-includes", "--list")
	for _, forbidden := range []string{canary, "credential.", "url.", "include.", "user.signingkey="} {
		if strings.Contains(local, forbidden) {
			t.Fatal("rewind restored forbidden Git configuration")
		}
	}
	runGit(fork.Workspace, "-c", "user.useConfigOnly=true", "merge", "--no-ff", "-m", "rewound merge", "topic")
	if got := runGit(fork.Workspace, "show", "-s", "--format=%cn <%ce>", "HEAD"); got != "Rewind Fixture <rewind@example.invalid>" {
		t.Errorf("rewound merge committer = %q, want checkpoint identity", got)
	}
}
