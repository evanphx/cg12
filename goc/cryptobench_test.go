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

// measureCryptoBench asks for the crypto signing benchmark to be run. It is
// opt-in for the same reasons TestEscapeDifferentialAgainstGC and
// TestSlogAllocationsAgainstGC are, plus one of its own: it is the only
// instrument in this tree that measures elapsed time, so its answer depends on
// what else the machine is doing. That is a thing a person runs deliberately on
// a quiet box, not something a parallel `go test ./...` should be deciding.
var measureCryptoBench = flag.Bool("crypto-bench", false,
	"build goc/testdata/crypto_signing_bench with goc and with the host Go toolchain and compare elapsed time on the signing path against the committed baseline")

// updateCryptoBench rewrites the checked-in baseline from this run.
var updateCryptoBench = flag.Bool("update-crypto-bench", false,
	"rewrite testdata/crypto_signing_bench_baseline.txt from this run")

// cryptoBenchRepsFlag overrides how many interleaved repetitions each compiler
// gets. Raising it narrows the interval the check draws around its answer -- as
// the square root, so it costs four runs to halve it -- and is what to reach for
// when a movement lands near the tolerance and the question is whether it is
// real. Lowering it below cryptoBenchMinimumReps is refused rather than
// silently producing an interval no one should believe.
var cryptoBenchRepsFlag = flag.Int("crypto-bench-reps", cryptoBenchReps,
	"how many interleaved repetitions of each compiler's binary to time")

const (
	cryptoBenchProgram  = "testdata/crypto_signing_bench/main.go"
	cryptoBenchBaseline = "testdata/crypto_signing_bench_baseline.txt"

	// cryptoBenchControl is the case every other case is divided by. See the
	// "Why an index and not nanoseconds" section below.
	cryptoBenchControl = "control/spin-fixed-work"
	// cryptoBenchHarness must cost approximately nothing; it measures the
	// harness rather than any case.
	cryptoBenchHarness = "control/empty-body"
)

// cryptoBenchReps is how many times each compiler's binary is timed, in one run
// of the check.
//
// It is not 1, which is what this check used to do, and the reason is measured.
// A single process's index moves by about 1% peak to peak on this box for
// reasons that have nothing to do with the compiler -- see the noise floor
// below -- and one number against a fixed bound turns that into a coin flip
// whenever the true value sits near the bound. Five gives a mean whose 95%
// interval is about a third of a single reading's spread, and it is the mean
// with its interval that the check compares, so what fires the check is a
// distribution that has moved rather than one run that wandered.
//
// Five and not more because the cost is linear: each repetition is one goc-built
// process doing real P-256 and RSA arithmetic, about 90 seconds of the roughly
// eight minutes the whole check takes. -crypto-bench-reps buys more when a
// movement lands near the tolerance and someone needs the interval narrowed.
const cryptoBenchReps = 5

// cryptoBenchMinimumReps is the fewest repetitions that produce an interval
// worth printing. At n = 2 Student's t is 12.7 and the interval is wider than
// any regression this check could plausibly see, which would mean a green run
// that proves nothing; refusing is better than pretending.
const cryptoBenchMinimumReps = 3

// cryptoBenchTolerance is how far a case's mean index may sit from the baseline
// before the run fails, as a fraction. It is one of the two numbers that decide
// what this check can see; cryptoBenchReps is the other.
//
// # Where 0.06 comes from
//
// It is a little over three times the measured noise floor of the instrument as
// this file now runs it, and it is above the largest movement this benchmark has
// been shown to produce without any code getting worse.
//
// The floor, measured on this box on 4 Aug 2026 (linux/arm64, 64 cores,
// Neoverse-N1, idle, load ~1.0), by the method the tree already had: two
// byte-identical goc -O builds of the benchmark treated as two arms, one run of
// every arm per repetition in a shuffled order, each run pinned to one core with
// taskset, ten repetitions. Every number below is therefore same-source against
// same-source, where the true difference is exactly zero:
//
//	case                  single-rep sd    20-rep range    95% interval of a mean-of-5
//	p256/sign-verify           0.48 %          1.83 %              +/- 0.60 %
//	p256/verify                0.53 %          1.99 %              +/- 0.66 %
//	p384/sign-verify           0.36 %          1.28 %              +/- 0.44 %
//	rsa2048/sign-verify        1.03 %          4.06 %              +/- 1.28 %
//
// The same binaries measured unpinned, which is what this check did before, put
// the same ranges at 3.03 %, 3.36 %, 2.62 % and 4.53 %. Pinning and interleaving
// are where most of the noise went; see CCWORK_REPORT.md for both tables.
//
// # Why it is not simply three times 0.60 %
//
// Because run-to-run noise is not the only thing that moves this number without
// anything getting worse. Code placement does too. goc aligns the entry of every
// function containing a loop to 32 bytes, which pins a function's phase inside
// the instruction fetch granule, and that took the placement-induced spread on
// this benchmark from 6.1 % to 0.4 % when it landed. An independent measurement
// on the wave-7 gate could not reproduce those figures and got 4.25 % -> 2.73 %.
// Both agree the effect shrank and neither says it is gone: alignment fixes a
// phase, not an address. So a commit that changes the size of some cold code
// elsewhere can still move this index by low single digits with the crypto
// path's instructions unchanged, and a tolerance under about 3 % would fire on
// that -- which is a movement the triage note calls "nothing got worse".
//
// 0.06 clears the larger placement estimate with room, and clears the noise floor
// by more than three times.
//
// # What this check can and cannot see
//
// The check fails only when the whole 95% interval of this run's mean lies
// outside the baseline's tolerance band, so the movement it fires on is the
// tolerance plus this run's interval. At the default five repetitions that is
// about 6.6 % on the p256 cases and about 7.3 % on rsa2048.
//
//   - It will catch the regression this file exists for. The escape publication
//     fix was reported at +5.8 % and the change in that neighbourhood this tree
//     did make measured -4.9 %; neither is above the new bar on its own, and that
//     is the honest cost of this change -- see the paragraph after next.
//   - It will not see anything under about 6 %. A 3 % real regression passes
//     green here and there is no version of this instrument on this box that
//     both sees 3 % and does not fail at random.
//   - It is not blind under 6 %, only silent. The baseline is committed, so a
//     2 % movement that passes still appears as a changed number the next time
//     someone runs -update-crypto-bench, and the interval is printed in the file
//     beside it so a reader can tell a real 2 % from a wandering one.
//
// About the 5.8 % and the 4.9 %: both were single-run figures from an instrument
// whose spread was never measured, and the 5.8 % has since been re-measured at
// +0.04 % [-1.37 %, +1.46 %] -- a null. What is left that this check would still
// catch is the -4.9 %, whose interval was [-6.1 %, -3.8 %]; at six repetitions it
// would fire and at five it would not. That is written down here rather than
// engineered around, because the alternative is a tolerance this box cannot
// support, and a check that fires at random is worse than no check.
const cryptoBenchTolerance = 0.06

// cryptoBenchPrecisionCeiling is the widest 95% interval, as a fraction of the
// mean, that a run may draw around a case before the whole run is thrown out.
//
// This is what keeps a green run meaningful. The comparison rule -- fail only
// when the interval clears the tolerance band -- has an obvious failure mode: on
// a loaded box the interval grows until nothing can ever fall outside it, and the
// check turns green precisely when it has stopped working. A ceiling turns that
// into a failure with a reason attached instead.
//
// 0.02 is about three times the interval a quiet box produces at five
// repetitions (+/- 0.60 % on p256/sign-verify above) and a third of the
// tolerance, so a run that passes it can still resolve the thing the tolerance
// is set to catch. rsa2048 is the noisiest case and sits at +/- 1.28 %, which is
// the reason the ceiling is not tighter.
const cryptoBenchPrecisionCeiling = 0.02

// cryptoBenchHarnessCeiling is the largest fraction of the control the empty
// body may cost before the whole run is thrown out. The harness does one
// closure call per round; at any real machine speed that is far below a
// thousandth of the control's twenty million multiply-adds.
const cryptoBenchHarnessCeiling = 0.001

const cryptoBenchHeader = `# Elapsed time on the crypto signing path under goc and under the host Go
# toolchain. Regenerate with
#
#     make bench-crypto-update
#
# and read the diff; check it with
#
#     make bench-crypto
#
# The run needs a Go toolchain and a system linker, and takes about eight
# minutes: it times each compiler's binary five times, interleaved, because one
# reading of an elapsed time is not a measurement. Run it on an idle machine:
# unlike every other baseline in this directory it measures time, so a loaded box
# produces a number about the box.
#
# It will not start on a box that is not idle. A pre-flight measures the core the
# run is about to pin to -- a calibration burst against /proc/stat, about a second
# and a half -- and refuses there rather than eight minutes later at the precision
# ceiling. GOC_BENCH_PREFLIGHT=off skips it, for when a number from a busy box is
# wanted deliberately; goc/benchpreflight_test.go says what it measures.
#
# Both columns come from the same program -- goc/testdata/crypto_signing_bench --
# compiled by both compilers, so the two answers are produced by the same
# measurement rather than by two benchmarks that happen to be named alike. That
# program's doc comment is where the method is written down.
#
# # Why an index and not nanoseconds
#
# Nanoseconds are a property of the machine as much as of the compiler, and a
# baseline of raw times could only ever be checked on the one box it was taken
# on. Every case is therefore divided by control/spin-fixed-work, a fixed amount
# of integer arithmetic measured in the same binary in the same process, and it
# is that ratio -- the index column -- that is committed and compared. A machine
# twice as fast changes both numbers and does not change the index.
#
# The raw nanoseconds are printed alongside so the index can be sanity-checked,
# but they are not compared; they are expected to differ between machines.
#
# # What the +/-% columns are, and why the check has them
#
# Each index is the mean of five interleaved, core-pinned repetitions, and the
# +/-% beside it is the 95% confidence interval of that mean, as a fraction of
# it. It is a property of the box the file was produced on, and it is committed
# because it is the only thing that says how much of a movement in the column
# beside it is believable.
#
# A run fails only when the whole interval of its own mean falls outside this
# file's value widened by the tolerance. So the smallest movement that fails is
# the tolerance plus the run's interval -- about 6.6% on the p256 cases -- and a
# movement smaller than that is reported by this file changing, not by the check
# going red. That is deliberate: this box's same-source run-to-run spread is
# around 2% pinned and 3% unpinned, and a check that fires below its own noise
# is a check people learn to ignore.
#
# # A movement is not automatically a regression
#
# This used to be the main thing to know about this file. goc gave function
# entries no alignment, so where the crypto code landed inside the processor's
# 32-byte instruction fetch granule was decided by the total size of everything
# emitted before it, and this index was a square wave in that shift with an
# amplitude of 6.1%. goc now places a function that contains a loop on a 32-byte
# boundary, which pins the phase, and the same experiment moved this index by
# 0.4% when that landed -- though an independent measurement on the wave-7 gate
# got 2.73% for the same quantity, so call the residue low single digits rather
# than a number.
#
# It is not zero, which is why the tolerance is set above it. Alignment fixes a
# function's phase, not its address: two builds still put the code in different
# cache sets, and a function with no loop in it is still placed wherever the
# previous one ended. So before treating a movement here as a regression, still
# check whether the path's instruction *encodings* changed at all --
# goc/cryptobench_test.go's failure message gives the commands. A movement with
# identical encodings is a placement effect and nothing got worse.
#
`

// TestCryptoSigningBench holds the elapsed cost of the crypto signing path to a
// committed baseline.
//
// # Why the signing path
//
// crypto/internal/fips140/bigmod is where allocation placement and elapsed time
// meet. Nat.Mul's default arm builds &Nat{limbs: T} out of a locally made
// slice, so its cost is decided by two escape questions at once, and P-256's
// four-limb arithmetic is what takes that arm. It is also the shape that caused
// the only measured performance regression this tree has a record of.
//
// # How it measures, and why that shape
//
// One reading of an elapsed time is not a measurement, and this check used to
// take one. It failed 3 of 7 runs on a tree that emitted byte-identical code,
// on different cases in opposite directions, while a byte-identical control tree
// passed 7 of 7 -- a coin flip dressed as a gate. See CCWORK_REPORT.md, "make
// bench-crypto as a check that only fires on real regressions".
//
// So the protocol is the one the tree's own placement investigation used, which
// is the one that got a usable interval out of this box:
//
//   - Repetitions, not one run. Each compiler's binary is timed cryptoBenchReps
//     times and what the check compares is the mean.
//   - Interleaved. One run of every binary per repetition, with the order of the
//     two compilers rotated between repetitions, so that drift over the eight
//     minutes lands on both arms equally instead of on whichever ran last. An
//     earlier session measured the size of that ordering artefact directly: a
//     non-interleaved set put one case at +1.28% and interleaving reversed it to
//     -0.79%.
//   - Pinned. Every run is `taskset -c N` on one core. On this box that is worth
//     more than everything else here put together: the same pair of
//     byte-identical binaries spread 3.03% unpinned and 1.83% pinned.
//   - Warm-up discarded, which the program has always done: one untimed round
//     before the timed ones, then the fastest of five timed rounds within the
//     process.
//   - An interval, not a point. The mean's 95% interval is computed, committed
//     to the baseline, and used by the comparison: a case fails only when its
//     whole interval lies outside the baseline widened by the tolerance. What
//     fires this check is a distribution that moved.
//
// # Why it fails in both directions
//
// Slower is the obvious news and is why the file exists. Faster is news too, and
// for the same reason the allocation census fails on an object that moves to the
// frame: the cheap way to make this path faster is to stop heap-allocating
// something, and "stopped heap-allocating something" is indistinguishable, from
// here, between a real escape-analysis improvement and the analysis becoming
// permissive about an object that genuinely outlives its frame. The census says
// which sites moved; this file says what it cost. A drop here with no
// corresponding line in the census diff is the combination that should worry
// someone.
//
// # What it is not
//
// It is not a benchmark suite and it does not try to be a general performance
// gate. It watches one path, chosen because that path is where the tree's one
// known regression was, and it makes a number that used to exist only in a
// report into a number that a command reproduces.
//
// It is also not sensitive. It sees a regression of about 6.6% on the p256 cases
// and nothing below that; cryptoBenchTolerance is where that number is derived
// and where the cost of it is written down.
//
// # The third cause, which is not in the failure message's first two
//
// A movement here has three possible causes and not two. The first two are the
// ones this file was built for: an allocation moved, or the code generated for
// the path changed. The third is that the code did not change at all and *moved*.
//
// It used to be a large effect. Measured on this box, holding the compiled
// program byte-for-byte identical and shifting the whole text section by K
// bytes, the p256/sign-verify index was a square wave in K with a period of 32
// bytes -- the Neoverse-N1 instruction fetch granule -- and an amplitude of 6.1%:
//
//	K mod 32 = 0    index 47.72 - 47.86
//	K mod 32 = 16   index 44.79 - 44.83
//
// So a change anywhere in the program that altered the number of bytes emitted
// before the crypto code -- a cold branch in an unrelated package, which is what
// 96996b0 was -- could move this row by more than the tolerance while the ECDSA
// path's instructions were unchanged.
//
// goc now aligns the entry of every function that contains a loop to 32 bytes
// (arm64/align.go), which is >= the granule, so a function's phase no longer
// depends on anything emitted before it. The same K sweep moved this index by
// 0.4% when that landed, and across the eight-program corpus at
// goc/testdata/placement_bench the median case moved 1.0% where it used to move
// 14.6%. A later, independent measurement on the wave-7 gate could not reproduce
// those figures and put the residue at 2.73%, down from 4.25%. The two agree on
// the direction and not on the size, so treat the residue as low single digits.
//
// The residue is not zero and is not expected to be. Alignment pins a phase, not
// an address: two builds put the code in different cache sets, and a function
// with no loop in it is still laid down wherever the previous one ended.
//
// Which is to say: this row failing is not by itself evidence that anything got
// worse -- the tolerance is set above the placement residue precisely so that it
// usually is evidence, but "usually" is not "always". compareCryptoBench's
// message says how to tell the three apart.
func TestCryptoSigningBench(t *testing.T) {
	if !*measureCryptoBench && !*updateCryptoBench {
		t.Skip("pass -crypto-bench to build goc/testdata/crypto_signing_bench with both compilers and compare elapsed time")
	}
	reps := *cryptoBenchRepsFlag
	require.GreaterOrEqual(t, reps, cryptoBenchMinimumReps,
		"-crypto-bench-reps=%d would produce an interval too wide to mean anything; %d is the fewest this check accepts",
		reps, cryptoBenchMinimumReps)

	// The pre-flight, before a byte is compiled: the precision ceiling below is
	// what says this box was too busy to conclude anything from, and it says it
	// after the whole run. Asking the same question of the machine first costs
	// about a second and a half. The ceiling is unchanged and still has the last
	// word, since it sees the run that was actually measured.
	pin, pinNote := cryptoBenchPin()
	requireQuietBoxBefore(t, benchPreflightSuite{
		target:  "make bench-crypto",
		cost:    "about eight minutes",
		pin:     pin,
		pinNote: pinNote,
		coreVar: "GOC_BENCH_CORE",
	})

	goVersion := hostGoVersion(t)
	t.Logf("host toolchain: %s", goVersion)

	scratch := t.TempDir()
	gcBinary := buildCryptoBenchWithHostToolchain(t, scratch)
	gocBinary := buildCryptoBenchWithGoc(t, scratch)

	t.Logf("timing protocol: %d interleaved repetitions, %s", reps, pinNote)

	gocRuns, gcRuns, method := runCryptoBenchInterleaved(t, gocBinary, gcBinary, pin, reps)
	rows := cryptoBenchRows(t, gocRuns, gcRuns)

	protocol := fmt.Sprintf("%d interleaved repetitions, %s, mean with a 95%% interval", reps, pinNote)
	rendered := renderCryptoBench(goVersion, method, protocol, rows)
	t.Logf("this run:\n%s", rendered)

	// Before the baseline is written and before it is compared: a run that could
	// not resolve its own answer is not one to accept a baseline from either.
	checkCryptoBenchInstrument(t, gocRuns, gcRuns, rows)

	if *updateCryptoBench {
		require.NoError(t, os.WriteFile(cryptoBenchBaseline, []byte(rendered), 0o644))
		t.Skip("baseline rewritten; rerun without -update-crypto-bench to check it")
	}

	accepted, err := os.ReadFile(cryptoBenchBaseline)
	require.NoError(t, err)
	compareCryptoBench(t, parseCryptoBenchBaseline(string(accepted)), rows)
}

// checkCryptoBenchInstrument checks the instrument before anything measured with
// it is believed. Two questions, both of which have a wrong answer that would
// otherwise show up as a green run:
//
//   - is the harness a measurable part of a row? If the empty case is not far
//     below the control then every number in the table is partly the harness.
//   - did this run resolve anything? The comparison passes a case whose interval
//     overlaps the tolerance band, so a run noisy enough to draw a very wide
//     interval passes everything. That is exactly when the answer should be "the
//     box was too busy", and it is what the precision ceiling says.
func checkCryptoBenchInstrument(t *testing.T, gocRuns, gcRuns []map[string]float64, rows []cryptoBenchRow) {
	t.Helper()

	for _, side := range []struct {
		compiler string
		runs     []map[string]float64
	}{{"goc", gocRuns}, {"gc", gcRuns}} {
		shares := make([]float64, 0, len(side.runs))
		for _, times := range side.runs {
			shares = append(shares, times[cryptoBenchHarness]/times[cryptoBenchControl])
		}
		harnessShare, _ := meanAndHalfWidth(shares)
		require.Less(t, harnessShare, cryptoBenchHarnessCeiling,
			"%s is a calibration case: under %s it cost %.4f of %s on the mean of %d runs, which means\n"+
				"the harness is a measurable part of every row and no other number in this run means anything.",
			cryptoBenchHarness, side.compiler, harnessShare, cryptoBenchControl, len(side.runs))
	}

	var imprecise []string
	for _, row := range rows {
		if share := row.gocHalfWidth / row.gocIndex; share > cryptoBenchPrecisionCeiling {
			imprecise = append(imprecise, fmt.Sprintf("%s\n      goc index %.4f +/- %.2f%% (ceiling %.2f%%)",
				row.name, row.gocIndex, share*100, cryptoBenchPrecisionCeiling*100))
		}
	}
	require.Empty(t, imprecise,
		"this run did not resolve its own answer well enough to be worth comparing. The interval on the\n"+
			"mean is wider than %.2f%%, which is wide enough that a real regression of the size this check\n"+
			"is set to catch could hide inside it -- so a pass would have meant nothing.\n\n"+
			"This is a statement about the machine, not about the compiler. Something else was running,\n"+
			"or the runs were not pinned to a core (see the protocol line logged above). Rerun on an idle\n"+
			"box, or raise -crypto-bench-reps, which narrows the interval as the square root of the count.\n  %s",
		cryptoBenchPrecisionCeiling*100, strings.Join(imprecise, "\n  "))
}

// cryptoBenchRow is one case's pair of indexes, each the mean of the run's
// repetitions, with the 95% half-width of that mean beside it.
type cryptoBenchRow struct {
	name                   string
	gocIndex, gocHalfWidth float64
	gcIndex, gcHalfWidth   float64
	gocNanos, gcNanos      float64
}

func buildCryptoBenchWithHostToolchain(t *testing.T, scratch string) string {
	t.Helper()

	binary := filepath.Join(scratch, "cryptobench.gc")
	build := exec.Command("go", "build", "-o", binary, cryptoBenchProgram)
	build.Env = append(os.Environ(), "GOFLAGS=")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "the host toolchain would not build %s:\n%s", cryptoBenchProgram, output)
	return binary
}

// buildCryptoBenchWithGoc compiles the benchmark with goc -O.
//
// -O and not the default arm: this measures generated code, and the optimized
// arm is the one whose speed is worth watching. It is also the arm the recovered
// measurement used, so the two numbers are the same quantity.
func buildCryptoBenchWithGoc(t *testing.T, scratch string) string {
	t.Helper()

	driver := filepath.Join(scratch, "goc")
	build := exec.Command("go", "build", "-o", driver, "github.com/evanphx/cg12/cmd/goc")
	build.Env = append(os.Environ(), "GOFLAGS=")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "could not build the goc driver:\n%s", output)

	binary := filepath.Join(scratch, "cryptobench.goc")
	compile := exec.Command(driver, "-O", "-o", binary, cryptoBenchProgram)
	output, err = compile.CombinedOutput()
	require.NoError(t, err, "goc would not build %s:\n%s", cryptoBenchProgram, output)
	return binary
}

// cryptoBenchPin returns the command prefix that runs a binary on a single core,
// and the note about it that goes in the log and in the baseline.
//
// Pinning is the single largest thing that can be done to this instrument for
// free. Measured on this box with two byte-identical binaries, the same-source
// range over twenty runs was 1.83% pinned and 3.03% unpinned on
// p256/sign-verify; the whole point of a tolerance derived from a noise floor is
// that the floor be as low as it cheaply can be first.
//
// Which core: the highest one this process is allowed on, because CPU 0 is where
// most kernels land interrupts. GOC_BENCH_CORE overrides it -- set it when two
// of these are running on one box, since two runs that pick the same core would
// measure each other. GOC_BENCH_CORE="" turns pinning off, which is honest but
// costs about a factor of two in spread.
//
// Not pinning is not fatal. The check still runs, the note says so, and the
// precision ceiling is what catches the case where the resulting spread was too
// wide to conclude anything from.
func cryptoBenchPin() (prefix []string, note string) {
	requested, overridden := os.LookupEnv("GOC_BENCH_CORE")
	if overridden && requested == "" {
		return nil, "not pinned (GOC_BENCH_CORE is empty)"
	}
	taskset, err := exec.LookPath("taskset")
	if err != nil {
		return nil, "not pinned (taskset is not on PATH), so expect about twice the spread"
	}
	if overridden {
		return []string{taskset, "-c", requested}, "pinned to core " + requested + " (GOC_BENCH_CORE)"
	}
	core, err := highestAllowedCPU()
	if err != nil {
		return nil, "not pinned (" + err.Error() + "), so expect about twice the spread"
	}
	return []string{taskset, "-c", strconv.Itoa(core)}, fmt.Sprintf("pinned to core %d", core)
}

// highestAllowedCPU reads this process's CPU affinity and returns the largest
// core in it. Reading the affinity rather than assuming runtime.NumCPU()-1: under
// a cpuset cgroup -- which is how this repository's jobs are run on a shared box
// -- the process may own a slice of the machine and not the top of it.
func highestAllowedCPU() (int, error) {
	cores, err := allowedCPUs()
	if err != nil {
		return 0, err
	}
	return cores[len(cores)-1], nil
}

// runCryptoBenchInterleaved times both compilers' binaries reps times each, one
// run of each per repetition.
//
// The order of the two compilers is rotated between repetitions. It matters less
// than it would if the index were raw time -- each binary is divided by a control
// measured inside its own process, which is what removes the machine's state from
// the number -- but the two arms are eight minutes of work and the box is not
// guaranteed to be the same at the end of that as at the start, so the cheap
// symmetry is worth taking.
//
// Every run is a fresh process. That is the unit of variation this check is
// trying to average over: the same binary run twice differs in where its heap
// lands, what the allocator's size classes look like when the timed round starts,
// and which way the branch predictor was leaning, and none of those is a property
// of the compiler.
func runCryptoBenchInterleaved(t *testing.T, gocBinary, gcBinary string, pin []string, reps int) (gocRuns, gcRuns []map[string]float64, method string) {
	t.Helper()

	for rep := 0; rep < reps; rep++ {
		first, second := gocBinary, gcBinary
		firstRuns, secondRuns := &gocRuns, &gcRuns
		if rep%2 == 1 {
			first, second = second, first
			firstRuns, secondRuns = secondRuns, firstRuns
		}
		var times map[string]float64
		times, method = runCryptoBench(t, first, pin, method)
		*firstRuns = append(*firstRuns, times)
		times, method = runCryptoBench(t, second, pin, method)
		*secondRuns = append(*secondRuns, times)
	}
	return gocRuns, gcRuns, method
}

// runCryptoBench runs every case in one process and returns each one's fastest
// round in nanoseconds.
//
// One process for all cases, where the slog comparison uses one process per
// case: there, a case could kill the program and take the rest of the table with
// it, and the cases are cheap. Here every case shares expensive setup (three key
// generations), no case has ever crashed, and the index is only meaningful when
// the case and its control were measured under the same conditions -- which one
// process guarantees and separate processes do not.
//
// method is the "# rounds=..." line the program prints, threaded through so that
// both compilers' answers can be checked to have been produced with the same
// parameters.
func runCryptoBench(t *testing.T, binary string, pin []string, method string) (map[string]float64, string) {
	t.Helper()

	argv := append(append([]string{}, pin...), binary)
	output, err := exec.Command(argv[0], argv[1:]...).Output()
	require.NoError(t, err, "%s did not run", strings.Join(argv, " "))

	times := map[string]float64{}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "#") {
			reported := strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if method == "" {
				method = reported
			}
			require.Equal(t, method, reported,
				"%s measured with different parameters than an earlier run used", binary)
			continue
		}
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 2 {
			continue
		}
		nanos, err := strconv.ParseFloat(fields[1], 64)
		require.NoError(t, err)
		times[fields[0]] = nanos
	}
	require.Contains(t, times, cryptoBenchControl, "%s printed no control row", binary)
	require.NotZero(t, times[cryptoBenchControl])
	return times, method
}

// cryptoBenchRows turns the two sets of timings into the indexed table, sorted
// the way the program lists its cases.
//
// The index is formed per repetition and then averaged, not formed once out of
// two averages. A repetition's case time and its control time come from the same
// process; dividing them there is what removes the machine, and averaging the
// ratios keeps that pairing. Averaging first and dividing after would divide one
// run's case by another run's control.
func cryptoBenchRows(t *testing.T, goc, gc []map[string]float64) []cryptoBenchRow {
	t.Helper()

	names := make([]string, 0, len(goc[0]))
	for name := range goc[0] {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]cryptoBenchRow, 0, len(names))
	for _, name := range names {
		if name == cryptoBenchControl || name == cryptoBenchHarness {
			continue
		}
		require.Contains(t, gc[0], name, "the gc-built binary did not report %s", name)
		row := cryptoBenchRow{name: name}
		row.gocIndex, row.gocHalfWidth = meanAndHalfWidth(cryptoBenchIndexes(goc, name))
		row.gcIndex, row.gcHalfWidth = meanAndHalfWidth(cryptoBenchIndexes(gc, name))
		row.gocNanos, _ = meanAndHalfWidth(cryptoBenchNanos(goc, name))
		row.gcNanos, _ = meanAndHalfWidth(cryptoBenchNanos(gc, name))
		rows = append(rows, row)
	}
	return rows
}

func cryptoBenchIndexes(runs []map[string]float64, name string) []float64 {
	indexes := make([]float64, 0, len(runs))
	for _, times := range runs {
		indexes = append(indexes, times[name]/times[cryptoBenchControl])
	}
	return indexes
}

func cryptoBenchNanos(runs []map[string]float64, name string) []float64 {
	nanos := make([]float64, 0, len(runs))
	for _, times := range runs {
		nanos = append(nanos, times[name])
	}
	return nanos
}

// studentT95 is the two-sided 95% critical value of Student's t at n-1 degrees
// of freedom, keyed by n. A table and not an approximation: the range of n this
// instrument can be run at is small, three decimal places is more than the
// measurement deserves, and any row of it can be checked against a book.
var studentT95 = map[int]float64{
	2: 12.706, 3: 4.303, 4: 3.182, 5: 2.776, 6: 2.571, 7: 2.447, 8: 2.365,
	9: 2.306, 10: 2.262, 11: 2.228, 12: 2.201, 13: 2.179, 14: 2.160,
	15: 2.145, 16: 2.131, 17: 2.120, 18: 2.110, 19: 2.101, 20: 2.093,
}

// studentT95Beyond is used for a sample larger than the table. It is the t at
// n = 20 rather than the normal limit of 1.96, so a large -crypto-bench-reps
// draws a slightly wider interval than it has earned rather than a narrower one.
const studentT95Beyond = 2.093

// meanAndHalfWidth returns the sample mean and the half-width of its two-sided
// 95% confidence interval, in the units of the values.
//
// Student's t and not 1.96 sigma: this instrument runs at five observations by
// default, where the normal approximation understates the interval by about 40%,
// and an interval that is too narrow turns straight into a check that fires on
// noise -- which is the defect this file was rewritten to fix.
func meanAndHalfWidth(values []float64) (mean, halfWidth float64) {
	if len(values) == 0 {
		return 0, 0
	}
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	if len(values) < 2 {
		return mean, 0
	}
	sumSquares := 0.0
	for _, value := range values {
		sumSquares += (value - mean) * (value - mean)
	}
	deviation := math.Sqrt(sumSquares / float64(len(values)-1))
	t, known := studentT95[len(values)]
	if !known {
		t = studentT95Beyond
	}
	return mean, t * deviation / math.Sqrt(float64(len(values)))
}

func renderCryptoBench(goVersion, method, protocol string, rows []cryptoBenchRow) string {
	var out strings.Builder
	out.WriteString(cryptoBenchHeader)
	fmt.Fprintf(&out, "host toolchain: %s\n", goVersion)
	fmt.Fprintf(&out, "measurement:    %s\n", method)
	fmt.Fprintf(&out, "protocol:       %s\n", protocol)
	fmt.Fprintf(&out, "index:          case time / %s time, same binary, same process\n", cryptoBenchControl)
	fmt.Fprintf(&out, "tolerance:      %.2f of the index, both directions, and the run's own interval must clear it\n\n", cryptoBenchTolerance)

	fmt.Fprintf(&out, "%-24s %12s %9s %12s %9s %14s %14s\n",
		"case", "goc index", "goc +/-%", "gc index", "gc +/-%", "goc ns (fyi)", "gc ns (fyi)")
	for _, row := range rows {
		fmt.Fprintf(&out, "%-24s %12.4f %9.2f %12.4f %9.2f %14.0f %14.0f\n",
			row.name,
			row.gocIndex, row.gocHalfWidth/row.gocIndex*100,
			row.gcIndex, row.gcHalfWidth/row.gcIndex*100,
			row.gocNanos, row.gcNanos)
	}
	return out.String()
}

func parseCryptoBenchBaseline(text string) []cryptoBenchRow {
	var rows []cryptoBenchRow
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 7 || fields[0] == "case" {
			continue
		}
		gocIndex, gocError := strconv.ParseFloat(fields[1], 64)
		gocSpread, gocSpreadError := strconv.ParseFloat(fields[2], 64)
		gcIndex, gcError := strconv.ParseFloat(fields[3], 64)
		gcSpread, gcSpreadError := strconv.ParseFloat(fields[4], 64)
		if gocError != nil || gcError != nil || gocSpreadError != nil || gcSpreadError != nil {
			continue
		}
		rows = append(rows, cryptoBenchRow{
			name:         fields[0],
			gocIndex:     gocIndex,
			gocHalfWidth: gocIndex * gocSpread / 100,
			gcIndex:      gcIndex,
			gcHalfWidth:  gcIndex * gcSpread / 100,
		})
	}
	return rows
}

// cryptoBenchVerdict says whether the difference between this run and the
// baseline has been resolved to be larger than the tolerance.
//
// The rule, and the reason for it: the check fails only when the interval on the
// *difference* between the two means clears the tolerance band. A single number
// crossing a fixed line is what this check used to do, and on this box a single
// number wanders by about 2% for reasons that have nothing to do with the
// compiler, so it fired at random. Asking instead whether the distribution has
// demonstrably moved by more than the tolerance is a question with a stable
// answer.
//
// Both intervals go into it. The baseline is a measurement too -- it is the mean
// of its own five repetitions and the file records its half-width -- so the
// difference of two independent means carries both, combined in quadrature.
// Treating the committed number as exact would make the check about a third
// more likely to fire on a run that happened to be compared against an unlucky
// baseline.
//
// It also means the movement that fires the check is the tolerance plus that
// combined interval, not the tolerance. That is a real loss of sensitivity and
// it is stated in cryptoBenchTolerance rather than hidden here.
func cryptoBenchVerdict(baseline, baselineHalfWidth, found, foundHalfWidth float64) (moved bool, change float64) {
	change = relativeChange(baseline, found)
	band := math.Abs(baseline) * cryptoBenchTolerance
	resolved := math.Abs(found-baseline) - math.Hypot(foundHalfWidth, baselineHalfWidth)
	return resolved > band, change
}

// compareCryptoBench sorts the difference between this run and the committed
// baseline into the questions a reviewer has to answer separately. Slower and
// faster are not the same news and a single "the file changed" assertion would
// get read as noise.
func compareCryptoBench(t *testing.T, accepted, found []cryptoBenchRow) {
	t.Helper()

	acceptedByName := make(map[string]cryptoBenchRow, len(accepted))
	for _, row := range accepted {
		acceptedByName[row.name] = row
	}
	foundByName := make(map[string]cryptoBenchRow, len(found))
	for _, row := range found {
		foundByName[row.name] = row
	}

	// The green run's own report. A check whose passing output is the word "ok"
	// tells a reader nothing about what it looked at, and this one passes
	// movements of several percent by design -- so it says what it saw and how
	// far that was from firing.
	var summary strings.Builder
	fmt.Fprintf(&summary, "%-24s %10s %10s %8s %10s %s\n",
		"case", "baseline", "this run", "change", "resolved", "verdict")
	for _, row := range found {
		before, known := acceptedByName[row.name]
		if !known {
			continue
		}
		moved, change := cryptoBenchVerdict(before.gocIndex, before.gocHalfWidth, row.gocIndex, row.gocHalfWidth)
		resolved := (math.Abs(row.gocIndex-before.gocIndex) - math.Hypot(row.gocHalfWidth, before.gocHalfWidth)) /
			math.Abs(before.gocIndex)
		verdict := fmt.Sprintf("within tolerance (%.0f%%)", cryptoBenchTolerance*100)
		if moved {
			verdict = "PAST TOLERANCE"
		}
		fmt.Fprintf(&summary, "%-24s %10.4f %10.4f %+7.1f%% %+9.1f%% %s\n",
			row.name, before.gocIndex, row.gocIndex, change*100, resolved*100, verdict)
	}
	t.Logf("goc index against the committed baseline. \"resolved\" is the part of the change that\n"+
		"survives both intervals -- a negative number means this run cannot tell the movement from\n"+
		"zero, and it is what is compared against the tolerance.\n%s", summary.String())

	var slower, faster, reference, appeared, vanished []string
	for _, row := range found {
		before, known := acceptedByName[row.name]
		if !known {
			appeared = append(appeared, fmt.Sprintf("%s\n      now index %.4f +/- %.2f%% under goc",
				row.name, row.gocIndex, row.gocHalfWidth/row.gocIndex*100))
			continue
		}
		if moved, change := cryptoBenchVerdict(before.gocIndex, before.gocHalfWidth, row.gocIndex, row.gocHalfWidth); moved {
			line := fmt.Sprintf("%s\n      goc index %.4f (+/- %.2f%%) -> %.4f (+/- %.2f%%), %+.1f%%, and the whole interval is past the %.0f%% tolerance",
				row.name, before.gocIndex, before.gocHalfWidth/before.gocIndex*100,
				row.gocIndex, row.gocHalfWidth/row.gocIndex*100, change*100, cryptoBenchTolerance*100)
			if change > 0 {
				slower = append(slower, line)
			} else {
				faster = append(faster, line)
			}
		}
		if moved, change := cryptoBenchVerdict(before.gcIndex, before.gcHalfWidth, row.gcIndex, row.gcHalfWidth); moved {
			reference = append(reference, fmt.Sprintf("%s\n      gc index %.4f -> %.4f (%+.1f%%)",
				row.name, before.gcIndex, row.gcIndex, change*100))
		}
	}
	for _, row := range accepted {
		if _, ok := foundByName[row.name]; !ok {
			vanished = append(vanished, fmt.Sprintf("%s\n      was index %.4f under goc", row.name, row.gocIndex))
		}
	}

	assert.Empty(t, slower,
		"the crypto signing path costs more than the baseline says, by more than this instrument's own\n"+
			"interval plus its tolerance -- so this is a movement of the distribution and not one noisy\n"+
			"run. This is paid by every ECDSA operation in every program goc compiles. There are three\n"+
			"causes and they are distinguishable; work down the list before accepting or reverting\n"+
			"anything.\n\n"+
			"  1. An allocation moved. Diff testdata/alloc_census_baseline.txt: a bigmod site that\n"+
			"     went FRAME -> HEAP is the same shape as the regression this file was created for.\n\n"+
			"  2. The generated code changed. Build the benchmark with the suspect compiler and\n"+
			"     with its parent, and compare the *encoded instruction words* of the functions on\n"+
			"     the path -- bigmod.addMulVVW, Nat.montgomeryMul, Nat.Mul:\n\n"+
			"         nm -S bench.a | grep bigmod        # addresses and sizes\n"+
			"         objdump -d --start-address=A --stop-address=B bench.a | awk '{print $2}'\n\n"+
			"     Compare the encoding column, not objdump's rendering: it prints absolute branch\n"+
			"     targets, which differ whenever the text shifts even if the code is identical.\n\n"+
			"  3. Nothing changed and the code moved. If (2) says the bytes are identical and the\n"+
			"     symbol addresses differ, this is a code placement effect and the honest answer\n"+
			"     is that nothing got worse. This is what happened at 96996b0, when it was worth\n"+
			"     6.1%% because functions had no alignment at all. goc now places a function with a\n"+
			"     loop in it on a 32-byte boundary, and the residue after that has been measured at\n"+
			"     0.4%% once and 2.73%% once -- low single digits, in other words, which is below this\n"+
			"     check's %.0f%% tolerance but not by a margin that lets you skip the check. Compare the\n"+
			"     addresses.\n\n"+
			"Only (1) and (2) are regressions. Then rerun with -update-crypto-bench.\n  %s",
		cryptoBenchTolerance*100, strings.Join(slower, "\n  "))
	assert.Empty(t, faster,
		"the crypto signing path costs less than the baseline says, by more than this instrument's own\n"+
			"interval plus its tolerance. That is the good direction and it is still a change someone has\n"+
			"to look at: the cheap way to get it is to stop heap-allocating something, and from here that\n"+
			"is indistinguishable between an escape analysis that got better and one that got permissive\n"+
			"about an object which outlives its frame. Check the census diff for the sites that moved\n"+
			"HEAP -> FRAME and say what proves each one cannot outlive the frame. Then rerun with\n"+
			"-update-crypto-bench.\n  %s",
		strings.Join(faster, "\n  "))
	assert.Empty(t, reference,
		"cmd/compile's index moved. goc did not necessarily change at all: this is the reference\n"+
			"column and it moves when the host toolchain does. Check the toolchain recorded at the\n"+
			"top of %s first, and the machine's load second.\n  %s",
		cryptoBenchBaseline, strings.Join(reference, "\n  "))
	assert.Empty(t, appeared,
		"the program measures a case %s does not list. Expected when a case is added, in which case\n"+
			"rerun with -update-crypto-bench.\n  %s", cryptoBenchBaseline, strings.Join(appeared, "\n  "))
	assert.Empty(t, vanished,
		"%s lists a case the program no longer measures. Expected when a case is removed;\n"+
			"otherwise the program and the baseline have drifted apart.\n  %s",
		cryptoBenchBaseline, strings.Join(vanished, "\n  "))
}

func relativeChange(before, after float64) float64 {
	if before == 0 {
		return 0
	}
	return (after - before) / before
}
