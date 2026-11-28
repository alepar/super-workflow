package wf

import "encoding/json"

// SnapshotRequest identifies a world and an adapter's private checkpoint directory.
// Worktrees lists run-owned checkouts relative to Workspace; StateWorktrees lists
// those relative to StateRoot. Directory is outside captured worktree contents.
// Roots and Directory are absolute; worktree paths are relative to their root.
type SnapshotRequest struct {
	Workspace      string
	StateRoot      string
	Directory      string
	Worktrees      []string
	StateWorktrees []string
	// Exclude lists checkpoint/state directories relative to Workspace.
	Exclude []string
	// Include lists files relative to Workspace that the primary tree's
	// capture carries even when git ignores them: a client's own install
	// records (EngineOptions.SnapshotFiles). One absent at capture is left
	// out; a restore writes each one carried, untracked, as it was.
	Include []string
	// Store, when set, takes a directory into the checkpoint without a
	// copy: the adapter hands it its source instead of copying the source
	// into Directory.
	Store SnapshotStore
}

// SnapshotStore takes src, a directory, into the checkpoint as rel under
// the adapter's Directory, reading it now (the adapter still holds whatever
// keeps it quiescent) and storing only bytes the run's store lacks. skip,
// when set, leaves out the entries it names by base name. A restore finds it
// at Directory/rel, as if the adapter had copied it there.
type SnapshotStore interface {
	PutDir(rel, src string, skip func(name string) bool) error
}

// RestoreRequest restores a captured adapter world into a new workspace. Directory
// names that adapter's saved checkpoint directory, not the original workspace.
type RestoreRequest struct {
	Workspace string
	StateRoot string
	Directory string
	// Omit records something a restore deliberately left out of the fork,
	// such as a tracker's remote (a fork is a sandbox): the fork's rewound
	// record lists each one (RewoundData.Omitted). Nil drops them.
	Omit func(what string) `json:"-"`
}

// WorldSnapshotter is an optional adapter capability. Preflight must check support
// without modifying the world. Snapshot writes only its checkpoint and snapshot
// refs, never source branches or indexes. Restore must not contact or modify the
// source world. Metadata is adapter-owned and must contain no source absolute paths.
// The engine publishes a checkpoint only after every participating adapter succeeds.
type WorldSnapshotter interface {
	SnapshotPreflight(SnapshotRequest) error
	Snapshot(SnapshotRequest) (json.RawMessage, error)
	Restore(RestoreRequest, json.RawMessage) error
}

// WorldStamper is a snapshot adapter that can name, without modifying it,
// everything its capture reads (git: the branch and tag refs and each
// captured tree's HEAD, branch, staged index and working-file contents; bd:
// the tracker's file contents, read under its gates). A capture taken while
// children are live is valid only if every adapter's stamp is the same
// before and after it: the world did not move during the capture, so the
// checkpoint is one instant of it (a consistent cut). Otherwise the offer is
// recorded unavailable. A change undone within the capture (A to B to A) is
// not seen. An adapter without it cannot take part in a live capture.
type WorldStamper interface {
	SnapshotStamp(SnapshotRequest) (string, error)
}
