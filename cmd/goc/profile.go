package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"
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

// -memprofile writes a heap profile of the compile. It is the counterpart to
// -cpuprofile and exists for the same reason: a whole-program goc build peaks in
// the gigabytes, the bound on how many compiles a machine may run at once is
// derived from that peak, and "which allocation sites hold the gigabytes" is not
// answerable by guessing.
//
// Two moments are worth profiling and they answer different questions, so the
// flag pair covers both:
//
//   - at the end (the default), after an explicit runtime.GC, inuse_space is
//     what the compile still *retains* once it is done. That is the number to
//     read when asking whether whole-module IR is being held alive past the point
//     it could have been freed. The same profile's alloc_space is the compile's
//     whole allocation history, which is what to read for churn.
//   - at the peak (-memprofile-peak), with no GC forced, inuse_space is what was
//     resident at the high-water mark, garbage included. That is the number peak
//     RSS actually follows, and it is not the same set of sites: a pass that
//     allocates and drops a copy of the module shows up here and nowhere else.
//
// Peak tracking samples runtime.ReadMemStats rather than hooking the allocator,
// which costs a brief stop-the-world per sample. At the sampling interval below
// that is noise against a compile measured in tens of seconds, and it is only
// paid when the flag is set.
const (
	peakMemSampleInterval = 50 * time.Millisecond
	// Rewriting the profile on every new high would rewrite it thousands of times
	// while the heap grows smoothly. Only a materially higher peak is worth the
	// write.
	peakMemGrowthFactor = 1.05
)

type peakHeapProfiler struct {
	name string
	once sync.Once
	stop chan struct{}
	done chan struct{}
}

// startPeakHeapProfile begins sampling the heap and rewriting name whenever the
// heap reaches a materially new peak.
func startPeakHeapProfile(name string) (*peakHeapProfiler, error) {
	// Fail on an unwritable path now rather than after the compile has run.
	file, err := os.Create(name)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	profiler := &peakHeapProfiler{
		name: name,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go profiler.sample()
	return profiler, nil
}

func (p *peakHeapProfiler) sample() {
	defer close(p.done)
	ticker := time.NewTicker(peakMemSampleInterval)
	defer ticker.Stop()
	var peak uint64
	var stats runtime.MemStats
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
		}
		runtime.ReadMemStats(&stats)
		if float64(stats.HeapAlloc) < float64(peak)*peakMemGrowthFactor {
			continue
		}
		peak = stats.HeapAlloc
		// A failed write leaves whatever the last successful sample wrote, which
		// is more useful than aborting the compile over a profile.
		if err := writeHeapProfile(p.name, false); err != nil {
			fmt.Fprintf(os.Stderr, "goc: peak heap profile: %v\n", err)
			return
		}
	}
}

// Stop ends sampling, leaving the highest-water profile written so far in place.
// The compile marks the end of the span it wants profiled and a deferred call
// covers the paths that exit early, so this has to tolerate being called twice.
func (p *peakHeapProfiler) Stop() {
	p.once.Do(func() {
		close(p.stop)
		<-p.done
	})
}

// writeHeapProfile writes a heap profile to name. With collect set it forces a
// collection first, so inuse_space counts only what survives -- see the comment
// on the flags above for why that distinction is the whole point.
func writeHeapProfile(name string, collect bool) error {
	if collect {
		runtime.GC()
	}
	file, err := os.Create(name)
	if err != nil {
		return err
	}
	if err := pprof.WriteHeapProfile(file); err != nil {
		file.Close()
		return fmt.Errorf("write a heap profile: %w", err)
	}
	return file.Close()
}

// The subcommands parse their own flag sets, so -cpuprofile and -memprofile had
// to be registered on each of them separately or not exist there at all -- and
// they did not exist there at all. That left the two commands most worth
// profiling unprofilable: `compile-batch`, which is the whole test corpus in one
// process, and `build-runtime`, which is the single most expensive thing goc
// does. Both were being reasoned about from the outside.
//
// A subcommand profiles its whole run rather than a phase inside it. The main
// path stops its heap profile where the module is finished, because there the
// link and the compiled program would otherwise be counted as compile cost;
// here the whole command is the thing being asked about.
type subcommandProfiles struct {
	cpuProfile     *string
	memProfile     *string
	memProfilePeak *bool
	peak           *peakHeapProfiler
}

// registerProfileFlags adds the profiling flags to a subcommand's flag set.
// Call it before Parse; call start after.
func registerProfileFlags(flags *flag.FlagSet) *subcommandProfiles {
	return &subcommandProfiles{
		cpuProfile: flags.String("cpuprofile", "",
			"write a CPU profile of this command to this file"),
		memProfile: flags.String("memprofile", "",
			"write a heap profile of this command to this file"),
		memProfilePeak: flags.Bool("memprofile-peak", false,
			"with -memprofile, profile the peak heap instead of what is retained at the end"),
	}
}

// start begins whatever the flags asked for. A failure to start is reported
// rather than ignored: a command that silently declines to profile is worse
// than one that refuses, because the absence looks like a result.
func (p *subcommandProfiles) start() error {
	if *p.cpuProfile != "" {
		if err := startCPUProfile(*p.cpuProfile); err != nil {
			return err
		}
	}
	if *p.memProfile != "" && *p.memProfilePeak {
		started, err := startPeakHeapProfile(*p.memProfile)
		if err != nil {
			stopCPUProfile()
			return err
		}
		p.peak = started
	}
	return nil
}

// finish writes the profiles out. It tolerates being called twice, because the
// paths that os.Exit have to call it explicitly and the deferred call still
// runs on the paths that return.
func (p *subcommandProfiles) finish() error {
	stopCPUProfile()
	if *p.memProfile == "" {
		return nil
	}
	if p.peak != nil {
		p.peak.Stop()
		p.peak = nil
		return nil
	}
	written := *p.memProfile
	*p.memProfile = ""
	return writeHeapProfile(written, true)
}
