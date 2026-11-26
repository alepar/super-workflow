package wf

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func withBuildInfo(t *testing.T, bi *debug.BuildInfo) {
	t.Helper()
	old := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, bi != nil }
	t.Cleanup(func() { readBuildInfo = old })
}

func withDeclared(t *testing.T, workflow, engine, modified string) {
	t.Helper()
	w, e, m := declaredWorkflowRevision, declaredEngineRevision, declaredModified
	declaredWorkflowRevision, declaredEngineRevision, declaredModified = workflow, engine, modified
	t.Cleanup(func() { declaredWorkflowRevision, declaredEngineRevision, declaredModified = w, e, m })
}

func vcs(rev, modified string) []debug.BuildSetting {
	return []debug.BuildSetting{{Key: "-buildmode", Value: "exe"}, {Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: modified}}
}

// The stamp records what the build info says and marks every gap unknown;
// a declared revision fills only a gap, and says it is declared.
func TestBuildStampFromBuildInfo(t *testing.T) {
	const wfPath = "example.com/someflow"
	engineAt := func(version string, replace *debug.Module) []*debug.Module {
		return []*debug.Module{{Path: "example.com/other", Version: "v1.0.0"}, {Path: engineModule, Version: version, Replace: replace}}
	}
	for _, c := range []struct {
		name                     string
		bi                       *debug.BuildInfo
		workflow, engine, modify string // declared
		wantWorkflow, wantEngine string
		exact                    string
	}{
		{name: "pinned engine, clean vcs workflow",
			bi:           &debug.BuildInfo{GoVersion: "go1.26.0", Main: debug.Module{Path: wfPath, Version: "v0.0.0-20261126000000-0123456789ab"}, Deps: engineAt("v0.3.0", nil), Settings: vcs("0123456789abcdef", "false")},
			wantWorkflow: wfPath + " v0.0.0-20261126000000-0123456789ab revision 0123456789abcdef (vcs)",
			wantEngine:   engineModule + " v0.3.0", exact: "exact"},
		{name: "dirty workflow",
			bi:           &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("v0.3.0", nil), Settings: vcs("0123456789abcdef", "true")},
			wantWorkflow: wfPath + " (devel) revision 0123456789abcdef (vcs) modified",
			wantEngine:   engineModule + " v0.3.0", exact: "not exact: workflow modified tree"},
		{name: "-buildvcs=false",
			bi:           &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("v0.3.0", nil)},
			wantWorkflow: wfPath + " (devel) revision unknown",
			wantEngine:   engineModule + " v0.3.0", exact: "not exact: workflow revision unknown"},
		{name: "go.work engine",
			bi:           &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("(devel)", nil), Settings: vcs("0123456789abcdef", "false")},
			wantWorkflow: wfPath + " (devel) revision 0123456789abcdef (vcs)",
			wantEngine:   engineModule + " (devel) revision unknown", exact: "not exact: engine revision unknown"},
		{name: "local replace",
			bi:           &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("v0.0.0", &debug.Module{Path: "./.engine"}), Settings: vcs("0123456789abcdef", "false")},
			wantWorkflow: wfPath + " (devel) revision 0123456789abcdef (vcs)",
			wantEngine:   engineModule + " v0.0.0 => ./.engine revision unknown", exact: "not exact: engine revision unknown"},
		{name: "local replace as Go reports it",
			// A directory replacement's version is "(devel)", which is no module version.
			bi:           &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("v0.0.0", &debug.Module{Path: "./.engine", Version: "(devel)"}), Settings: vcs("0123456789abcdef", "false")},
			wantWorkflow: wfPath + " (devel) revision 0123456789abcdef (vcs)",
			wantEngine:   engineModule + " v0.0.0 => ./.engine revision unknown", exact: "not exact: engine revision unknown"},
		{name: "declared fills a local replace",
			bi:       &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("v0.0.0", &debug.Module{Path: "./.engine", Version: "(devel)"})},
			workflow: "aaaa", engine: "bbbb", modify: "false",
			wantWorkflow: wfPath + " (devel) revision aaaa (declared)",
			wantEngine:   engineModule + " v0.0.0 => ./.engine revision bbbb (declared)", exact: "exact"},
		{name: "module replace",
			bi:           &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("v0.0.0", &debug.Module{Path: "example.com/enginefork", Version: "v0.3.1"}), Settings: vcs("0123456789abcdef", "false")},
			wantWorkflow: wfPath + " (devel) revision 0123456789abcdef (vcs)",
			wantEngine:   engineModule + " v0.0.0 => example.com/enginefork@v0.3.1", exact: "exact"},
		{name: "declared fills only the gaps",
			bi:       &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("(devel)", nil)},
			workflow: "aaaa", engine: "bbbb", modify: "false",
			wantWorkflow: wfPath + " (devel) revision aaaa (declared)",
			wantEngine:   engineModule + " (devel) revision bbbb (declared)", exact: "exact"},
		{name: "declared never overrides vcs or a module version",
			bi:       &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("v0.3.0", nil), Settings: vcs("0123456789abcdef", "false")},
			workflow: "aaaa", engine: "bbbb", modify: "true",
			wantWorkflow: wfPath + " (devel) revision 0123456789abcdef (vcs)",
			wantEngine:   engineModule + " v0.3.0", exact: "exact"},
		{name: "declared without a modified flag",
			bi:           &debug.BuildInfo{Main: debug.Module{Path: wfPath, Version: "(devel)"}, Deps: engineAt("v0.3.0", nil)},
			workflow:     "aaaa",
			wantWorkflow: wfPath + " (devel) revision aaaa (declared) modified unknown",
			wantEngine:   engineModule + " v0.3.0", exact: "not exact: workflow modified flag unknown"},
		{name: "the engine is the main module",
			bi:           &debug.BuildInfo{Main: debug.Module{Path: engineModule, Version: "(devel)"}, Settings: vcs("0123456789abcdef", "false")},
			wantWorkflow: engineModule + " (devel) revision 0123456789abcdef (vcs)",
			wantEngine:   engineModule + " (devel) revision 0123456789abcdef (vcs)", exact: "exact"},
		{name: "no build info",
			wantWorkflow: "(unknown module) revision unknown",
			wantEngine:   engineModule + " revision unknown", exact: "not exact: engine revision unknown, workflow revision unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withBuildInfo(t, c.bi)
			withDeclared(t, c.workflow, c.engine, c.modify)
			b := currentBuild()
			if got := b.Workflow.String(); got != c.wantWorkflow {
				t.Errorf("workflow %q, want %q", got, c.wantWorkflow)
			}
			if got := b.Engine.String(); got != c.wantEngine {
				t.Errorf("engine %q, want %q", got, c.wantEngine)
			}
			if got := b.Lines()[4]; got != "source   "+c.exact {
				t.Errorf("%q, want %q", got, c.exact)
			}
			if b.JournalFormat != 1 || b.GOOS == "" || b.GOARCH == "" || b.Go == "" {
				t.Errorf("stamp %+v", b)
			}
		})
	}
}

// Another target is the same source; another revision or toolchain is not.
func TestBuildStampDiffers(t *testing.T) {
	no := false
	a := BuildStamp{Engine: ModuleStamp{Path: engineModule, Version: "v0.3.0", Source: StampModule},
		Workflow: ModuleStamp{Path: "example.com/someflow", Revision: "aaaa", Modified: &no, Source: StampVCS}, Go: "go1.26.0", GOOS: "linux", GOARCH: "amd64", JournalFormat: 1}
	b := a
	b.GOOS, b.GOARCH = "darwin", "arm64"
	no2 := false
	b.Workflow.Modified = &no2
	if d := a.differs(b); d != "" {
		t.Fatalf("other target differs: %s", d)
	}
	b.Engine.Version, b.Go = "v0.3.1", "go1.26.1"
	if d := a.differs(b); d != "engine "+engineModule+" v0.3.1; go go1.26.1" {
		t.Fatalf("differs %q", d)
	}
}

// A hash cache from a newer engine names its version; the caller still
// discards it and rebuilds.
func TestHashCacheNewerVersionIsNamed(t *testing.T) {
	body := binary.LittleEndian.AppendUint64(nil, hashCacheVersion+1)
	sum := sha256.Sum256(body)
	dir := t.TempDir()
	path := filepath.Join(dir, hashCacheName)
	if err := os.WriteFile(path, append(body, sum[:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHashCache(path); !errors.Is(err, ErrNewerFormat) || !strings.Contains(err.Error(), "hash cache is format 3") {
		t.Fatalf("newer cache: %v", err)
	}
	if c := hashCacheFor(dir); len(c.old) != 0 {
		t.Fatalf("a newer cache was used: %v", c.old)
	}
}
