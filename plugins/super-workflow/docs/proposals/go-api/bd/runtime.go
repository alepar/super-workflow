package bd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Config configures the bd runtime.
type Config struct {
	// Bin is the bd executable, "bd" when empty.
	Bin string
	// Timeout bounds each call; wf.ToolTimeout when zero.
	Timeout time.Duration
}

// Runtime returns the wf.AdapterRuntime serving every op this package declares
// through the bd CLI with --json.
//
// Owns: bd argv, result normalisation into Issue and the prior-completion
// checks of the declared effects. Every call runs with the run workspace as
// its working directory, so the workspace's tracker is the one touched.
// Does not: journal, retry or route; or choose which labels scope or exclude
// ready work: ReadyIn passes them, and the deprecated Ready keeps its fixed
// legacy convention (legacyReadyExclude, the sp:<epic> label).
// Extend by: a new op is a case in each of Read, Apply and Check, plus its name
// in Ops.
func Runtime(cfg Config) wf.AdapterRuntime {
	if cfg.Bin == "" {
		cfg.Bin = "bd"
	}
	return runtime{bin: cfg.Bin, timeout: cfg.Timeout}
}

type runtime struct {
	bin     string
	timeout time.Duration
	// sandbox runs every call with --sandbox: the run is a fork, and bd must
	// not sync it to a configured remote (EffectContext.Sandboxed).
	sandbox bool
}

// at is the runtime for one call of a run: sandboxed for a fork.
func (r runtime) at(ctx wf.EffectContext) runtime {
	r.sandbox = r.sandbox || ctx.Sandboxed
	return r
}

// ReadAt, ApplyAt and CheckAt are Read, Apply and Check for a call of a run.
func (r runtime) ReadAt(workspace string, ctx wf.EffectContext, d wf.ObservationDesc) (json.RawMessage, error) {
	return r.at(ctx).Read(workspace, d)
}

func (r runtime) ApplyAt(workspace string, ctx wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, error) {
	return r.at(ctx).Apply(workspace, e)
}

func (r runtime) CheckAt(workspace string, ctx wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, error) {
	return r.at(ctx).Check(workspace, e)
}

// metadataKey is a metadata key set_metadata accepts: a plain word, so the
// key=value argument always starts with the key, never with '-'.
var metadataKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (runtime) Ops() []string {
	return []string{"bd.ready", "bd.show", "bd.list", "bd.epic_close_eligible", "bd.create", "bd.update_status", "bd.set_metadata", "bd.dep_add", "bd.close", "bd.supersede"}
}

// run invokes bd in workspace and returns its stdout.
func (r runtime) run(workspace string, args ...string) ([]byte, error) {
	// bd's first call under a fresh HOME prints a metrics notice that would
	// prefix every error the adapter quotes; metrics are off for the adapter.
	tool := wf.Tool{Bin: r.bin, Dir: workspace, Timeout: r.timeout, Env: []string{"BD_DISABLE_METRICS=1"}}
	if r.sandbox {
		args = append([]string{"--sandbox"}, args...)
		// A sandboxed call runs with an empty environment but PATH and
		// HOME: it never sees bd's storage overrides (BEADS_DB, BD_*,
		// DOLT_*) or anything else that would point it elsewhere.
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); k != "PATH" && k != "HOME" {
				tool.UnsetEnv = append(tool.UnsetEnv, k)
			}
		}
	}
	tool.Args = args
	res, err := wf.RunTool(tool)
	if err != nil {
		return nil, err
	}
	if res.Exit != 0 {
		msg := strings.TrimSpace(res.Stderr + res.Stdout)
		if transient(diagnostic(res.Stdout, res.Stderr, args)) {
			return nil, fmt.Errorf("%w: bd %s: exit %d: %s", wf.ErrUnavailable, strings.Join(args, " "), res.Exit, msg)
		}
		return nil, fmt.Errorf("bd %s: exit %d: %s", strings.Join(args, " "), res.Exit, msg)
	}
	return []byte(res.Stdout), nil
}

// transientFailure matches a bd failure that passes on its own: a timeout or
// contention for a lock or database another process holds. A bare "lock" is
// not enough: a lock file that cannot be created is permanent.
var transientFailure = regexp.MustCompile(`(?i)timed out|database is locked|lock held|lock acquisition timeout|resource busy|database is busy`)

// permanentFailure matches a failure that does not pass on its own even when
// it mentions a lock: permissions and paths. It takes precedence.
var permanentFailure = regexp.MustCompile(`(?i)permission denied|no such file|read-only file system`)

// transient reports whether a failed call's diagnostic is a transient
// failure, returned as wf.ErrUnavailable so the engine backs off and reads
// again.
func transient(diag string) bool {
	return transientFailure.MatchString(diag) && !permanentFailure.MatchString(diag)
}

// diagnostic is bd's own statement of why a call failed, without the call's
// inputs or bd's hints, so neither can make a failure look transient (an id
// "timed out" that is not found is a permanent failure). With --json bd
// reports it as the "error" field of a JSON object on stdout; otherwise it is
// stderr less its Hint: lines. Every argument of the call is cut out of it
// where it stands as a whole token, so a short argument ("a") never erases
// part of a word.
func diagnostic(stdout, stderr string, args []string) string {
	var out struct{ Error string }
	diag := ""
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out) == nil && out.Error != "" {
		diag = out.Error
	} else {
		var lines []string
		for _, l := range strings.Split(stderr, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "Hint:") {
				lines = append(lines, l)
			}
		}
		diag = strings.Join(lines, "\n")
	}
	for _, a := range args {
		if a != "" {
			token := regexp.MustCompile(`(^|[^A-Za-z0-9_.:-])` + regexp.QuoteMeta(a) + `($|[^A-Za-z0-9_.:-])`)
			diag = token.ReplaceAllString(diag, "$1$2")
		}
	}
	return diag
}

// rawIssue is bd's lower-snake JSON shape for a bead.
type rawIssue struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	IssueType    string   `json:"issue_type"`
	Status       string   `json:"status"`
	Priority     int      `json:"priority"`
	Labels       []string `json:"labels"`
	Parent       string   `json:"parent"`
	Dependencies []struct {
		ID   string `json:"id"`
		Type string `json:"dependency_type"`
	} `json:"dependencies"`
}

func (r rawIssue) issue() Issue {
	return Issue{ID: r.ID, Title: r.Title, Description: r.Description, Type: r.IssueType,
		Status: Status(r.Status), Priority: r.Priority, Labels: r.Labels, Parent: r.Parent}
}

// beads decodes a bd JSON array of beads.
func beads(out []byte) ([]rawIssue, error) {
	var v []rawIssue
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("decode bd output: %w", err)
	}
	return v, nil
}

func (r runtime) show(workspace, id string) (rawIssue, error) {
	out, err := r.run(workspace, "show", id, "--json")
	if err != nil {
		return rawIssue{}, err
	}
	v, err := beads(out)
	if err != nil {
		return rawIssue{}, err
	}
	if len(v) != 1 {
		return rawIssue{}, fmt.Errorf("bd show %s returned %d beads", id, len(v))
	}
	return v[0], nil
}

func issues(v []rawIssue) []Issue {
	out := make([]Issue, 0, len(v))
	for _, x := range v {
		out = append(out, x.issue())
	}
	return out
}

func argStr(args map[string]any, k string) string {
	switch v := args[k].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// issueArg reads the "issue" arg, which is an Issue in process and a decoded
// map after a journal round trip.
func issueArg(args map[string]any) (Issue, error) {
	b, err := json.Marshal(args["issue"])
	if err != nil {
		return Issue{}, err
	}
	var is Issue
	if err := json.Unmarshal(b, &is); err != nil {
		return Issue{}, fmt.Errorf("decode issue arg: %w", err)
	}
	return is, nil
}

// keyLabel is the label a created bead is stamped with. bd parses --label and
// --labels as CSV, so every CSV-special character in the key (it can hold a
// title) is percent-encoded: a comma, a double quote, CR and LF
// (labelEscaper; a key with none of them is stamped as it always was). bd
// caps a label at 255 characters, so a key whose escaped label would pass
// maxKeyLabel is replaced by the SHA-256 of the key; Apply and Check share
// this derivation.
func keyLabel(e wf.ExternalEffect) string {
	l := "key:" + labelEscaper.Replace(e.KeyID())
	if len(l) > maxKeyLabel {
		sum := sha256.Sum256([]byte(e.KeyID()))
		return "key:sha256-" + hex.EncodeToString(sum[:])
	}
	return l
}

// labelEscaper percent-encodes the characters bd's CSV label parsing gives a
// meaning to.
var labelEscaper = strings.NewReplacer(",", "%2C", `"`, "%22", "\r", "%0D", "\n", "%0A")

// maxKeyLabel stays under bd's 255-character label limit.
const maxKeyLabel = 200

const readyLimit = 500

// legacyReadyExclude is the exclusion the deprecated Ready descriptor (args
// "epic") always applied. It is kept so a recorded Ready read means the same.
const legacyReadyExclude = "blocker,sp:review,sp:gate,human-gate,human"

func (r runtime) Read(workspace string, d wf.ObservationDesc) (json.RawMessage, error) {
	switch d.Op {
	case "ready":
		if _, legacy := d.Args["epic"]; legacy {
			label := ""
			if epic := argStr(d.Args, "epic"); epic != "" {
				label = "sp:" + epic
			}
			return r.ready(workspace, label, legacyReadyExclude)
		}
		return r.ready(workspace, argStr(d.Args, "label"), argStr(d.Args, "exclude_labels"))
	case "show":
		b, err := r.show(workspace, argStr(d.Args, "id"))
		if err != nil {
			return nil, err
		}
		return json.Marshal(b.issue())
	case "list":
		out, err := r.run(workspace, "list", "--label", argStr(d.Args, "label"), "--all", "--limit", "0", "--json")
		if err != nil {
			return nil, err
		}
		v, err := beads(out)
		if err != nil {
			return nil, err
		}
		return json.Marshal(issues(v))
	case "epic_close_eligible":
		// --dry-run is load-bearing: without it bd closes the epics it lists.
		out, err := r.run(workspace, "epic", "close-eligible", "--dry-run", "--json")
		if err != nil {
			return nil, err
		}
		return closeEligible(out, argStr(d.Args, "epic"))
	}
	return nil, fmt.Errorf("bd: unknown read op %q", d.Op)
}

// ready returns the claimable work carrying label (all of it when empty) and
// none of the comma-separated exclude labels, widening the limit while the
// result fills it.
func (r runtime) ready(workspace, label, exclude string) (json.RawMessage, error) {
	limit := readyLimit
	for attempt := 0; ; attempt++ {
		args := []string{"ready", "--exclude-type=epic,gate"}
		if exclude != "" {
			args = append(args, "--exclude-label", exclude)
		}
		if label != "" {
			args = append(args, "--label", label)
		}
		out, err := r.run(workspace, append(args, "--limit", fmt.Sprint(limit), "--json")...)
		if err != nil {
			return nil, err
		}
		v, err := beads(out)
		if err != nil {
			return nil, err
		}
		if len(v) < limit || attempt == 3 {
			return json.Marshal(issues(v))
		}
		limit *= 2
	}
}

// closeEligible extracts the ids of fully-closed epics, restricted to epic's
// tree when epic is set.
func closeEligible(out []byte, epic string) (json.RawMessage, error) {
	// With nothing eligible bd prints a summary object, not an array.
	if bytes.HasPrefix(bytes.TrimSpace(out), []byte("{")) {
		return json.Marshal([]string{})
	}
	var v []struct {
		Epic struct {
			ID string `json:"id"`
		} `json:"epic"`
		Eligible bool `json:"eligible_for_close"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("decode bd output: %w", err)
	}
	ids := []string{}
	for _, x := range v {
		if x.Eligible && (epic == "" || x.Epic.ID == epic || strings.HasPrefix(x.Epic.ID, epic+".")) {
			ids = append(ids, x.Epic.ID)
		}
	}
	return json.Marshal(ids)
}

func (r runtime) Apply(workspace string, e wf.ExternalEffect) (json.RawMessage, error) {
	switch e.Op {
	case "create":
		is, err := issueArg(e.Args)
		if err != nil {
			return nil, err
		}
		return r.create(workspace, e, is)
	case "set_metadata":
		id := argStr(e.Args, "id")
		fields, _ := e.Args["fields"].(map[string]any)
		keys := make([]string, 0, len(fields))
		for k := range fields {
			if !metadataKey.MatchString(k) {
				return nil, fmt.Errorf("bd set_metadata: key %q is not a plain word", k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		argv := []string{"update", id}
		for _, k := range keys {
			v, _ := fields[k].(string)
			argv = append(argv, "--set-metadata", k+"="+v)
		}
		if _, err := r.run(workspace, append(argv, "--json")...); err != nil {
			return nil, err
		}
		b, err := r.show(workspace, id)
		if err != nil {
			return nil, err
		}
		return json.Marshal(b.issue())
	case "update_status":
		id, status := argStr(e.Args, "id"), argStr(e.Args, "status")
		b, err := r.show(workspace, id)
		if err != nil {
			return nil, err
		}
		if b.Status != status {
			if _, err := r.run(workspace, "update", id, "--status", status, "--json"); err != nil {
				return nil, err
			}
			b.Status = status
		}
		return json.Marshal(b.issue())
	case "close":
		id := argStr(e.Args, "id")
		b, err := r.show(workspace, id)
		if err != nil {
			return nil, err
		}
		if b.Status != string(Closed) {
			if _, err := r.run(workspace, "close", id, "--reason", argStr(e.Args, "reason"), "--json"); err != nil {
				return nil, err
			}
			b.Status = string(Closed)
		}
		return json.Marshal(b.issue())
	case "dep_add":
		return r.depAdd(workspace, e)
	case "supersede":
		b, done, err := r.supersedeState(workspace, e)
		if err != nil {
			return nil, err
		}
		if !done {
			// bd refuses the supersedes edge while another dependency links
			// old to new. A blocks edge (from wiring the pair) is obsolete
			// once old is superseded, so it is removed first; any other type
			// carries meaning (provenance, hierarchy) and is never deleted.
			for _, d := range b.Dependencies {
				if d.ID != argStr(e.Args, "new") || d.Type == "supersedes" {
					continue
				}
				if d.Type != "blocks" {
					return nil, fmt.Errorf("bd supersede: %s already depends on %s with type %q; superseding would replace that link, so it is refused", argStr(e.Args, "old"), d.ID, d.Type)
				}
				if _, err := r.run(workspace, "dep", "remove", argStr(e.Args, "old"), argStr(e.Args, "new")); err != nil {
					return nil, err
				}
			}
			if _, err := r.run(workspace, "supersede", argStr(e.Args, "old"), "--with", argStr(e.Args, "new")); err != nil {
				return nil, err
			}
			b, done, err = r.supersedeState(workspace, e)
			if err != nil {
				return nil, err
			}
			if !done {
				return nil, fmt.Errorf("bd supersede: replacement not confirmed")
			}
		}
		if err := r.repointDependents(workspace, argStr(e.Args, "old"), argStr(e.Args, "new")); err != nil {
			return nil, err
		}
		return json.Marshal(b.issue())
	}
	return nil, fmt.Errorf("bd: unknown effect op %q", e.Op)
}

// create files the bead with the I3 triple and the key label.
func (r runtime) create(workspace string, e wf.ExternalEffect, is Issue) (json.RawMessage, error) {
	args := []string{"create", is.Title, "--json"}
	if is.Type != "" {
		args = append(args, "--type", is.Type)
	}
	if is.Description != "" {
		args = append(args, "--description", is.Description)
	}
	if is.Priority > 0 {
		args = append(args, "--priority", fmt.Sprint(is.Priority))
	}
	labels := append([]string(nil), is.Labels...)
	if is.RootLabel != "" {
		labels = append(labels, is.RootLabel)
	}
	if e.KeyID() != "" {
		labels = append(labels, keyLabel(e))
	}
	if len(labels) > 0 {
		args = append(args, "--labels", strings.Join(labels, ","))
	}
	if is.Parent != "" {
		args = append(args, "--parent", is.Parent)
	}
	if is.NoInheritLabels {
		args = append(args, "--no-inherit-labels")
	}
	out, err := r.run(workspace, args...)
	if err != nil {
		return nil, err
	}
	var b rawIssue
	if err := json.Unmarshal(out, &b); err != nil {
		return nil, fmt.Errorf("decode bd output: %w", err)
	}
	if b.Parent == "" {
		b.Parent = is.Parent
	}
	return json.Marshal(b.issue())
}

// depAdd records dependent blocked by blocker, then the reason line on the
// dependent's description (bd dep add has no reason flag).
func (r runtime) depAdd(workspace string, e wf.ExternalEffect) (json.RawMessage, error) {
	dependent, blocker, reason := argStr(e.Args, "dependent"), argStr(e.Args, "blocker"), argStr(e.Args, "reason")
	b, err := r.show(workspace, dependent)
	if err != nil {
		return nil, err
	}
	if !blocks(b, blocker) {
		if _, err := r.run(workspace, "dep", "add", dependent, blocker); err != nil {
			return nil, err
		}
	}
	if reason != "" && !hasLine(b.Description, reason) {
		desc := reason
		if b.Description != "" {
			desc = strings.TrimRight(b.Description, "\n") + "\n" + reason
		}
		if _, err := r.run(workspace, "update", dependent, "--description", desc, "--json"); err != nil {
			return nil, err
		}
	}
	b, err = r.show(workspace, dependent)
	if err != nil {
		return nil, err
	}
	return json.Marshal(b.issue())
}

func blocks(b rawIssue, blocker string) bool {
	for _, d := range b.Dependencies {
		if d.ID == blocker && d.Type != "parent-child" {
			return true
		}
	}
	return false
}

func hasLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == strings.TrimSpace(line) {
			return true
		}
	}
	return false
}

// Check answers from a read of the tracker; a failed read is EffectUnknown.
func (r runtime) Check(workspace string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	yes := func(ok bool) wf.EffectStatus {
		if ok {
			return wf.EffectDone
		}
		return wf.EffectNotDone
	}
	switch e.Op {
	case "supersede":
		_, done, err := r.supersedeState(workspace, e)
		if err != nil {
			return wf.EffectUnknown, err
		}
		if !done {
			return yes(false), nil
		}
		// Done also takes the dependents moved onto the replacement.
		deps, err := r.blockingDependents(workspace, argStr(e.Args, "old"), argStr(e.Args, "new"))
		if err != nil {
			return wf.EffectUnknown, err
		}
		return yes(len(deps) == 0), nil
	case "create":
		if e.KeyID() == "" {
			return wf.EffectUnknown, nil
		}
		is, err := issueArg(e.Args)
		if err != nil {
			return wf.EffectUnknown, err
		}
		out, err := r.run(workspace, "list", "--label", keyLabel(e), "--all", "--limit", "0", "--json")
		if err != nil {
			return wf.EffectUnknown, err
		}
		v, err := beads(out)
		if err != nil {
			return wf.EffectUnknown, err
		}
		for _, x := range v {
			if x.Title == is.Title && (is.Parent == "" || x.Parent == is.Parent) {
				return wf.EffectDone, nil
			}
		}
		return wf.EffectNotDone, nil
	case "set_metadata":
		// Rewriting the same values is harmless: recovery applies it again.
		return wf.EffectNotDone, nil
	case "update_status", "close", "dep_add":
		id := argStr(e.Args, "id")
		if e.Op == "dep_add" {
			id = argStr(e.Args, "dependent")
		}
		b, err := r.show(workspace, id)
		if err != nil {
			return wf.EffectUnknown, err
		}
		switch e.Op {
		case "update_status":
			return yes(b.Status == argStr(e.Args, "status")), nil
		case "close":
			return yes(b.Status == string(Closed)), nil
		}
		// An edge without its reason line is not done: recovery re-applies
		// the idempotent depAdd.
		reason := argStr(e.Args, "reason")
		return yes(blocks(b, argStr(e.Args, "blocker")) && (reason == "" || hasLine(b.Description, reason))), nil
	}
	return wf.EffectUnknown, fmt.Errorf("bd: unknown effect op %q", e.Op)
}

// blockingDependents are the open beads blocked on id (bd dep list
// --direction=up -t blocks), the replacement itself excluded. A closed
// dependent (one superseded earlier, often onto the same replacement) carries
// no work: it is neither moved nor waited for.
func (r runtime) blockingDependents(workspace, id, except string) ([]string, error) {
	out, err := r.run(workspace, "dep", "list", id, "--direction=up", "-t", "blocks", "--json")
	if err != nil {
		return nil, err
	}
	var deps []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &deps); err != nil {
		return nil, fmt.Errorf("bd dep list %s: %w", id, err)
	}
	var ids []string
	for _, d := range deps {
		if d.ID != "" && d.ID != except && d.Status != string(Closed) {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}

// repointDependents moves every bead blocked on oldID onto newID (item 85): the
// edge to the replacement is added before the edge to the closed bead is
// removed, so a dependent is never unblocked in between. Both bd commands are
// idempotent, so a replay after a crash finishes the move.
func (r runtime) repointDependents(workspace, oldID, newID string) error {
	deps, err := r.blockingDependents(workspace, oldID, newID)
	if err != nil {
		return err
	}
	for _, d := range deps {
		// bd refuses the edge when it would make a cycle (the replacement
		// already waits on d) or when d is the replacement's parent. Then d
		// cannot wait on the replacement at all; it still leaves the closed
		// bead, which no longer stands for any work.
		if _, err := r.run(workspace, "dep", "add", d, newID); err != nil && !edgeRefused(err) {
			return err
		}
		if _, err := r.run(workspace, "dep", "remove", d, oldID); err != nil {
			return err
		}
	}
	return nil
}

// edgeRefused reports a bd dep add that bd refused for the graph's sake (a
// cycle, a parent-child deadlock), not because bd failed.
func edgeRefused(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "would create a cycle") || strings.Contains(msg, "already a child of")
}

// supersedeState uses bd show's outgoing supersedes edge (old -> replacement).
// Both identities must match exactly: a closed status alone proves nothing.
func (r runtime) supersedeState(workspace string, e wf.ExternalEffect) (rawIssue, bool, error) {
	oldID, newID := argStr(e.Args, "old"), argStr(e.Args, "new")
	for _, id := range []string{oldID, newID} {
		if id == "" || strings.TrimSpace(id) != id || strings.HasPrefix(id, "-") {
			return rawIssue{}, false, fmt.Errorf("bd supersede: invalid issue ID %q", id)
		}
	}
	if oldID == newID {
		return rawIssue{}, false, fmt.Errorf("bd supersede: issue cannot replace itself")
	}
	old, err := r.show(workspace, oldID)
	if err != nil {
		return rawIssue{}, false, err
	}
	if old.ID != oldID {
		return rawIssue{}, false, fmt.Errorf("bd supersede: expected issue %q, got %q", oldID, old.ID)
	}
	target, err := r.show(workspace, newID)
	if err != nil {
		return rawIssue{}, false, err
	}
	if target.ID != newID {
		return rawIssue{}, false, fmt.Errorf("bd supersede: expected replacement %q, got %q", newID, target.ID)
	}
	linked := false
	for _, d := range old.Dependencies {
		if d.Type != "supersedes" {
			continue
		}
		if d.ID != newID {
			return rawIssue{}, false, fmt.Errorf("bd supersede: conflicting or invalid replacement %q", d.ID)
		}
		linked = true
	}
	return old, linked && old.Status == string(Closed), nil
}
