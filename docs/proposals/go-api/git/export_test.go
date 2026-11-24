package git

// SetSnapshotTreeHook runs f after each tree's capture until restore.
func SetSnapshotTreeHook(f func(tree int)) (restore func()) {
	snapshotTreeHook = f
	return func() { snapshotTreeHook = nil }
}
