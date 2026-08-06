//go:build !unix

package main

import "time"

// acquireFileLock has no implementation off unix. Reporting "not taken" is the
// correct answer rather than a degradation: the callers all treat the lock as an
// optimization over a sequence that is already safe without it, so a platform
// without flock duplicates work and still produces the right pack.
func acquireFileLock(path string, wait time.Duration) (func(), bool) {
	return nil, false
}
