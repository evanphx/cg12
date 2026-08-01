package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/evanphx/cg12/goc"
	"github.com/stretchr/testify/require"
)

// Some GC paths cannot be observed from inside the program that reaches them.
// Nothing in runtime/metrics separates a dedicated mark worker from a fractional
// one; nothing tells a program that the frame it was interrupted in was scanned
// conservatively rather than precisely; and madvise(MADV_HUGEPAGE) on the page
// allocator's metadata leaves no trace a Go program can read. RUNTIME_PLAN.md
// section 6 asks for those paths anyway.
//
// The runtime coverage instrumentation answers exactly that question and nothing
// else: it records, per emitted basic block, whether the block executed. Asking
// it whether runtime.scanConservative ran is a direct observation of the path
// rather than an inference from a program's exit status, which is what section 2
// point 3 asks a focused semantic test to be. The cost is one instrumented
// compile per program, which is a few seconds.
//
// These tests are deliberately about reachability of a named runtime function.
// The semantic behaviour of each path is checked by the capability program
// itself, which is compiled and run normally by the matrix; this file only
// establishes that the path the program is named after is the path it took.

// runtimeFunctionExecution compiles a capability program with runtime coverage
// instrumentation, runs it once with the given environment, and reports which of
// the named runtime functions executed.
//
// A function that was never compiled into this executable is reported as a
// failure rather than as "not executed": the two are different, and confusing
// them would let a renamed or unreachable function look like a missing path.
func runtimeFunctionExecution(
	t *testing.T,
	source string,
	environment []string,
	timeout time.Duration,
	names ...string,
) map[string]bool {
	t.Helper()

	compiler := sharedGOCBinary(t)
	directory := t.TempDir()
	program := filepath.Join("..", "..", "goc", "testdata", source)
	executable := filepath.Join(directory, strings.TrimSuffix(source, ".go")+".bin")
	metadataPath := executable + ".runtime-cover.json"

	compile := exec.Command(compiler, "-o", executable, "-runtime-covermeta", metadataPath, program)
	compileOutput, err := compile.CombinedOutput()
	require.NoErrorf(t, err, "compile %s with runtime coverage:\n%s", source, compileOutput)

	run := exec.Command(executable)
	run.Env = append(os.Environ(), environment...)
	timer := time.AfterFunc(timeout, func() {
		if run.Process != nil {
			run.Process.Kill()
		}
	})
	runOutput, runErr := run.CombinedOutput()
	timer.Stop()

	metadataJSON, err := os.ReadFile(metadataPath)
	require.NoError(t, err, "read runtime coverage metadata")
	var metadata goc.RuntimeCoverage
	require.NoError(t, json.Unmarshal(metadataJSON, &metadata), "decode runtime coverage metadata")

	result, err := goc.ExtractRuntimeCoverage(runOutput, &metadata)
	require.NoErrorf(t, err, "extract the coverage packet from %s:\n%s", source, runOutput)
	require.NoErrorf(t, runErr, "run %s:\n%s", source, result.Output)

	entries := make(map[string]int, len(names))
	for _, function := range metadata.Functions {
		entries[function.Name] = function.Entry
	}

	executed := make(map[string]bool, len(names))
	for _, name := range names {
		entry, ok := entries[name]
		require.Truef(t, ok, "%s is not in the runtime coverage metadata; has it been renamed?", name)
		require.Lessf(t, entry, len(result.Hits), "%s reports block %d, outside the bitmap", name, entry)
		executed[name] = result.Hits[entry]
	}
	return executed
}

func requireARM64WithCC(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 Go runtime capability paths")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc unavailable")
	}
}

// The conservative scan is the path a precise stack map cannot describe: a frame
// interrupted between calls has no stack-map index, so scanframe scans the
// asyncPreempt frame and its parent word by word. gc/conservative-preempt is
// written to be interrupted there -- its loops contain no calls at all, so the
// preemption signal is the only thing that can stop them -- but a program cannot
// tell whether it was in fact interrupted, and one that is never preempted still
// exits 0.
func TestConservativePreemptCapabilityReachesTheConservativeScan(t *testing.T) {
	requireARM64WithCC(t)

	executed := runtimeFunctionExecution(
		t,
		"runtime_gc_conservative_preempt.go",
		[]string{"GOMAXPROCS=4"},
		120*time.Second,
		"runtime.scanConservative",
		"runtime.asyncPreempt2",
		"runtime.suspendG",
	)

	require.True(t, executed["runtime.asyncPreempt2"],
		"no goroutine was asynchronously preempted, so the call-free loops were never interrupted")
	require.True(t, executed["runtime.scanConservative"],
		"a frame was never scanned conservatively, so this capability is not reaching the path it is named after")
	require.True(t, executed["runtime.suspendG"],
		"no goroutine's stack was scanned by suspendG")
}

// Mark worker modes. The controller hands a P a dedicated worker while
// dedicatedMarkWorkersNeeded is positive, a fractional worker when the
// utilisation goal is not a whole number of Ps, and an idle worker whenever a P
// would otherwise go to sleep during a mark phase. Each mode has its own drain
// wrapper, which exists precisely so that profiles can tell them apart, and
// nothing in runtime/metrics can: cpuStats.accumulate adds fractional time into
// GCDedicatedTime, so /cpu/classes/gc/mark/dedicated:cpu-seconds covers both.
//
// GOMAXPROCS=4 gives a whole dedicated worker (4 * 0.25 = 1) and no fractional
// one; GOMAXPROCS=3 gives 0.75 of a worker, which is a fractional worker and no
// dedicated one. Running the same program both ways reaches all three modes.
func TestMarkWorkerModesAreAllReached(t *testing.T) {
	requireARM64WithCC(t)

	const (
		dedicated  = "runtime.gcDrainMarkWorkerDedicated"
		fractional = "runtime.gcDrainMarkWorkerFractional"
		idle       = "runtime.gcDrainMarkWorkerIdle"
	)

	whole := runtimeFunctionExecution(
		t,
		"runtime_gc_mark_workers.go",
		[]string{"GOMAXPROCS=4"},
		120*time.Second,
		dedicated, fractional, idle,
	)
	require.Truef(t, whole[dedicated],
		"GOMAXPROCS=4 asks for exactly one dedicated mark worker and none ran")

	fraction := runtimeFunctionExecution(
		t,
		"runtime_gc_mark_workers.go",
		[]string{"GOMAXPROCS=3"},
		120*time.Second,
		dedicated, fractional, idle,
	)
	require.Truef(t, fraction[fractional],
		"GOMAXPROCS=3 asks for three quarters of a mark worker and no fractional worker ran")

	require.Truef(t, whole[idle] || fraction[idle],
		"no idle mark worker ran in either configuration")
}

// GC metadata huge pages. mheap.enableMetadataHugePages is called at the end of a
// cycle whose heap goal crosses minHeapForMetadataHugePages, which is 1 GiB, and
// it madvises the page allocator's chunk bitmaps and the arena L2 tables. Whether
// the kernel then backs them with huge pages is a property of the host that a Go
// program cannot see, so what is asserted here is that the transition ran.
func TestMetadataHugePageTransitionIsReached(t *testing.T) {
	requireARM64WithCC(t)

	executed := runtimeFunctionExecution(
		t,
		"runtime_gc_metadata_hugepages.go",
		[]string{"GOMAXPROCS=4"},
		240*time.Second,
		"runtime.(*mheap).enableMetadataHugePages",
		"runtime.(*pageAlloc).enableChunkHugePages",
	)

	require.True(t, executed["runtime.(*mheap).enableMetadataHugePages"],
		"the heap goal never crossed the metadata huge-page threshold")
	require.True(t, executed["runtime.(*pageAlloc).enableChunkHugePages"],
		"the chunk bitmap huge-page transition never ran")
}

// Sweep, scavenge and assist are reachable from a program, but a program that
// merely allocates does not prove it reached them: the pacer can satisfy a small
// workload out of already-swept spans and never run a mark assist at all. These
// are the functions the three stress capabilities exist to reach.
func TestGCStressCapabilitiesReachPacerAndScavenger(t *testing.T) {
	requireARM64WithCC(t)

	assist := runtimeFunctionExecution(
		t,
		"runtime_gc_assist_credit.go",
		[]string{"GOMAXPROCS=4"},
		180*time.Second,
		"runtime.gcAssistAlloc",
		"runtime.gcAssistAlloc1",
		"runtime.gcFlushBgCredit",
	)
	for name, ran := range assist {
		require.Truef(t, ran, "%s never ran, so gc-stress/assist-credit is not producing assist work", name)
	}

	sweep := runtimeFunctionExecution(
		t,
		"runtime_gc_sweep_pacing.go",
		[]string{"GOMAXPROCS=4"},
		180*time.Second,
		"runtime.deductSweepCredit",
		"runtime.sweepone",
		"runtime.(*mspan).sweep",
	)
	for name, ran := range sweep {
		require.Truef(t, ran, "%s never ran, so gc-stress/sweep-pacing is not exercising proportional sweep", name)
	}

	scavenge := runtimeFunctionExecution(
		t,
		"runtime_gc_scavenge_release.go",
		[]string{"GOMAXPROCS=4"},
		240*time.Second,
		"runtime.(*pageAlloc).scavenge",
		"runtime.(*scavengerState).run",
		"runtime.sysUnusedOS",
	)
	for name, ran := range scavenge {
		require.Truef(t, ran, "%s never ran, so gc-stress/scavenge-release is not returning memory", name)
	}
}

// A negative control for the mechanism itself. The instrumentation reports a
// block as executed only if it executed, so a function that this program cannot
// reach must come back false. Without this the three tests above would pass
// against a bitmap that was all ones.
func TestRuntimeFunctionExecutionReportsUnreachedFunctions(t *testing.T) {
	requireARM64WithCC(t)

	const unreached = "runtime.badmorestackgsignal"
	executed := runtimeFunctionExecution(
		t,
		"hello.go",
		[]string{"GOMAXPROCS=1"},
		60*time.Second,
		unreached,
	)
	require.Falsef(t, executed[unreached],
		"%s is only reachable from a corrupted signal stack; a true here means the bitmap is not being read", unreached)
}
