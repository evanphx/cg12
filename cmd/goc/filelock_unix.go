//go:build unix

package main

import (
	"os"
	"syscall"
	"time"
)

// acquireFileLock takes an exclusive advisory lock on path, waiting up to wait
// for it, and returns the release. A false result means the lock was not taken
// and the caller should proceed without it: every caller in this package treats
// the lock as an optimization over an already-correct sequence.
//
// The wait is a poll rather than a blocking flock so that giving up is possible
// at all. A blocking flock with no timeout hands a stuck process the power to
// stop every compile on the machine, and there is no signal to distinguish "still
// building net/http" from "wedged" other than how long it has been.
//
// wait of 0 means try once. Callers that would rather skip the work than wait for
// it -- the cache trim -- use that.
func acquireFileLock(path string, wait time.Duration) (func(), bool) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	release := func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return release, true
		}
		if err != syscall.EWOULDBLOCK || !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, false
		}
		time.Sleep(fileLockPollInterval)
	}
}

// fileLockPollInterval is short against a pack build (seconds to minutes) and
// long against the syscall, so the wait costs nothing measurable and still hands
// the lock over promptly.
const fileLockPollInterval = 100 * time.Millisecond
