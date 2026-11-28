// Package bd is the beads (bd) adapter. Constructors return descriptors and
// nothing here runs them; Runtime (runtime.go) is the wf.AdapterRuntime that
// performs them through the bd CLI.
//
// Reads return wf.Observation values; writes return wf.ExternalEffect values
// whose idempotency Key is the first, positional parameter, so a call without
// a key does not compile. The Engine performs the operations through Runtime
// and the already_done check described on each effect.
package bd

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Status is a bead lifecycle status.
type Status string

// Bead statuses.
const (
	Open       Status = "open"
	InProgress Status = "in_progress"
	Closed     Status = "closed"
)

// Issue is the typed shape of a bead, for reads and for Create.
//
// Parent, NoInheritLabels and RootLabel place a created bead in a tree: Parent
// attaches it to a parent bead, NoInheritLabels stops it inheriting the
// parent's labels, and RootLabel is a label the client's ready query scopes
// by. They are typed fields the adapter always renders, so a client that
// relies on them cannot forget one.
//
// Create sends each of Labels and RootLabel through sink.NewLabel, which
// percent-encodes what bd's label parsing would split or trim, so a label
// reads back in that encoded form. An empty label in Labels, or one holding
// a NUL byte, is refused.
type Issue struct {
	ID              string
	Title           string
	Description     string
	Type            string
	Status          Status
	Priority        int
	Labels          []string
	Parent          string
	NoInheritLabels bool
	RootLabel       string
	// Metadata is the bead's metadata: a JSON object, written with the bead
	// by Create (`bd create --metadata`) and read back by every read that
	// returns beads. It is omitted when empty, so an Issue without it encodes
	// as before.
	Metadata json.RawMessage `json:",omitempty" render:"omitzero"`
}

// IssueKey is the idempotency key for Create: dedupe by label plus title
// (invariant 11).
func IssueKey(label, title string) wf.Key { return wf.KeyOf("bd", label, title) }

// Ready observes `bd ready --json`, scoped to the tree of epic.
//
// Deprecated: Ready applies one client's label convention: it scopes by the
// label sp:<epic> and excludes the labels blocker, sp:review, sp:gate,
// human-gate and human. It stays, with that exact behaviour and descriptor,
// so journals that recorded it replay unchanged. New definitions use ReadyIn
// and pass their own labels.
func Ready(epic string) wf.Observation[[]Issue] {
	return wf.Observe[[]Issue]("bd", "ready", map[string]any{"epic": epic})
}

// ReadyIn observes `bd ready --json`: open, unblocked beads that are not epics
// or gates. A non-empty label keeps only beads carrying it; excludeLabels drop
// beads carrying any of them. Which labels scope and exclude work is the
// client's convention. Labels are the values as given to Create: the adapter
// encodes each one, and refuses an empty exclude label or one holding a NUL
// byte.
func ReadyIn(label string, excludeLabels ...string) wf.Observation[[]Issue] {
	args := map[string]any{"label": label}
	if len(excludeLabels) > 0 {
		// Exclusions holding no comma are recorded joined, as they always
		// were, so a journal's recorded read keeps its descriptor; a comma
		// in one would be ambiguous there, so then they are recorded as a
		// list.
		if slices.ContainsFunc(excludeLabels, func(l string) bool { return strings.Contains(l, ",") }) {
			args["exclude_labels"] = slices.Clone(excludeLabels)
		} else {
			args["exclude_labels"] = strings.Join(excludeLabels, ",")
		}
	}
	return wf.Observe[[]Issue]("bd", "ready", args)
}

// Show observes `bd show <id> --json`.
func Show(id string) wf.Observation[Issue] {
	return wf.Observe[Issue]("bd", "show", map[string]any{"id": id})
}

// ListByLabel observes `bd list --label <label> --json`. label is the value
// as given to Create; the adapter encodes it, and refuses an empty one.
func ListByLabel(label string) wf.Observation[[]Issue] {
	return wf.Observe[[]Issue]("bd", "list", map[string]any{"label": label})
}

// CloseEligible observes `bd epic close-eligible --json` for epic: the ids of
// epics whose children are all closed.
func CloseEligible(epic string) wf.Observation[[]string] {
	return wf.Observe[[]string]("bd", "epic_close_eligible", map[string]any{"epic": epic})
}

// Create files a bead from issue.
//
// Idempotent by key. already_done: the adapter lists beads with the label
// `key:<key>` and the same Title; if one exists it is returned and nothing is
// created. A created bead is stamped with that label. issue.Metadata, when
// set, is written with the bead in the same create; a bead found under the
// key and title whose metadata does not hold it is uncertain, not done.
func Create(key wf.Key, issue Issue) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "bd", Op: "create", Args: map[string]any{"issue": issue}, Key: key}
}

// UpdateStatus sets a bead's status.
//
// Idempotent by nature (setting the same status twice is a no-op).
// already_done: the adapter reads the bead and skips the write when its status
// already equals s.
func UpdateStatus(key wf.Key, id string, s Status) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "bd", Op: "update_status", Args: map[string]any{"id": id, "status": s}, Key: key}
}

// SetMetadata sets metadata fields on a bead: `bd update --metadata <json>
// -- <id>`, the fields as one JSON object bd merges into the bead's
// metadata. The adapter builds the argv, so a value (free text, JSON) is only
// ever a value inside that object and is never read as an option or a
// command (reference workflow S3a). A value is stored as --set-metadata k=v
// stored it: the words true, false and null and a JSON number typed, any
// other value as the string. A key must be a plain word.
//
// Idempotent: the same values are written again.
func SetMetadata(key wf.Key, id string, fields map[string]string) wf.ExternalEffect {
	f := make(map[string]any, len(fields))
	for k, v := range fields {
		f[k] = v
	}
	return wf.ExternalEffect{Adapter: "bd", Op: "set_metadata", Args: map[string]any{"id": id, "fields": f}, Key: key}
}

// DepAdd makes dependent blocked by blocker. reason is the
// `blocked-by <id>: consumes ...` line recorded on the dependent (invariant 5).
//
// Idempotent. already_done: the adapter reads dependent's dependencies and
// skips the write when blocker is already listed.
func DepAdd(key wf.Key, dependent, blocker, reason string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "bd", Op: "dep_add", Args: map[string]any{"dependent": dependent, "blocker": blocker, "reason": reason}, Key: key}
}

// Close closes a bead with a reason.
//
// Idempotent: closing a closed bead is a no-op. already_done: the adapter reads
// the bead and skips the write when its status is already closed.
func Close(key wf.Key, id, reason string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "bd", Op: "close", Args: map[string]any{"id": id, "reason": reason}, Key: key}
}

// Supersede closes oldID and records its replacement as newID.
// already_done: oldID is closed with a supersedes dependency to exactly newID.
// IDs must already be resolved by the caller; amendment policy stays there.
func Supersede(key wf.Key, oldID, newID string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "bd", Op: "supersede", Args: map[string]any{"old": oldID, "new": newID}, Key: key}
}
