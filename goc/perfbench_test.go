package goc_test

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// measurePerfBench asks for the runtime performance suite to be run. Opt-in for
// the same reasons TestCryptoSigningBench, TestEscapeDifferentialAgainstGC and
// TestSlogAllocationsAgainstGC are: it needs a host Go toolchain and a system
// linker, and it measures elapsed time, so its answer is partly about what else
// the machine is doing. That is a thing a person runs deliberately on a quiet
// box, not something a parallel `go test ./...` should be deciding.
var measurePerfBench = flag.Bool("perf-bench", false,
	"build every program under goc/testdata/perf_bench and the reused half of goc/testdata/placement_bench with goc and with the host Go toolchain, and compare elapsed time against the committed baseline")

// updatePerfBench rewrites the checked-in baseline from this run.
var updatePerfBench = flag.Bool("update-perf-bench", false,
	"rewrite testdata/perf_suite_baseline.txt from this run")

// perfBenchRepsFlag overrides how many interleaved repetitions each arm gets.
// Raising it narrows the interval the check draws around its answer -- as the
// square root, so it costs four runs to halve it -- and is what to reach for
// when a movement lands near a row's tolerance and the question is whether it is
// real.
var perfBenchRepsFlag = flag.Int("perf-bench-reps", perfBenchReps,
	"how many interleaved repetitions of each arm to time")

// perfBenchOnly restricts the run to some programs, by name. It exists for
// triage: when one row moves, rerunning that one program at high repetitions
// costs a minute instead of eleven. A restricted run refuses to write a
// baseline, because a baseline missing ten programs is worse than none.
var perfBenchOnly = flag.String("perf-bench-only", "",
	"comma-separated program names to run instead of all of them; refuses -update-perf-bench")

const (
	perfBenchBaseline = "testdata/perf_suite_baseline.txt"

	// perfBenchControl is the case every program measures first: a fixed amount
	// of integer arithmetic, byte-identical source in all eleven programs. It is
	// not divided out of anything here -- see the doc comment on
	// TestPerformanceSuite for why this suite compares a goc/host ratio rather
	// than an index -- but it is measured and reported, because eleven copies of
	// one loop in eleven differently laid out binaries is a direct reading of how
	// much program-level code placement moves a number on this box.
	perfBenchControl = "control/spin-fixed-work"
)

// perfBenchReps is how many times each arm is timed, in one run of the check.
//
// Nine, and not one, for the reason the crypto benchmark was rewritten: a single
// elapsed time on this box wanders by a couple of percent for reasons that have
// nothing to do with the compiler, and one number against a fixed bound is a coin
// flip whenever the true value sits near the bound. Nine gives a mean whose 95%
// interval is about three quarters of a single reading's spread.
//
// Nine and not ten because nine is divisible by three, and three is how many arms
// each program has -- the goc binary, the same goc binary again as a null, and
// the host binary. The three are run in a rotated order so that each one occupies
// each position in the rotation exactly three times, which is what keeps a
// first-position or last-position artefact from landing on one arm.
//
// Nine and not more because the cost is linear: one repetition is about a minute
// of the roughly eleven the whole check takes.
const perfBenchReps = 9

// perfBenchMinimumReps is the fewest repetitions that produce an interval worth
// printing. At n = 3 Student's t is already 4.3; below that the interval is wider
// than any regression this suite could see, which would mean a green run that
// proves nothing.
const perfBenchMinimumReps = 4

// perfBenchMinimumTolerance is the floor under every row's tolerance, whatever
// its measured noise says.
//
// It is not there for run-to-run noise -- the per-row tolerance below already
// covers that, from a measurement. It is there for code placement, which the null
// arm cannot see and which is not a regression.
//
// goc aligns the entry of every function containing a loop to 32 bytes, which
// pins a function's phase inside this core's instruction fetch granule. That took
// the placement-induced spread on the crypto benchmark from 6.1% to 0.4% when it
// landed, and an independent measurement on the wave-7 gate put the same quantity
// at 4.25% -> 2.73%. The two agree on the direction and not the size, so the
// residue is low single digits and not zero: alignment fixes a phase, not an
// address, and a function with no loop in it is still laid down wherever the
// previous one ended.
//
// goc/testdata/placement_bench/analysis_shift_phase.txt has the residue per case
// under the shipped policy. Most rows are under 2%; map/build-probe is 2.9% and
// the two text rows are 15% and 22%, which is why those rows get their tolerance
// from their own noise rather than from this floor.
//
// 5% clears the typical residue with room. It is a deliberate loss of
// sensitivity, stated here rather than hidden: this suite does not see a 3%
// regression on a quiet row, and no instrument on this box both sees 3% and does
// not fire at random.
const perfBenchMinimumTolerance = 0.05

// perfBenchNoiseMultiple is how many times a row's own measured noise its
// tolerance is set to, when that is above the floor.
//
// Three sigma of a quantity whose true value is constant within a run. Both
// binaries are fixed once they are built, so the true ratio does not change
// between repetitions and every departure from the mean is this instrument
// measuring itself. A row whose ratio wanders by 7% one repetition to the next --
// text/sprintf does -- cannot be given a 5% bar without firing constantly, and
// pretending otherwise is how a check gets ignored.
//
// The spread that sets the tolerance is the *ratio's*, not the null's, and the
// difference is not academic. The null runs the same goc binary twice, so it sees
// only goc-side noise. Measured on this box, json/marshal's ratio moves 3.7% one
// repetition to the next while its null moves 1.0%, and goroutine/spawn-join's
// ratio moves 5.8% while its null moves 0.09% -- because the host-built binary
// finishes those cases in about 12 ms and it is the *host* run that is jittering.
// A tolerance drawn from the null would have been three to sixty times too tight
// on exactly those rows.
const perfBenchNoiseMultiple = 3.0

// perfBenchNullBiasCeiling is how far the null arm's mean may sit from 1.0000,
// after its own interval is taken off, before the whole run is thrown out.
//
// The null is the same file run twice. Its true ratio is exactly 1. A resolved
// deviation from 1 is therefore not noise -- noise is what the interval already
// absorbed -- it is a systematic artefact of the protocol: one position in the
// rotation running consistently warmer than another, or the machine drifting in
// one direction across the run. The crypto benchmark measured an artefact of that
// kind at 1.28% before it interleaved. 2% is above that and far below any
// regression this suite is set to catch.
const perfBenchNullBiasCeiling = 0.02

// perfBenchNoiseCeiling is the widest one-repetition spread a row may have before
// the whole run is thrown out, whatever a baseline says.
//
// It is the only thing standing between a loud box and a baseline whose
// tolerances are permanently too wide to catch anything, because the growth
// ceiling below needs a baseline to compare against and an -update-perf-bench run
// is exactly the run that has none.
//
// 15% is set from measurement rather than taste. On a quiet box the rows here
// span 0.01% (interp) to about 11% (text/format-append), and the one row that
// ever exceeded 15% -- an early gc/slice-grow that grew a 4 MB slice four times a
// round, at 19% -- was not noisy, it was badly written: whether a collection
// landed inside the timed region was a coin flip. That is the case this ceiling
// is for. A row above it cannot be gated by any tolerance and is a workload to
// fix or drop, not a measurement to widen a band around.
const perfBenchNoiseCeiling = 0.15

// perfBenchNoiseGrowthCeiling is how many times the baseline's measured noise
// this run's may be before the run is thrown out.
//
// This is what keeps a green run meaningful, and it is the job the crypto
// benchmark's fixed precision ceiling does there. The comparison rule -- fail only
// when the interval clears the tolerance band -- has an obvious failure mode: on a
// loaded box the interval grows until nothing can fall outside it, and the check
// turns green precisely when it has stopped working. Here the null arm says
// directly how noisy the box is today, in the same units the baseline recorded,
// so the run can compare the two and say "this box is not the box that produced
// the baseline" rather than passing everything.
//
// It watches the ratio's spread, which is the statistic the tolerances come from.
// The null's spread is watched too, and reported, but only as the diagnostic that
// says whether the noise is goc's side or the host's.
const perfBenchNoiseGrowthCeiling = 3.0

// perfBenchRunAttempts is how many times one run of one binary is tried before
// the whole suite gives up on it.
//
// It was built for a defect this tree had written down: a goc-built
// goc/testdata/placement_bench/flate died in the collector on about one run in
// fifteen. That defect is fixed -- a slice expression that consumed its whole
// source pointed one byte past the end of the allocation, so the collector was
// handed a pointer it rejects and the buffer was freed under a live slice; see
// CCWORK_REPORT.md, "Root cause: a slice expression that consumes its source
// points past it". flate is 0 crashes in 600 runs since.
//
// The retry stays, because what it buys is not tolerance of that defect. It is
// that one dead run does not cost the other ten programs their eleven minutes:
// a crash is not a slow measurement, it is a missing one, so replacing it and
// then failing at the end with the whole picture beats dying at repetition five
// with no table at all.
//
// What must not happen is the retry hiding a crash, so every one is logged and
// perfBenchCrashCeiling now fails the run on any of them.
const perfBenchRunAttempts = 3

// perfBenchCrashCeiling is the fraction of one program's runs that may die
// before the suite fails rather than reports.
//
// Zero. It was 20%, five times the flate rate, chosen so it would not fire on a
// defect that was known and live. Nothing in this suite is expected to crash any
// more, and a ceiling that excuses one run in six would let a new collector bug
// print a green table for a long time. A run that dies is now a failure with the
// dead run's stderr attached, whatever it was.
const perfBenchCrashCeiling = 0.0

// perfBenchNoiseGrowthFloor is the absolute noise below which the growth ceiling
// is not applied. A row whose baseline noise is 0.05% and whose run noise is 0.2%
// has quadrupled and is still silent; complaining about it would be arithmetic,
// not measurement.
const perfBenchNoiseGrowthFloor = 0.01

// perfBenchProgram is one workload: a self-contained program that both compilers
// build from the same source, prints one `case<TAB>nanoseconds` line per case,
// and measures a control first.
type perfBenchProgram struct {
	name   string
	source string
	// presses is what this workload is in the suite for, in one line. It is
	// printed into the baseline, because a corpus whose reason for existing is
	// only in a commit message stops being a corpus and becomes a pile.
	presses string
}

// perfBenchPrograms is the workload set.
//
// Seven of them are goc/testdata/placement_bench, reused unmodified. That corpus
// was built for a different question -- how much does a benchmark's number depend
// on where its code landed -- but it was built to this method, its programs are
// deliberately unlike one another, and its committed sweep
// (analysis_shift_phase.txt) already says what each case's placement residue is.
// Reusing it rather than copying it means the placement sweep and this suite
// measure the same binaries, so when a row here moves and the question is whether
// it is placement, the answer is already in the tree for that exact program.
//
// Four are new, for what that corpus does not press: memory latency, the
// scheduler, the allocator and collector, and floating point.
//
// p256 is in placement_bench and deliberately not here. `make bench-crypto`
// already gates that path with its own baseline and its own triage note, and two
// gates on one path means two red lights for one cause.
var perfBenchPrograms = []perfBenchProgram{
	{"interp", "testdata/placement_bench/interp/main.go",
		"a bytecode interpreter: a switch dispatch loop, the shape a real interpreter has"},
	{"sha", "testdata/placement_bench/sha/main.go",
		"SHA-256 and HMAC over a buffer: one tight block loop, and the same assembly under both compilers"},
	{"regexp", "testdata/placement_bench/regexp/main.go",
		"regexp matching: a second, larger interpreter over a pointer-linked program"},
	{"json", "testdata/placement_bench/json/main.go",
		"encoding/json round trip: reflection, interface dispatch and a hand-written scanner"},
	{"sortmap", "testdata/placement_bench/sortmap/main.go",
		"sort.Slice and map build/probe: indirect calls through a comparison callback, and hashing"},
	{"flate", "testdata/placement_bench/flate/main.go",
		"compress/flate round trip: table-driven loops over byte slices"},
	{"text", "testdata/placement_bench/text/main.go",
		"strconv, fmt and strings.Builder: string building and formatting"},
	{"chase", "testdata/perf_bench/chase/main.go",
		"dependent loads at three cache depths: the only workload here bound by memory latency"},
	{"conc", "testdata/perf_bench/conc/main.go",
		"goroutines, channels and mutexes: cost paid inside goc's runtime rather than in emitted code"},
	{"gcpress", "testdata/perf_bench/gcpress/main.go",
		"allocation churn, a live heap, the write barrier and slice growth: what an allocation costs"},
	{"float", "testdata/perf_bench/float/main.go",
		"floating-point arithmetic: a separate register file and a separate set of lowering decisions"},
}

const perfBenchHeader = `# goc against the host Go toolchain, on eleven workloads. Regenerate with
#
#     make bench-perf-update
#
# and read the diff; check it with
#
#     make bench-perf
#
# The run needs a Go toolchain and a system linker, and takes about eleven
# minutes. Run it on an idle machine: like the crypto benchmark and unlike every
# other baseline in this directory it measures time, so a loaded box produces a
# number about the box. If another timing benchmark is running on the same
# machine, set GOC_PERF_CORE to a core it is not using.
#
# # What is compared, and why it is a ratio and not a time
#
# Both columns come from the same program compiled by both compilers, and the
# gated number is
#
#     ratio = goc nanoseconds / host nanoseconds
#
# formed inside one repetition, from two runs seconds apart on the same pinned
# core, and then averaged over the repetitions. Machine speed divides out of it,
# so the file is checkable on a box other than the one that produced it, and --
# unlike an index formed against a control measured in the goc binary -- it does
# not hide a change that made goc's control loop slower too. The crypto
# benchmark uses that index and says so; this suite needs the other property,
# because a change that costs 5% everywhere is exactly what it exists to catch.
#
# The raw nanoseconds are printed alongside so the ratio can be sanity-checked.
# They are not compared; they are expected to differ between machines.
#
# # The noise columns, and the null
#
# Both binaries are fixed once built, so a row's true ratio does not change
# between repetitions and every departure from its mean is this instrument
# measuring itself. ratio-sd% is that departure -- one repetition's spread, not
# the mean's -- and it is each row's noise floor and where its tolerance comes
# from.
#
# The null is a second reading of the same thing from a different angle: every
# repetition runs the goc binary twice and divides one run by the other. It is
# the same file, so its true value is exactly 1.0000. Two things come out of it.
# A run whose null is not 1.0000 to within its own interval has a systematic
# artefact in how its runs were ordered, and none of its other columns should be
# believed. And null-sd% against ratio-sd% says which side the noise is on: the
# null sees goc-side noise only, so a row that is loud in ratio-sd% and quiet in
# null-sd% is a row whose *host* binary is jittering -- usually because the host
# finishes that case in a few milliseconds.
#
# # tol% and detect%
#
# tol% is how far this row's ratio may move from the committed value before the
# run fails: three times ratio-sd%, or 5%, whichever is larger. The 5% floor is
# for code placement, which neither noise column can see because both use one
# pair of binaries, and which is not a regression; see goc/perfbench_test.go.
#
# detect% is the smallest movement this row can actually fail on, which is tol%
# plus the interval a run draws around its own answer. It is the honest answer to
# "what does a green run mean for this row". A row with a large detect% is not
# blind below it, only silent: the baseline is committed, so a movement smaller
# than detect% still appears as a changed number the next time someone runs
# -update-perf-bench.
#
# # A movement is not automatically a regression
#
# There are three causes and they are distinguishable. An allocation moved, the
# generated code changed, or the code did not change and moved. goc aligns the
# entry of every function containing a loop to 32 bytes, which pins its phase in
# the 32-byte fetch granule, but two builds still land code in different cache
# sets. goc/perfbench_test.go's failure message has the commands that tell the
# three apart.
#
`

// TestPerformanceSuite holds goc's elapsed cost, relative to the host Go
// toolchain, to a committed baseline across eleven workloads.
//
// # Why this exists
//
// This tree has good allocation instruments and, before this file, one runtime
// instrument: `make bench-crypto`, on the ECDSA signing path. So a change that
// quietly cost 5% everywhere outside crypto would land green, and performance
// work anywhere else was unmeasurable. This is the thing that lets performance
// work be parallelised and judged.
//
// # What is measured
//
// The gated quantity is goc nanoseconds divided by host nanoseconds on the same
// case, paired inside one repetition. The absolute times matter much less than
// that ratio: "goc is 1.63x the host on a dependent integer loop, +/- 0.2%" is a
// fact someone can act on, and "goc took 54 ms" is not.
//
// It is a ratio against the host and not an index against a control measured in
// the goc binary, which is what the crypto benchmark compares. Both remove the
// machine. Only the ratio survives a change that moved the control too, and a
// change that costs a few percent everywhere is the specific thing this suite is
// for. The cost of the choice is that the host toolchain is now part of the
// baseline; its version is recorded at the top of the file and a movement in it
// is reported separately from a movement in goc.
//
// # How it measures, and why that shape
//
// The protocol is the crypto benchmark's, which is the tree's own answer to an
// instrument that failed 3 of 7 runs on a byte-identical tree:
//
//   - Repetitions, not one run. Each arm is timed perfBenchReps times and what is
//     compared is the mean.
//   - Interleaved. One run of every arm per repetition, with the order of the
//     three arms rotated, so drift over eleven minutes lands on all of them
//     equally instead of on whichever ran last.
//   - Pinned. Every run is `taskset -c N` on one core. On this box that was worth
//     more than everything else put together: the same pair of byte-identical
//     binaries spread 3.03% unpinned and 1.83% pinned.
//   - Warm-up discarded, which every program does: one untimed round, then the
//     fastest of three timed rounds inside the process.
//   - An interval, not a point, on every row, committed and used by the
//     comparison.
//
// and adds one thing the crypto benchmark does not have: a null arm. The goc
// binary is run twice per repetition and one run is divided by the other. It is
// the same file, so the answer is 1.0000 by construction, and the spread around
// it is this instrument's noise measured in the same units and in the same run as
// the thing it is used to judge. Each row's tolerance is derived from it, so a
// quiet row gets a tight bar and a noisy row is not asked to meet one it cannot.
//
// # Why it fails in both directions
//
// Slower is the obvious news. Faster is news too, for the same reason the
// allocation census fails on an object that moves to the frame: the cheap way to
// make Go code faster is to stop heap-allocating something, and "stopped
// heap-allocating something" is indistinguishable from here between an escape
// analysis that got better and one that got permissive about an object which
// outlives its frame. The census says which sites moved; this says what it cost.
// A drop here with no corresponding census line is the combination that should
// worry someone.
//
// # What it is not
//
// It is not a claim that goc should match the host toolchain. Several rows are
// far from 1.00 for reasons that are not defects -- the host has hand-written
// assembly for P-256 that goc does not use, and goc's runtime is younger than
// its code generator. The baseline's job is to hold each row where it is and say
// when it moved, not to assert where it ought to be.
func TestPerformanceSuite(t *testing.T) {
	if !*measurePerfBench && !*updatePerfBench {
		t.Skip("pass -perf-bench to build the performance corpus with both compilers and compare elapsed time")
	}
	reps := *perfBenchRepsFlag
	require.GreaterOrEqual(t, reps, perfBenchMinimumReps,
		"-perf-bench-reps=%d would produce an interval too wide to mean anything; %d is the fewest this check accepts",
		reps, perfBenchMinimumReps)

	programs := selectPerfBenchPrograms(t)
	restricted := len(programs) != len(perfBenchPrograms)
	require.False(t, restricted && *updatePerfBench,
		"-perf-bench-only and -update-perf-bench together would write a baseline missing every program the filter dropped")

	goVersion := hostGoVersion(t)
	t.Logf("host toolchain: %s", goVersion)

	scratch := t.TempDir()
	driver := buildGocDriver(t, scratch)
	binaries := make([]perfBenchBinaries, 0, len(programs))
	for _, program := range programs {
		binaries = append(binaries, buildPerfBenchProgram(t, scratch, driver, program))
	}

	pin, pinNote := perfBenchPin()
	t.Logf("timing protocol: %d interleaved repetitions of three arms (goc, goc again as a null, host), %s",
		reps, pinNote)

	gocRuns, nullRuns, gcRuns := runPerfBenchInterleaved(t, binaries, pin, reps)
	rows := perfBenchRows(t, programs, gocRuns, nullRuns, gcRuns)

	protocol := fmt.Sprintf("%d interleaved repetitions, %s, mean with a 95%% interval", reps, pinNote)
	rendered := renderPerfBench(goVersion, protocol, programs, rows)
	t.Logf("this run:\n%s", rendered)

	// Before the baseline is written and before it is compared: a run that could
	// not resolve its own answer is not one to accept a baseline from either.
	checkPerfBenchInstrument(t, rows)

	if *updatePerfBench {
		require.NoError(t, os.WriteFile(perfBenchBaseline, []byte(rendered), 0o644))
		t.Skip("baseline rewritten; rerun without -update-perf-bench to check it")
	}

	accepted, err := os.ReadFile(perfBenchBaseline)
	require.NoError(t, err)
	comparePerfBench(t, parsePerfBenchBaseline(string(accepted)), rows, restricted)
}

// selectPerfBenchPrograms applies -perf-bench-only.
func selectPerfBenchPrograms(t *testing.T) []perfBenchProgram {
	t.Helper()

	if strings.TrimSpace(*perfBenchOnly) == "" {
		return perfBenchPrograms
	}
	wanted := map[string]bool{}
	for _, name := range strings.Split(*perfBenchOnly, ",") {
		wanted[strings.TrimSpace(name)] = true
	}
	var selected []perfBenchProgram
	for _, program := range perfBenchPrograms {
		if wanted[program.name] {
			selected = append(selected, program)
			delete(wanted, program.name)
		}
	}
	require.Empty(t, wanted, "-perf-bench-only named a program this suite does not have")
	require.NotEmpty(t, selected)
	return selected
}

// perfBenchBinaries is one workload compiled by both compilers.
type perfBenchBinaries struct {
	program perfBenchProgram
	goc     string
	gc      string
}

// perfBenchKey identifies a row. The case name alone is not enough: every
// program measures control/spin-fixed-work, which is the point of it.
type perfBenchKey struct {
	program string
	name    string
}

// perfBenchRow is one case's answer.
type perfBenchRow struct {
	key perfBenchKey
	// ratio is goc nanoseconds over host nanoseconds, and ratioHalfWidth is the
	// 95% half-width of that mean, in the same units.
	ratio, ratioHalfWidth float64
	// ratioSpread is the relative standard deviation of the ratio's repetitions:
	// one reading's noise, not the mean's, because what a tolerance has to clear
	// is what one run can do. Both binaries are fixed, so the true ratio is
	// constant and all of this is the instrument.
	ratioSpread float64
	// null is the goc binary over itself. Its true value is 1.
	null, nullHalfWidth float64
	// nullSpread is the same statistic for the null, which sees goc-side noise
	// only. Reported and not gated on: it is what says whether a noisy row is
	// noisy because of goc or because of the host binary it is divided by.
	nullSpread float64
	tolerance  float64
	// gocNanos and gcNanos are means, printed for sanity and not compared.
	gocNanos, gcNanos float64
}

// detectable is the smallest movement this row can fail on: its tolerance plus
// the interval a comparison draws around the difference of two runs like this
// one. It is what a green run means for this row.
func (row perfBenchRow) detectable() float64 {
	if row.ratio == 0 {
		return row.tolerance
	}
	return row.tolerance + math.Sqrt2*row.ratioHalfWidth/row.ratio
}

func buildGocDriver(t *testing.T, scratch string) string {
	t.Helper()

	driver := filepath.Join(scratch, "goc")
	build := exec.Command("go", "build", "-o", driver, "github.com/evanphx/cg12/cmd/goc")
	build.Env = append(os.Environ(), "GOFLAGS=")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "could not build the goc driver:\n%s", output)
	return driver
}

// buildPerfBenchProgram compiles one workload with goc -O and with the host
// toolchain, into scratch.
//
// -O and not the default arm, for the crypto benchmark's reason: this measures
// generated code, and the optimized arm is the one whose speed is worth
// watching.
//
// Both outputs go to scratch, which is t.TempDir(). That is not tidiness. `goc
// -o` writing into the working directory is how seven binaries have been
// committed to this repository by accident.
func buildPerfBenchProgram(t *testing.T, scratch string, driver string, program perfBenchProgram) perfBenchBinaries {
	t.Helper()

	gcBinary := filepath.Join(scratch, program.name+".gc")
	build := exec.Command("go", "build", "-o", gcBinary, program.source)
	build.Env = append(os.Environ(), "GOFLAGS=")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "the host toolchain would not build %s:\n%s", program.source, output)

	gocBinary := filepath.Join(scratch, program.name+".goc")
	compile := exec.Command(driver, "-O", "-o", gocBinary, program.source)
	output, err = compile.CombinedOutput()
	require.NoError(t, err, "goc would not build %s:\n%s", program.source, output)

	return perfBenchBinaries{program: program, goc: gocBinary, gc: gcBinary}
}

// perfBenchPin returns the command prefix that runs a binary on a single core,
// and the note about it that goes in the log and in the baseline.
//
// Pinning is the single largest thing that can be done to this instrument for
// free: measured on this box with two byte-identical binaries, the same-source
// range over twenty runs was 1.83% pinned and 3.03% unpinned.
//
// Which core: the second-highest this process is allowed on, where the crypto
// benchmark takes the highest. That is deliberate and it is the whole reason
// this function is not a call to that one. Both suites measure time, both are
// run on a shared box, and two runs that pick the same core would measure each
// other instead of the compiler. Offsetting by one lets `make bench-crypto` and
// `make bench-perf` run at the same time. GOC_PERF_CORE overrides it, which is
// what to set when a third timing job is running; GOC_PERF_CORE="" turns pinning
// off, which is honest and costs about a factor of two in spread.
//
// Core 0 is avoided when there is any choice, because it is where most kernels
// land interrupts.
func perfBenchPin() (prefix []string, note string) {
	requested, overridden := os.LookupEnv("GOC_PERF_CORE")
	if overridden && requested == "" {
		return nil, "not pinned (GOC_PERF_CORE is empty)"
	}
	taskset, err := exec.LookPath("taskset")
	if err != nil {
		return nil, "not pinned (taskset is not on PATH), so expect about twice the spread"
	}
	if overridden {
		return []string{taskset, "-c", requested}, "pinned to core " + requested + " (GOC_PERF_CORE)"
	}
	cores, err := allowedCPUs()
	if err != nil {
		return nil, "not pinned (" + err.Error() + "), so expect about twice the spread"
	}
	core := cores[len(cores)-1]
	if len(cores) > 1 {
		core = cores[len(cores)-2]
	}
	return []string{taskset, "-c", strconv.Itoa(core)},
		fmt.Sprintf("pinned to core %d (the second of %d this process is allowed, leaving the top one to make bench-crypto)", core, len(cores))
}

// allowedCPUs returns every core in this process's CPU affinity, ascending.
//
// Reading the affinity rather than assuming runtime.NumCPU(): under a cpuset
// cgroup -- which is how this repository's jobs are run on a shared box -- the
// process may own a slice of the machine and not the top of it, and picking a
// core outside the slice makes taskset fail rather than pin.
func allowedCPUs() ([]int, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, fmt.Errorf("could not read this process's CPU affinity: %w", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		list, ok := strings.CutPrefix(line, "Cpus_allowed_list:")
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(list)
		var cores []int
		for _, span := range strings.Split(trimmed, ",") {
			first, last, ranged := strings.Cut(span, "-")
			if !ranged {
				last = first
			}
			low, lowErr := strconv.Atoi(strings.TrimSpace(first))
			high, highErr := strconv.Atoi(strings.TrimSpace(last))
			if lowErr != nil || highErr != nil || high < low {
				return nil, fmt.Errorf("could not parse Cpus_allowed_list %q", trimmed)
			}
			for core := low; core <= high; core++ {
				cores = append(cores, core)
			}
		}
		if len(cores) == 0 {
			return nil, fmt.Errorf("Cpus_allowed_list %q named no core", trimmed)
		}
		sort.Ints(cores)
		return cores, nil
	}
	return nil, fmt.Errorf("/proc/self/status has no Cpus_allowed_list line")
}

// runPerfBenchInterleaved times three arms of every program, reps times each.
//
// The three arms are the goc binary, the same goc binary again -- the null -- and
// the host binary. Their order is rotated by repetition and by program, so that
// over a run divisible by three each arm sits in each position of the rotation
// equally often. That matters more here than it would with two arms: the null's
// whole value is that its true answer is 1, and a protocol where one arm always
// ran first would put a first-position artefact straight into the number the
// tolerances are derived from.
//
// The program order is rotated too, so that a program is not always measured at
// the same point in a repetition's minute.
//
// Every run is a fresh process, which is the unit of variation being averaged
// over: the same binary run twice differs in where its heap lands, what the
// allocator's size classes look like when the timed round starts, and which way
// the branch predictor was leaning, and none of those is a property of the
// compiler.
func runPerfBenchInterleaved(t *testing.T, binaries []perfBenchBinaries, pin []string, reps int) (gocRuns, nullRuns, gcRuns []map[perfBenchKey]float64) {
	t.Helper()

	attempts := map[string]int{}
	crashes := map[string]int{}
	for rep := 0; rep < reps; rep++ {
		goc := map[perfBenchKey]float64{}
		null := map[perfBenchKey]float64{}
		gc := map[perfBenchKey]float64{}
		for offset := range binaries {
			index := (offset + rep) % len(binaries)
			binary := binaries[index]
			arms := []struct {
				path string
				into map[perfBenchKey]float64
			}{
				{binary.goc, goc},
				{binary.goc, null},
				{binary.gc, gc},
			}
			rotation := (rep + index) % len(arms)
			for step := range arms {
				arm := arms[(rotation+step)%len(arms)]
				runPerfBenchArm(t, binary.program, arm.path, pin, arm.into, attempts, crashes)
			}
		}
		gocRuns = append(gocRuns, goc)
		nullRuns = append(nullRuns, null)
		gcRuns = append(gcRuns, gc)
		t.Logf("repetition %d of %d done", rep+1, reps)
	}
	checkPerfBenchCrashes(t, attempts, crashes)
	return gocRuns, nullRuns, gcRuns
}

// runPerfBenchArm runs one binary once, retrying a run that died.
//
// A crashed run is a missing measurement, not a slow one, so replacing it does
// not bias the sample the way discarding a slow run would. Every retry is counted
// and logged; checkPerfBenchCrashes decides whether the count is the rate the
// tree already knows about or something new.
func runPerfBenchArm(t *testing.T, program perfBenchProgram, binary string, pin []string, into map[perfBenchKey]float64, attempts, crashes map[string]int) {
	t.Helper()

	var failures []string
	for attempt := 0; attempt < perfBenchRunAttempts; attempt++ {
		attempts[program.name]++
		times, err := runPerfBenchOnce(program, binary, pin)
		if err == nil {
			for key, nanos := range times {
				into[key] = nanos
			}
			return
		}
		crashes[program.name]++
		failures = append(failures, err.Error())
		t.Logf("%s: a run died and is being retried (attempt %d of %d): %v",
			program.name, attempt+1, perfBenchRunAttempts, err)
	}
	require.Fail(t, "a binary died on every attempt",
		"%s did not complete a single run in %d attempts, so this repetition has no reading of it and the\n"+
			"suite cannot go on. No program in this suite is expected to crash: the collector crash that goc-built\n"+
			"flate used to die of is fixed (CCWORK_REPORT.md, \"Root cause: a slice expression that consumes its\n"+
			"source points past it\"), and flate has run 600 times since without one. A goc-built binary that has\n"+
			"started dying matters more than any timing in this table -- read the stderr below first.\n\n%s",
		program.name, perfBenchRunAttempts, strings.Join(failures, "\n\n"))
}

// checkPerfBenchCrashes fails the run when a program died at all.
func checkPerfBenchCrashes(t *testing.T, attempts, crashes map[string]int) {
	t.Helper()

	var dying []string
	names := make([]string, 0, len(crashes))
	for name := range crashes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rate := float64(crashes[name]) / float64(attempts[name])
		t.Logf("%s: %d of %d runs died and were retried (%.1f%%)", name, crashes[name], attempts[name], rate*100)
		if rate > perfBenchCrashCeiling {
			dying = append(dying, fmt.Sprintf("%s\n      %d of %d runs died (%.1f%%)",
				name, crashes[name], attempts[name], rate*100))
		}
	}
	require.Empty(t, dying,
		"a program's binary died. Nothing in this suite is expected to: goc-built flate used to crash in the\n"+
			"collector on about one run in fifteen, which is why runs are retried at all, and that defect is fixed\n"+
			"(CCWORK_REPORT.md, \"Root cause: a slice expression that consumes its source points past it\").\n"+
			"A goc-built binary that has started dying is a bigger result than any timing here -- the retry only\n"+
			"exists so this run still produced a table. Triage the crash first and come back to the numbers.\n  %s",
		strings.Join(dying, "\n  "))
}

// runPerfBenchOnce runs one binary once and returns every case it printed.
//
// One process for all of a program's cases, where the slog comparison uses one
// process per case: there, a case could kill the program and take the rest of the
// table with it, and the cases are cheap. Here every case shares the program's
// setup, which for chase is a 64 MiB ring and for gcpress is a live tree, and
// paying that once per case would triple the run.
func runPerfBenchOnce(program perfBenchProgram, binary string, pin []string) (map[perfBenchKey]float64, error) {
	argv := append(append([]string{}, pin...), binary)
	command := exec.Command(argv[0], argv[1:]...)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, perfBenchTail(stderr.String()))
	}

	times := map[perfBenchKey]float64{}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 2 {
			continue
		}
		nanos, parseError := strconv.ParseFloat(fields[1], 64)
		if parseError != nil {
			return nil, fmt.Errorf("%s printed an unparseable time for %s: %q", binary, fields[0], fields[1])
		}
		if nanos == 0 {
			return nil, fmt.Errorf("%s reported zero nanoseconds for %s, which no case can take", binary, fields[0])
		}
		times[perfBenchKey{program: program.name, name: fields[0]}] = nanos
	}
	if len(times) == 0 {
		return nil, fmt.Errorf("%s printed no case at all", binary)
	}
	if _, ok := times[perfBenchKey{program: program.name, name: perfBenchControl}]; !ok {
		return nil, fmt.Errorf("%s printed no %s row, so the run has no reading of the machine", binary, perfBenchControl)
	}
	return times, nil
}

// perfBenchTail keeps the last few lines of a dead run's stderr. A goc runtime
// fault prints a long traceback and the diagnosis is in the first lines of the
// fault, which are the last lines before the traceback.
func perfBenchTail(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > 12 {
		lines = lines[:12]
	}
	return strings.Join(lines, "\n")
}

// perfBenchRows turns the three sets of timings into the compared table.
//
// The ratio is formed per repetition and then averaged, not formed once out of
// two averages. A repetition's two readings are seconds apart on the same core;
// dividing them there is what removes the machine, and averaging the ratios keeps
// that pairing. Averaging first and dividing after would divide one repetition's
// goc by another repetition's host.
func perfBenchRows(t *testing.T, programs []perfBenchProgram, goc, null, gc []map[perfBenchKey]float64) []perfBenchRow {
	t.Helper()

	order := map[string]int{}
	for index, program := range programs {
		order[program.name] = index
	}
	keys := make([]perfBenchKey, 0, len(goc[0]))
	for key := range goc[0] {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if order[keys[i].program] != order[keys[j].program] {
			return order[keys[i].program] < order[keys[j].program]
		}
		return keys[i].name < keys[j].name
	})

	rows := make([]perfBenchRow, 0, len(keys))
	for _, key := range keys {
		require.Contains(t, gc[0], key, "the host-built %s did not report %s", key.program, key.name)
		row := perfBenchRow{key: key}
		ratios := perfBenchQuotients(goc, gc, key)
		row.ratio, row.ratioHalfWidth = meanAndHalfWidth(ratios)
		row.ratioSpread = relativeStandardDeviation(ratios)
		nulls := perfBenchQuotients(goc, null, key)
		row.null, row.nullHalfWidth = meanAndHalfWidth(nulls)
		row.nullSpread = relativeStandardDeviation(nulls)
		row.tolerance = math.Max(perfBenchNoiseMultiple*row.ratioSpread, perfBenchMinimumTolerance)
		row.gocNanos, _ = meanAndHalfWidth(perfBenchTimes(goc, key))
		row.gcNanos, _ = meanAndHalfWidth(perfBenchTimes(gc, key))
		rows = append(rows, row)
	}
	return rows
}

func perfBenchQuotients(numerator, denominator []map[perfBenchKey]float64, key perfBenchKey) []float64 {
	quotients := make([]float64, 0, len(numerator))
	for rep := range numerator {
		below := denominator[rep][key]
		if below == 0 {
			continue
		}
		quotients = append(quotients, numerator[rep][key]/below)
	}
	return quotients
}

func perfBenchTimes(runs []map[perfBenchKey]float64, key perfBenchKey) []float64 {
	times := make([]float64, 0, len(runs))
	for _, run := range runs {
		times = append(times, run[key])
	}
	return times
}

// relativeStandardDeviation returns the sample standard deviation as a fraction
// of the mean.
//
// The standard deviation of the observations and not of their mean: this is used
// to set a tolerance, and what a tolerance has to clear is how far one reading
// can land from the truth, which does not shrink as more readings are taken.
func relativeStandardDeviation(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	mean := 0.0
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	if mean == 0 {
		return 0
	}
	sumSquares := 0.0
	for _, value := range values {
		sumSquares += (value - mean) * (value - mean)
	}
	return math.Sqrt(sumSquares/float64(len(values)-1)) / math.Abs(mean)
}

// checkPerfBenchInstrument checks the instrument before anything measured with
// it is believed.
//
// Two questions, both of which have a wrong answer that would otherwise show up
// as a green run:
//
//   - is the null still 1? It is the same file measured against itself. A
//     deviation that survives its own interval is a systematic artefact of the
//     protocol, and every tolerance in the run is derived from that arm.
//   - is any row so noisy that no tolerance could gate it? A row's tolerance is
//     three times its own spread, so a very noisy row silently buys itself a very
//     wide band and then passes everything. Past a ceiling that stops being a
//     measurement and starts being a workload that needs fixing.
func checkPerfBenchInstrument(t *testing.T, rows []perfBenchRow) {
	t.Helper()

	var biased, unusable []string
	for _, row := range rows {
		if resolved := math.Abs(row.null-1) - row.nullHalfWidth; resolved > perfBenchNullBiasCeiling {
			biased = append(biased, fmt.Sprintf("%s %s\n      null %.4f +/- %.2f%%, which is %.2f%% from 1.0000 after its own interval (ceiling %.2f%%)",
				row.key.program, row.key.name, row.null, row.nullHalfWidth/row.null*100,
				resolved*100, perfBenchNullBiasCeiling*100))
		}
		if row.ratioSpread > perfBenchNoiseCeiling {
			unusable = append(unusable, fmt.Sprintf("%s %s\n      ratio %.4f, one-repetition spread %.1f%% (ceiling %.1f%%), null spread %.1f%%",
				row.key.program, row.key.name, row.ratio, row.ratioSpread*100,
				perfBenchNoiseCeiling*100, row.nullSpread*100))
		}
	}

	require.Empty(t, biased,
		"the null arm is not 1.0000. It is the same goc binary run twice per repetition, so its true value is\n"+
			"exactly 1 and a deviation that survives its own interval is not noise -- it is a systematic artefact\n"+
			"of how the runs were ordered or of the machine drifting in one direction across the run. That makes\n"+
			"this a run whose other columns should not be believed. Rerun on an idle box; if it persists, the\n"+
			"rotation in runPerfBenchInterleaved is not balancing the arms.\n  %s",
		strings.Join(biased, "\n  "))

	require.Empty(t, unusable,
		"a row's own spread is past the ceiling, which means no tolerance can gate it: its band is three times\n"+
			"its noise, so it would pass anything. A green run including this row would be a green run that proved\n"+
			"nothing about it.\n\n"+
			"Look at the null spread beside it first. If the null is quiet and the ratio is not, the noise is in\n"+
			"the host-built binary, which usually means the case finishes too fast under the host toolchain for a\n"+
			"stable reading -- make the case do more work. If both are loud, either the box is busy (check that\n"+
			"nothing else is pinned to the same core; see GOC_PERF_CORE) or the case is at the mercy of when a\n"+
			"collection lands inside its timed region, which is a workload to rewrite rather than a band to\n"+
			"widen.\n  %s",
		strings.Join(unusable, "\n  "))
}

func renderPerfBench(goVersion, protocol string, programs []perfBenchProgram, rows []perfBenchRow) string {
	var out strings.Builder
	out.WriteString(perfBenchHeader)
	out.WriteString("# # The workloads, and what each is for\n#\n")
	for _, program := range programs {
		fmt.Fprintf(&out, "#   %-9s %s\n", program.name, program.presses)
	}
	out.WriteString("#\n")
	fmt.Fprintf(&out, "host toolchain: %s\n", goVersion)
	fmt.Fprintf(&out, "protocol:       %s\n", protocol)
	fmt.Fprintf(&out, "ratio:          goc nanoseconds / host nanoseconds, paired inside one repetition\n")
	fmt.Fprintf(&out, "null:           the goc binary over itself, whose true value is exactly 1.0000\n")
	fmt.Fprintf(&out, "tolerance:      per row, the larger of %.0fx the row's own ratio-sd%% and %.0f%%\n\n",
		perfBenchNoiseMultiple, perfBenchMinimumTolerance*100)

	fmt.Fprintf(&out, "%-9s %-24s %8s %9s %9s %8s %8s %8s %7s %8s %13s %13s\n",
		"program", "case", "ratio", "ratio+/-%", "ratio-sd%", "null", "null+/-%", "null-sd%",
		"tol%", "detect%", "goc ns (fyi)", "host ns (fyi)")
	for _, row := range rows {
		fmt.Fprintf(&out, "%-9s %-24s %8.4f %9.2f %9.2f %8.4f %8.2f %8.2f %7.1f %8.1f %13.0f %13.0f\n",
			row.key.program, row.key.name,
			row.ratio, row.ratioHalfWidth/row.ratio*100, row.ratioSpread*100,
			row.null, row.nullHalfWidth/row.null*100, row.nullSpread*100,
			row.tolerance*100, row.detectable()*100,
			row.gocNanos, row.gcNanos)
	}
	return out.String()
}

func parsePerfBenchBaseline(text string) []perfBenchRow {
	var rows []perfBenchRow
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 12 || fields[0] == "program" {
			continue
		}
		numbers := make([]float64, 0, 10)
		malformed := false
		for _, field := range fields[2:] {
			value, err := strconv.ParseFloat(field, 64)
			if err != nil {
				malformed = true
				break
			}
			numbers = append(numbers, value)
		}
		if malformed {
			continue
		}
		rows = append(rows, perfBenchRow{
			key:            perfBenchKey{program: fields[0], name: fields[1]},
			ratio:          numbers[0],
			ratioHalfWidth: numbers[0] * numbers[1] / 100,
			ratioSpread:    numbers[2] / 100,
			null:           numbers[3],
			nullHalfWidth:  numbers[3] * numbers[4] / 100,
			nullSpread:     numbers[5] / 100,
			tolerance:      numbers[6] / 100,
			gocNanos:       numbers[8],
			gcNanos:        numbers[9],
		})
	}
	return rows
}

// perfBenchVerdict says whether the difference between this run and the baseline
// has been resolved to be larger than the row's tolerance.
//
// The rule: fail only when the interval on the *difference* between the two means
// clears the tolerance band. A single number crossing a fixed line is what the
// crypto benchmark used to do, and on this box a single number wanders by about
// 2% for reasons that have nothing to do with the compiler, so it fired at
// random. Asking instead whether the distribution has demonstrably moved by more
// than the tolerance is a question with a stable answer.
//
// Both intervals go into it. The baseline is a measurement too -- it is the mean
// of its own repetitions and the file records its half-width -- so the difference
// of two independent means carries both, combined in quadrature. Treating the
// committed number as exact would make the check about a third more likely to
// fire on a run compared against an unlucky baseline.
//
// The tolerance is the *baseline's*, not this run's. This run computes a
// tolerance too, and using it would mean a noisy run widens its own bar and
// passes -- which is the failure mode the noise-growth check exists to catch, so
// it must not be available here as well.
func perfBenchVerdict(baseline, baselineHalfWidth, found, foundHalfWidth, tolerance float64) (moved bool, change float64) {
	change = relativeChange(baseline, found)
	band := math.Abs(baseline) * tolerance
	resolved := math.Abs(found-baseline) - math.Hypot(foundHalfWidth, baselineHalfWidth)
	return resolved > band, change
}

// comparePerfBench sorts the difference between this run and the committed
// baseline into the questions a reviewer has to answer separately.
func comparePerfBench(t *testing.T, accepted, found []perfBenchRow, restricted bool) {
	t.Helper()

	acceptedByKey := make(map[perfBenchKey]perfBenchRow, len(accepted))
	for _, row := range accepted {
		acceptedByKey[row.key] = row
	}
	foundByKey := make(map[perfBenchKey]perfBenchRow, len(found))
	for _, row := range found {
		foundByKey[row.key] = row
	}

	// The green run's own report. A check whose passing output is the word "ok"
	// tells a reader nothing about what it looked at, and this one passes
	// movements of several percent by design -- so it says what it saw and how far
	// that was from firing.
	var summary strings.Builder
	fmt.Fprintf(&summary, "%-9s %-24s %9s %9s %8s %10s %7s %s\n",
		"program", "case", "baseline", "this run", "change", "resolved", "tol", "verdict")
	for _, row := range found {
		before, known := acceptedByKey[row.key]
		if !known {
			continue
		}
		moved, change := perfBenchVerdict(before.ratio, before.ratioHalfWidth, row.ratio, row.ratioHalfWidth, before.tolerance)
		resolved := (math.Abs(row.ratio-before.ratio) - math.Hypot(row.ratioHalfWidth, before.ratioHalfWidth)) /
			math.Abs(before.ratio)
		verdict := "within tolerance"
		if moved {
			verdict = "PAST TOLERANCE"
		}
		fmt.Fprintf(&summary, "%-9s %-24s %9.4f %9.4f %+7.1f%% %+9.1f%% %6.1f%% %s\n",
			row.key.program, row.key.name, before.ratio, row.ratio, change*100, resolved*100,
			before.tolerance*100, verdict)
	}
	t.Logf("goc/host ratio against the committed baseline. \"resolved\" is the part of the change that survives\n"+
		"both intervals -- a negative number means this run cannot tell the movement from zero, and it is what\n"+
		"is compared against the tolerance.\n%s", summary.String())

	var slower, faster, louder, appeared, vanished []string
	for _, row := range found {
		before, known := acceptedByKey[row.key]
		if !known {
			appeared = append(appeared, fmt.Sprintf("%s %s\n      now ratio %.4f +/- %.2f%%",
				row.key.program, row.key.name, row.ratio, row.ratioHalfWidth/row.ratio*100))
			continue
		}
		if moved, change := perfBenchVerdict(before.ratio, before.ratioHalfWidth, row.ratio, row.ratioHalfWidth, before.tolerance); moved {
			line := fmt.Sprintf("%s %s\n      ratio %.4f (+/- %.2f%%) -> %.4f (+/- %.2f%%), %+.1f%%, and the whole interval is past the %.1f%% tolerance",
				row.key.program, row.key.name,
				before.ratio, before.ratioHalfWidth/before.ratio*100,
				row.ratio, row.ratioHalfWidth/row.ratio*100, change*100, before.tolerance*100)
			if change > 0 {
				slower = append(slower, line)
			} else {
				faster = append(faster, line)
			}
		}
		if row.ratioSpread > perfBenchNoiseGrowthFloor &&
			before.ratioSpread > 0 &&
			row.ratioSpread > perfBenchNoiseGrowthCeiling*before.ratioSpread {
			louder = append(louder, fmt.Sprintf("%s %s\n      one-repetition spread %.2f%% in the baseline, %.2f%% in this run (null %.2f%% -> %.2f%%)",
				row.key.program, row.key.name, before.ratioSpread*100, row.ratioSpread*100,
				before.nullSpread*100, row.nullSpread*100))
		}
	}
	if !restricted {
		for _, row := range accepted {
			if _, ok := foundByKey[row.key]; !ok {
				vanished = append(vanished, fmt.Sprintf("%s %s\n      was ratio %.4f", row.key.program, row.key.name, row.ratio))
			}
		}
	}

	// The box before the compiler: a run this noisy cannot support the tolerances
	// it is about to be judged against, so say that instead of a verdict.
	require.Empty(t, louder,
		"this box is noisier than the box that produced the baseline, by more than %.0fx on the spread of the\n"+
			"gated ratio itself. Every tolerance below was derived on the quieter box, so a\n"+
			"pass here would be a pass this run did not earn and a failure would not be attributable. Rerun on an\n"+
			"idle machine, check that nothing else is pinned to the same core (GOC_PERF_CORE), and only if the\n"+
			"noise is genuinely permanent rerun with -update-perf-bench so the tolerances widen to match.\n  %s",
		perfBenchNoiseGrowthCeiling, strings.Join(louder, "\n  "))

	assert.Empty(t, slower, perfBenchSlowerMessage(slower))
	assert.Empty(t, faster,
		"goc is faster than the baseline says, by more than this instrument's own interval plus the row's\n"+
			"tolerance. That is the good direction and it is still a change someone has to look at: the cheap way\n"+
			"to get it is to stop heap-allocating something, and from here that is indistinguishable between an\n"+
			"escape analysis that got better and one that got permissive about an object which outlives its frame.\n"+
			"Diff testdata/alloc_census_baseline.txt for the sites that moved HEAP -> FRAME and say what proves\n"+
			"each one cannot outlive the frame. Then rerun with -update-perf-bench.\n\n"+
			"If the census did not move at all, look at the host column instead: this ratio has the host toolchain\n"+
			"in its denominator, so a host that got slower reads here as goc getting faster. The version is\n"+
			"recorded at the top of "+perfBenchBaseline+".\n  %s",
		strings.Join(faster, "\n  "))
	assert.Empty(t, appeared,
		"a program measures a case %s does not list. Expected when a case is added, in which case rerun with\n"+
			"-update-perf-bench.\n  %s", perfBenchBaseline, strings.Join(appeared, "\n  "))
	assert.Empty(t, vanished,
		"%s lists a case no program measures any more. Expected when a case is removed; otherwise the corpus and\n"+
			"the baseline have drifted apart.\n  %s", perfBenchBaseline, strings.Join(vanished, "\n  "))
}

// perfBenchSlowerMessage is the triage note.
//
// It is a function rather than a constant because it is the most important thing
// this file produces. The crypto benchmark's equivalent is what made a 6.20%
// movement -- which turned out to be 80 bytes of code placement and not one
// changed ECDSA instruction -- solvable rather than argued about. The three
// causes are distinguishable and the commands are here so that nobody has to
// reconstruct them under time pressure.
func perfBenchSlowerMessage(slower []string) string {
	return "goc costs more against the host toolchain than the baseline says, by more than this instrument's own\n" +
		"interval plus the row's tolerance -- so this is a movement of the distribution and not one noisy run.\n\n" +
		"A movement has three causes and they are distinguishable. Work down the list before accepting or\n" +
		"reverting anything; only (1) and (2) are regressions. PROGRAM below is the first column of the failing\n" +
		"row and SOURCE is its path in perfBenchPrograms.\n\n" +
		"  1. An allocation moved. The two gcpress churn rows and any row that got slower together with them\n" +
		"     point here first:\n\n" +
		"         go test -run '^TestAllocationCensus$' ./goc -v\n" +
		"         git diff -- goc/testdata/alloc_census_baseline.txt\n\n" +
		"     A site that went FRAME -> HEAP is the same shape as the one regression this tree has a record of.\n\n" +
		"  2. The generated code changed. Build the program with the suspect compiler and with its parent and\n" +
		"     compare the *encoded instruction words*, which do not move when the text does:\n\n" +
		"         go build -o \"$TMPDIR/goc.suspect\" ./cmd/goc\n" +
		"         git stash && go build -o \"$TMPDIR/goc.parent\" ./cmd/goc && git stash pop\n" +
		"         for side in suspect parent; do\n" +
		"           \"$TMPDIR/goc.$side\" -O -o \"$TMPDIR/bench.$side\" goc/SOURCE\n" +
		"           objdump -d \"$TMPDIR/bench.$side\" |\n" +
		"             awk '/^[0-9a-f]+ </{f=$2; next} /^ +[0-9a-f]+:/{print f, $2}' > \"$TMPDIR/words.$side\"\n" +
		"         done\n" +
		"         diff \"$TMPDIR/words.parent\" \"$TMPDIR/words.suspect\" | head -40\n\n" +
		"     The encoding column, not objdump's rendering: the rendering prints absolute branch targets, which\n" +
		"     differ whenever the text shifts even if the code is identical.\n\n" +
		"  3. Nothing changed and the code moved. If (2) says the words are identical, this is code placement and\n" +
		"     nothing got worse. Confirm it two ways:\n\n" +
		"         nm \"$TMPDIR/bench.parent\"  | sort -k3 > \"$TMPDIR/syms.parent\"\n" +
		"         nm \"$TMPDIR/bench.suspect\" | sort -k3 > \"$TMPDIR/syms.suspect\"\n" +
		"         diff \"$TMPDIR/syms.parent\" \"$TMPDIR/syms.suspect\" | head\n\n" +
		"     identical words plus different addresses is the signature. Then measure how big the effect is on\n" +
		"     this exact program, using the sweep the tree already has -- GOC_TEXT_PAD=K puts K bytes of no-ops\n" +
		"     in front of the first function and changes not one instruction:\n\n" +
		"         for K in 0 4 8 12 16 20 24 28; do\n" +
		"           GOC_TEXT_PAD=$K \"$TMPDIR/goc.suspect\" -O -o \"$TMPDIR/pad.$K\" goc/SOURCE\n" +
		"           taskset -c \"$GOC_PERF_CORE\" \"$TMPDIR/pad.$K\"\n" +
		"         done\n\n" +
		"     If the row swings across K by as much as the movement you are triaging, the movement is placement.\n" +
		"     goc/testdata/placement_bench/analysis_shift_phase.txt has this already measured for the seven\n" +
		"     programs reused from that corpus, under the alignment policy that ships (the loop32 column).\n\n" +
		"  And before any of the three: rerun the one program at more repetitions, which costs a minute rather\n" +
		"  than eleven and is often the whole answer.\n\n" +
		"         go test -run '^TestPerformanceSuite$' ./goc -perf-bench -perf-bench-only=PROGRAM \\\n" +
		"             -perf-bench-reps=25 -v\n\n" +
		"Then rerun with -update-perf-bench.\n  " + strings.Join(slower, "\n  ")
}
