package wf_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Feature's setup output carries an absolute integration workdir. Restoring its
// checkout alone is insufficient: the reissued agent must bind the restored path.
func TestFeatureSnapshotExternalStateReissuesInsideFork(t *testing.T) {
	t.Run("external-state", func(t *testing.T) { featureSnapshotReissuesInsideFork(t, false) })
	t.Run("internal-state", func(t *testing.T) { featureSnapshotReissuesInsideFork(t, true) })
}

func featureSnapshotReissuesInsideFork(t *testing.T, internal bool) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	workspace, state := t.TempDir(), t.TempDir()
	if internal {
		state = filepath.Join(workspace, ".state")
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Snapshot Test")
	git("config", "user.email", "snapshot@example.invalid")
	git("config", "core.hooksPath", os.DevNull)
	git("config", "commit.gpgsign", "false")
	git("commit", "--allow-empty", "-qm", "fixture")
	if internal {
		if err := os.WriteFile(filepath.Join(workspace, ".git", "info", "exclude"), []byte(".state/\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	adapters := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{gitadapter.Runtime(gitadapter.Config{}), shell.Runtime(shell.Config{})} {
		if err := adapters.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	def := feature.Definition()
	engine := wf.NewEngine(wf.FileJournal(state), adapters, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: workspace, StateRoot: state, Snapshots: true,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
	}).(*wf.Engine)
	run, err := engine.Run(def, map[string]string{"goal": "Add an exported addition function", "suite": "true", "goal-check-command": "go test -count=1 -json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	offered, err := engine.Next(run, "")
	if err != nil {
		t.Fatalf("feature snapshots with external state: %v", err)
	}
	if len(offered) != 1 || offered[0].State != "Design" {
		t.Fatalf("first feature offer: %+v", offered)
	}
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, rec := range records {
		if rec.Kind == wf.JournalWorldSnapshot && rec.State == "Design" {
			seq = rec.Seq
		}
	}
	if seq == 0 {
		t.Fatal("missing Design checkpoint")
	}
	fork, err := engine.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatalf("fork feature with external state: %v", err)
	}
	relativeIntegration, err := filepath.Rel(state, offered[0].Workspace)
	if err != nil || strings.HasPrefix(relativeIntegration, "..") {
		t.Fatalf("source integration not under state: %q, %v", offered[0].Workspace, err)
	}
	expected := filepath.Join(fork.StateDir, relativeIntegration)
	if _, err := os.Stat(filepath.Join(expected, ".git")); err != nil {
		t.Fatalf("restored integration checkout: %v", err)
	}
	restored := wf.NewEngine(wf.FileJournal(fork.StateDir), adapters, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: fork.Workspace, StateRoot: fork.StateDir,
		RunDir: func(run wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(run)) },
	}).(*wf.Engine)
	next, err := restored.Next(run, "")
	if err != nil {
		t.Fatalf("fork continuation failed (source integration %q): %v", offered[0].Workspace, err)
	}
	if len(next) != 1 || next[0].State != "Design" {
		t.Fatalf("fork offer: %+v", next)
	}
	if next[0].Workspace != expected {
		t.Fatalf("restored Design workspace = %q, want %q (source offer used %q)", next[0].Workspace, expected, offered[0].Workspace)
	}

	assertFeatureSnapshotDelivery(t, next[0], fork, workspace, state)
	nextCheckpoint := featureOfferCheckpoint(t, fork.StateDir, run, "Design", "")
	if nextCheckpoint <= fork.Seq {
		t.Fatal("continuing the fork did not capture a new checkpoint")
	}
	typedIntents := featureSnapshotTypedIntents(t, fork.StateDir, run)
	// The second generation must use the first fork's bundle and typed paths,
	// even when both roots of the original world have disappeared.
	for _, root := range []string{workspace, state} {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
	}
	second, err := restored.ForkSnapshot(run, nextCheckpoint, filepath.Join(t.TempDir(), "second"))
	if err != nil {
		t.Fatalf("second generation with original world removed: %v", err)
	}
	for _, root := range []string{fork.Workspace, fork.StateDir} {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
	}
	secondEngine := featureSnapshotEngine(def, adapters, second)
	design := featureSnapshotOffer(t, secondEngine, run, "Design")
	assertFeatureSnapshotDelivery(t, design, second, workspace, state, fork.Workspace, fork.StateDir)
	if design.Workspace != filepath.Join(second.StateDir, relativeIntegration) {
		t.Fatalf("second generation workdir %q", design.Workspace)
	}
	if featureSnapshotTypedIntents(t, second.StateDir, run) != typedIntents {
		t.Fatal("fork lost typed worktree effect intents")
	}
	if featureOfferCheckpoint(t, second.StateDir, run, "Design", "") <= second.Seq {
		t.Fatal("second generation disabled snapshots")
	}
	featureTaskCheckpointRoundtrip(t, def, adapters, secondEngine, second, design, workspace, state, fork.Workspace, fork.StateDir)
}

func featureSnapshotEngine(def *wf.Def, adapters *wf.Adapters, fork wf.SnapshotFork) *wf.Engine {
	return wf.NewEngine(wf.FileJournal(fork.StateDir), adapters, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: fork.Workspace, StateRoot: fork.StateDir,
		RunDir: func(run wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(run)) },
	}).(*wf.Engine)
}

func featureSnapshotOffer(t *testing.T, e *wf.Engine, run wf.RunID, state string) wf.Instruction {
	t.Helper()
	offers, err := e.Next(run, "")
	if err != nil || len(offers) != 1 || offers[0].State != state || offers[0].Kind != wf.InstructionAgent {
		t.Fatalf("offer %s: %+v, %v", state, offers, err)
	}
	return offers[0]
}

func featureOfferCheckpoint(t *testing.T, state string, run wf.RunID, local, instance string) int64 {
	t.Helper()
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, record := range records {
		if record.Kind != wf.JournalWorldSnapshot || record.State != local || record.Instance != instance {
			continue
		}
		var data wf.WorldSnapshotData
		if err = json.Unmarshal(record.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Before != wf.JournalInstructionIssued {
			continue
		}
		if data.Unavailable != "" {
			t.Fatalf("checkpoint unavailable: %s", data.Unavailable)
		}
		seq = record.Seq
	}
	if seq == 0 {
		t.Fatalf("no checkpoint before %s on %s", local, instance)
	}
	return seq
}

func featureSnapshotTypedIntents(t *testing.T, state string, run wf.RunID) int {
	t.Helper()
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var started wf.RunStartedData
	if err = json.Unmarshal(records[0].Data, &started); err != nil || !started.Snapshots {
		t.Fatalf("snapshots not enabled in restored run: %s, %v", records[0].Data, err)
	}
	count := 0
	for _, record := range records {
		if record.Kind == wf.JournalEffectIntent && strings.Contains(string(record.Data), "$wf.runPath") {
			count++
		}
	}
	if count == 0 {
		t.Fatal("worktree effect intent lost typed RunPath")
	}
	return count
}

func assertFeatureSnapshotDelivery(t *testing.T, in wf.Instruction, fork wf.SnapshotFork, oldRoots ...string) {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "$wf.runPath") {
		t.Fatalf("typed path marker leaked to agent: %s", data)
	}
	for _, old := range oldRoots {
		if old != "" && strings.Contains(string(data), old) {
			t.Fatalf("source path %q leaked to restored instruction: %s", old, data)
		}
	}
	rel, err := filepath.Rel(fork.StateDir, in.Workspace)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("agent workspace outside fork state: %q", in.Workspace)
	}
	if _, err = os.Stat(filepath.Join(in.Workspace, ".git")); err != nil {
		t.Fatalf("agent checkout missing: %v", err)
	}
}

func featureTaskCheckpointRoundtrip(t *testing.T, def *wf.Def, adapters *wf.Adapters, e *wf.Engine, source wf.SnapshotFork, design wf.Instruction, oldRoots ...string) {
	t.Helper()
	run := source.Run
	var spec string
	for _, line := range strings.Split(design.Prompt, "\n") {
		if value, ok := strings.CutPrefix(line, "- spec.md: "); ok {
			spec = value
		}
	}
	if spec == "" {
		t.Fatalf("no allocated design artifact: %s", design.Prompt)
	}
	const body = "Export Add(a, b int) int. Task add implements Add; Add(2, 3) returns 5.\n"
	if err := os.MkdirAll(filepath.Dir(spec), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	submit := func(in wf.Instruction, out any, artifacts map[string]string) {
		t.Helper()
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		result, err := e.Submit(run, wf.Submission{Child: in.Child, Token: in.Token, Attempt: in.Attempt, Output: raw, Artifacts: artifacts})
		if err != nil || result.Class != wf.Accepted {
			t.Fatalf("submit %s: %+v %v", in.State, result, err)
		}
	}
	submit(design, feature.Plan{Tasks: []feature.Task{{ID: "add", Title: "Export addition", Files: []string{"calc.go"}, Acceptance: []string{"Add(2, 3) returns 5"}}}}, map[string]string{"spec.md": spec})
	review := featureSnapshotOffer(t, e, run, "DesignReview")
	assertFeatureSnapshotDelivery(t, review, source, oldRoots...)
	if !strings.Contains(review.Prompt, spec) {
		t.Fatalf("DesignReview lacks current spec path: %s", review.Prompt)
	}
	submit(review, feature.ReviewResult{Rows: []feature.ReviewRow{{ID: "g1", Met: true, Evidence: "spec.md specifies exported Add and task add tests Add(2, 3) == 5", Tasks: []string{"add"}}}}, nil)
	task := featureSnapshotOffer(t, e, run, "feature-task/Implement")
	seq := featureOfferCheckpoint(t, source.StateDir, run, "Implement", task.Child)
	// A future source mutation must survive fork creation and must not appear in
	// the historical task checkpoint.
	marker := filepath.Join(task.Workspace, "source-future.txt")
	if err := os.WriteFile(marker, []byte("source only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	taskGitFile := filepath.Join(task.Workspace, ".git")
	pointer, err := os.ReadFile(taskGitFile)
	if err != nil {
		t.Fatal(err)
	}
	taskGitDir := strings.TrimSpace(strings.TrimPrefix(string(pointer), "gitdir: "))
	if !filepath.IsAbs(taskGitDir) {
		taskGitDir = filepath.Join(task.Workspace, taskGitDir)
	}
	paths := []string{filepath.Join(source.StateDir, "runs", string(run), "journal.jsonl"), spec, marker, filepath.Join(source.Workspace, ".git", "HEAD"), filepath.Join(source.Workspace, ".git", "index")}
	paths = append(paths, taskGitFile, filepath.Join(taskGitDir, "HEAD"), filepath.Join(taskGitDir, "index"))
	before := map[string]string{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(b)
	}
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "task-fork"))
	if err != nil {
		t.Fatalf("fork Implement: %v", err)
	}
	restored := featureSnapshotEngine(def, adapters, fork)
	offer := featureSnapshotOffer(t, restored, run, "feature-task/Implement")
	assertFeatureSnapshotDelivery(t, offer, fork, append(oldRoots, source.Workspace, source.StateDir)...)
	var item struct {
		Workdir, Integration string
		Spec                 struct{ Path, Hash string }
	}
	if err = json.Unmarshal([]byte(offer.Inputs["item"]), &item); err != nil {
		t.Fatalf("task input: %s: %v", offer.Inputs["item"], err)
	}
	for label, path := range map[string]string{"workdir": item.Workdir, "integration": item.Integration, "spec": item.Spec.Path} {
		rel, err := filepath.Rel(fork.StateDir, path)
		if err != nil || !filepath.IsAbs(path) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("%s path outside fork state: %q", label, path)
		}
		if _, err = os.Stat(path); err != nil {
			t.Fatalf("%s path missing: %v", label, err)
		}
		if !strings.Contains(offer.Prompt, path) {
			t.Fatalf("task prompt missing %s path %q", label, path)
		}
	}
	if item.Workdir != offer.Workspace {
		t.Fatalf("task input workdir %q disagrees with offer %q", item.Workdir, offer.Workspace)
	}
	if b, err := os.ReadFile(item.Spec.Path); err != nil || string(b) != body || item.Spec.Hash == "" {
		t.Fatalf("fork spec content/hash lost: %q %q %v", b, item.Spec.Hash, err)
	}
	if _, err = os.Stat(filepath.Join(offer.Workspace, "source-future.txt")); !os.IsNotExist(err) {
		t.Fatal("fork captured future source content")
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("source changed at %s: %v", path, err)
		}
	}
	if featureOfferCheckpoint(t, fork.StateDir, run, "Implement", offer.Child) <= fork.Seq {
		t.Fatal("task fork disabled snapshots")
	}
	featureSnapshotTypedIntents(t, fork.StateDir, run)
}
