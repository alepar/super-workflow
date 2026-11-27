//go:build !linux && !darwin

package wf

import "io/fs"

// statIdentity has no portable form here: nothing is cached, every file is
// hashed.
func statIdentity(fs.FileInfo) (hashKey, bool) { return hashKey{}, false }
