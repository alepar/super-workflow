package wf

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// JournalFormat is the journal format this engine writes. Run creation
// records it in the Build stamp; reading a range of formats is later work.
const JournalFormat = 1

// engineModule is the engine's module path, looked up in the build info.
const engineModule = "github.com/alepar/super-workflow/docs/proposals/go-api"

// Declared source identity, for builds without VCS stamping
// (-buildvcs=false). A build script sets them with
//
//	-ldflags "-X <engineModule>/wf.declaredWorkflowRevision=<rev> -X <engineModule>/wf.declaredEngineRevision=<rev> -X <engineModule>/wf.declaredModified=false"
//
// They fill only what the build info lacks: a VCS revision or a real module
// version always wins, and a declared value is recorded as declared.
var (
	declaredWorkflowRevision string
	declaredEngineRevision   string
	declaredModified         string // "true" or "false"; anything else is unknown
)

// readBuildInfo is debug.ReadBuildInfo; tests replace it.
var readBuildInfo = debug.ReadBuildInfo

// Where a ModuleStamp's identity came from.
const (
	StampVCS      = "vcs"      // the build's own vcs.revision and vcs.modified
	StampModule   = "module"   // a real module version, immutable by construction
	StampDeclared = "declared" // the declared* link-time variables
)

// BuildStamp is the source identity of the binary that created a run: the
// engine and workflow modules, the toolchain and the journal format. The
// same revisions built for another GOOS/GOARCH are the same source.
type BuildStamp struct {
	Engine        ModuleStamp
	Workflow      ModuleStamp
	Go            string
	GOOS          string
	GOARCH        string
	JournalFormat int
}

// ModuleStamp identifies one module's source. Fields are as Go reports
// them; an empty Source means the identity is unknown, never guessed.
type ModuleStamp struct {
	Path     string
	Version  string `json:",omitempty"` // a tag, a pseudo-version or "(devel)"
	Replace  string `json:",omitempty"` // the replacement, path[@version]
	Revision string `json:",omitempty"`
	Modified *bool  `json:",omitempty"` // nil: unknown
	Source   string `json:",omitempty"` // StampVCS, StampModule, StampDeclared or "" (unknown)
}

// currentBuild is this binary's stamp.
func currentBuild() BuildStamp {
	b := BuildStamp{Go: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, JournalFormat: JournalFormat}
	bi, ok := readBuildInfo()
	if !ok || bi == nil {
		b.Engine = declared(ModuleStamp{Path: engineModule}, declaredEngineRevision)
		b.Workflow = declared(ModuleStamp{}, declaredWorkflowRevision)
		return b
	}
	if bi.GoVersion != "" {
		b.Go = bi.GoVersion
	}
	b.Workflow = mainStamp(bi)
	if bi.Main.Path == engineModule {
		b.Engine = b.Workflow
		if b.Engine.Source == "" {
			b.Engine = declared(b.Engine, declaredEngineRevision)
		}
		return b
	}
	b.Engine = ModuleStamp{Path: engineModule}
	for _, dep := range bi.Deps {
		if dep.Path == engineModule {
			b.Engine = depStamp(dep)
		}
	}
	if b.Engine.Source == "" {
		b.Engine = declared(b.Engine, declaredEngineRevision)
	}
	return b
}

// mainStamp is the main module's identity: its VCS settings, else a real
// module version (go install path@version), else the declared revision.
func mainStamp(bi *debug.BuildInfo) ModuleStamp {
	m := ModuleStamp{Path: bi.Main.Path, Version: bi.Main.Version}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			m.Revision, m.Source = s.Value, StampVCS
		case "vcs.modified":
			v := s.Value == "true"
			m.Modified = &v
		}
	}
	if m.Source == StampVCS {
		return m
	}
	m.Modified = nil
	if realVersion(m.Version) {
		m.Source = StampModule
		return m
	}
	return declared(m, declaredWorkflowRevision)
}

// depStamp is a dependency's identity: known only for a real module version,
// replaced or not; a local directory replace or a go.work module is unknown.
func depStamp(dep *debug.Module) ModuleStamp {
	m := ModuleStamp{Path: dep.Path, Version: dep.Version}
	if r := dep.Replace; r != nil {
		m.Replace = r.Path
		if r.Version != "" {
			m.Replace += "@" + r.Version
			m.Source = StampModule
		}
		return m
	}
	if realVersion(m.Version) {
		m.Source = StampModule
	}
	return m
}

func realVersion(v string) bool { return v != "" && v != "(devel)" }

// declared fills an unknown m from a declared revision, if one was set.
func declared(m ModuleStamp, revision string) ModuleStamp {
	if revision == "" {
		return m
	}
	m.Revision, m.Source, m.Modified = revision, StampDeclared, nil
	if declaredModified == "true" || declaredModified == "false" {
		v := declaredModified == "true"
		m.Modified = &v
	}
	return m
}

// Exact reports whether m names a source a binary can be rebuilt from, and
// if not, why.
func (m ModuleStamp) Exact() (bool, string) {
	switch {
	case m.Source == StampModule:
		return true, ""
	case m.Source == "":
		return false, "revision unknown"
	case m.Modified == nil:
		return false, "modified flag unknown"
	case *m.Modified:
		return false, "modified tree"
	}
	return true, ""
}

// Exact reports whether both modules are exact; GOOS, GOARCH and the
// toolchain do not enter it.
func (b BuildStamp) Exact() (bool, string) {
	var why []string
	for _, m := range []struct {
		name string
		m    ModuleStamp
	}{{"engine", b.Engine}, {"workflow", b.Workflow}} {
		if ok, r := m.m.Exact(); !ok {
			why = append(why, m.name+" "+r)
		}
	}
	return len(why) == 0, strings.Join(why, ", ")
}

// String is one module's identity on one line.
func (m ModuleStamp) String() string {
	path := m.Path
	if path == "" {
		path = "(unknown module)"
	}
	parts := []string{path}
	if m.Version != "" {
		parts = append(parts, m.Version)
	}
	if m.Replace != "" {
		parts = append(parts, "=> "+m.Replace)
	}
	switch m.Source {
	case StampVCS, StampDeclared:
		parts = append(parts, "revision "+m.Revision+" ("+m.Source+")")
		switch {
		case m.Modified == nil:
			parts = append(parts, "modified unknown")
		case *m.Modified:
			parts = append(parts, "modified")
		}
	case StampModule:
	default:
		parts = append(parts, "revision unknown")
	}
	return strings.Join(parts, " ")
}

// Lines renders the stamp, one fact per line.
func (b BuildStamp) Lines() []string {
	exact := "exact"
	if ok, why := b.Exact(); !ok {
		exact = "not exact: " + why
	}
	return []string{
		"engine   " + b.Engine.String(),
		"workflow " + b.Workflow.String(),
		fmt.Sprintf("go       %s %s/%s", b.Go, b.GOOS, b.GOARCH),
		fmt.Sprintf("journal  format %d", b.JournalFormat),
		"source   " + exact,
	}
}

// Summary is the stamp on one line, for status.
func (b BuildStamp) Summary() string {
	exact := "exact"
	if ok, why := b.Exact(); !ok {
		exact = "not exact: " + why
	}
	return fmt.Sprintf("engine %s; workflow %s; %s %s/%s; journal format %d; %s", b.Engine, b.Workflow, b.Go, b.GOOS, b.GOARCH, b.JournalFormat, exact)
}

// differs names how other's source identity differs from b's; GOOS and
// GOARCH do not count. Empty when they are the same.
func (b BuildStamp) differs(other BuildStamp) string {
	var d []string
	if b.Engine.String() != other.Engine.String() {
		d = append(d, "engine "+other.Engine.String())
	}
	if b.Workflow.String() != other.Workflow.String() {
		d = append(d, "workflow "+other.Workflow.String())
	}
	if b.Go != other.Go {
		d = append(d, "go "+other.Go)
	}
	if b.JournalFormat != other.JournalFormat {
		d = append(d, fmt.Sprintf("journal format %d", other.JournalFormat))
	}
	return strings.Join(d, "; ")
}

// ErrNewerFormat marks a durable artifact written in a format newer than
// this engine reads.
var ErrNewerFormat = errors.New("written by a newer engine")

// NewerFormat is the refusal for artifact at format got when this engine
// reads up to most. It wraps ErrNewerFormat.
func NewerFormat(artifact string, got, most int) error {
	return fmt.Errorf("%s is format %d; this engine reads up to %d: %w, so use that engine or a newer one", artifact, got, most, ErrNewerFormat)
}
