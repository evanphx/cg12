package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"sync"
)

// -cpuprofile writes a CPU profile of the compile itself. It exists because the
// compile's cost distribution across phases is the thing worth knowing about a
// compiler driver, and reading it out of a profile beats guessing at it.
//
// The file stays open for the length of the profile, so stopping has to close it
// as well as stop the profiler. os.Exit skips deferred functions, so the paths
// that exit stop the profile explicitly and this has to tolerate being called
// twice.
var (
	cpuProfileMutex sync.Mutex
	cpuProfileFile  *os.File
)

func startCPUProfile(name string) error {
	file, err := os.Create(name)
	if err != nil {
		return err
	}
	if err := pprof.StartCPUProfile(file); err != nil {
		file.Close()
		return fmt.Errorf("start a CPU profile: %w", err)
	}
	cpuProfileMutex.Lock()
	defer cpuProfileMutex.Unlock()
	cpuProfileFile = file
	return nil
}

func stopCPUProfile() {
	cpuProfileMutex.Lock()
	defer cpuProfileMutex.Unlock()
	if cpuProfileFile == nil {
		return
	}
	pprof.StopCPUProfile()
	cpuProfileFile.Close()
	cpuProfileFile = nil
}
