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

// cryptoBenchTolerance is how far an index may move before the run fails, as a
// fraction.
//
// It is 0.04 because that is three times the measured run-to-run spread and
// still well below the smallest movement anyone has claimed is worth acting on.
// On the box this baseline was taken on, ten consecutive runs of the same
// goc-built binary put the p256/sign-verify index between 45.669 and 46.301 --
// 1.38% peak to peak, sd 0.46% -- which is far tighter than the same runs' raw
// nanoseconds (3.7% peak to peak), because dividing by a control measured in the
// same process is what removes the machine's drift. The movement this instrument
// exists to have caught, the escape publication fix's reported effect on
// bigmod.Nat.Mul, was +5.8%; the change in that fix's neighbourhood that this
// tree did make, main against the pre-fix commit, was -4.9%. A tolerance of 0.04
// cannot fire on noise and would have caught either.
//
// A tolerance is not a licence to let the number drift. The baseline is
// committed, so a movement of 2% that stays under the tolerance still shows up
// as a changed number the next time someone reruns with -update-crypto-bench.
// That is the point of committing measured values rather than thresholds.
//
// An index is comparable across machines only to the extent that the ratio
// between integer arithmetic and P-256 arithmetic is; a box with a very
// different memory system may need its own baseline, which is what
// -update-crypto-bench and the recorded toolchain line are for.
const cryptoBenchTolerance = 0.04

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
# The run needs a Go toolchain and a system linker, and takes about 65 seconds,
# most of which is the goc-built binary doing real P-256 arithmetic. Run it on an
# idle machine: unlike every other baseline in this directory it measures time,
# so a loaded box produces a number about the box.
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
# # Why this file exists
#
# The escape publication fix (6245dbb) was measured at +5.8% on 200 P-256
# sign+verify when it landed. Nothing in this tree measured elapsed time, so that
# number went unchecked across the seven compiler changes that followed it and
# had to be re-established from scratch. See CCWORK_REPORT.md, "Recovered: the
# bigmod.Nat.Mul cost of the escape publication fix".
#
# gc's index is the reference. It should not move unless the host toolchain did;
# this file records the toolchain it was produced against for that reason.
#
# # A movement is not automatically a regression
#
# goc gives function entries no alignment, so where the crypto code lands inside
# the processor's 32-byte instruction fetch granule is decided by the total size
# of everything emitted before it. Measured on the box this baseline was taken
# on, with the compiled program held byte-for-byte identical and only the text
# section shifted, this index is a square wave in that shift: period 32 bytes,
# amplitude 6.1%, which is more than the tolerance. Before treating a movement
# here as a regression, check whether the path's instruction *encodings* changed
# at all -- goc/cryptobench_test.go's failure message gives the commands. A
# movement with identical encodings is a placement flip and nothing got worse.
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
// # The third cause, which is not in the failure message's first two
//
// A movement here has three possible causes and not two. The first two are the
// ones this file was built for: an allocation moved, or the code generated for
// the path changed. The third is that the code did not change at all and *moved*.
//
// It is not a small effect and it is not hypothetical. Measured on this box,
// holding the compiled program byte-for-byte identical and shifting the whole
// text section by K bytes, the p256/sign-verify index is a square wave in K with
// a period of 32 bytes -- the Neoverse-N1 instruction fetch granule -- and an
// amplitude of 6.1%:
//
//	K mod 32 = 0    index 47.72 - 47.86
//	K mod 32 = 16   index 44.79 - 44.83
//
// The tolerance is 0.04. So a change anywhere in the program that alters the
// number of bytes emitted before the crypto code -- including a change to a cold
// branch in an unrelated package, which is what 96996b0 was -- can move this row
// by more than the tolerance while the ECDSA path's instructions are unchanged.
// goc gives function entries no alignment (arm64/mc.go lays each function down at
// len(o.Text)), so which half of the wave a build lands in is decided by the
// running total of every byte emitted before it.
//
// Which is to say: this row failing is not by itself evidence that anything got
// worse. compareCryptoBench's message says how to tell the three apart.
func TestCryptoSigningBench(t *testing.T) {
	if !*measureCryptoBench && !*updateCryptoBench {
		t.Skip("pass -crypto-bench to build goc/testdata/crypto_signing_bench with both compilers and compare elapsed time")
	}

	goVersion := hostGoVersion(t)
	t.Logf("host toolchain: %s", goVersion)

	scratch := t.TempDir()
	gcBinary := buildCryptoBenchWithHostToolchain(t, scratch)
	gocBinary := buildCryptoBenchWithGoc(t, scratch)

	gcTimes, method := runCryptoBench(t, gcBinary, "")
	gocTimes, method := runCryptoBench(t, gocBinary, method)

	rows := cryptoBenchRows(t, gocTimes, gcTimes)

	rendered := renderCryptoBench(goVersion, method, rows)
	if *updateCryptoBench {
		require.NoError(t, os.WriteFile(cryptoBenchBaseline, []byte(rendered), 0o644))
		t.Skip("baseline rewritten; rerun without -update-crypto-bench to check it")
	}

	// The instrument, checked before anything measured with it is believed. If
	// the empty case is not far below the control then the harness is a
	// measurable part of every row and no row means anything.
	for _, side := range []struct {
		compiler string
		times    map[string]float64
	}{{"goc", gocTimes}, {"gc", gcTimes}} {
		harnessShare := side.times[cryptoBenchHarness] / side.times[cryptoBenchControl]
		require.Less(t, harnessShare, cryptoBenchHarnessCeiling,
			"%s is a calibration case: under %s it cost %.4f of %s, which means the harness is a\n"+
				"measurable part of every row and no other number in this run means anything.",
			cryptoBenchHarness, side.compiler, harnessShare, cryptoBenchControl)
	}

	accepted, err := os.ReadFile(cryptoBenchBaseline)
	require.NoError(t, err)
	compareCryptoBench(t, parseCryptoBenchBaseline(string(accepted)), rows)
}

// cryptoBenchRow is one case's pair of indexes.
type cryptoBenchRow struct {
	name              string
	gocIndex          float64
	gcIndex           float64
	gocNanos, gcNanos float64
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
func runCryptoBench(t *testing.T, binary, method string) (map[string]float64, string) {
	t.Helper()

	output, err := exec.Command(binary).Output()
	require.NoError(t, err, "%s did not run", binary)

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
func cryptoBenchRows(t *testing.T, goc, gc map[string]float64) []cryptoBenchRow {
	t.Helper()

	names := make([]string, 0, len(goc))
	for name := range goc {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]cryptoBenchRow, 0, len(names))
	for _, name := range names {
		if name == cryptoBenchControl || name == cryptoBenchHarness {
			continue
		}
		require.Contains(t, gc, name, "the gc-built binary did not report %s", name)
		rows = append(rows, cryptoBenchRow{
			name:     name,
			gocIndex: goc[name] / goc[cryptoBenchControl],
			gcIndex:  gc[name] / gc[cryptoBenchControl],
			gocNanos: goc[name],
			gcNanos:  gc[name],
		})
	}
	return rows
}

func renderCryptoBench(goVersion, method string, rows []cryptoBenchRow) string {
	var out strings.Builder
	out.WriteString(cryptoBenchHeader)
	fmt.Fprintf(&out, "host toolchain: %s\n", goVersion)
	fmt.Fprintf(&out, "measurement:    %s\n", method)
	fmt.Fprintf(&out, "index:          case time / %s time, same binary, same process\n", cryptoBenchControl)
	fmt.Fprintf(&out, "tolerance:      %.2f of the index, both directions\n\n", cryptoBenchTolerance)

	fmt.Fprintf(&out, "%-24s %12s %12s %14s %14s\n", "case", "goc index", "gc index", "goc ns (fyi)", "gc ns (fyi)")
	for _, row := range rows {
		fmt.Fprintf(&out, "%-24s %12.4f %12.4f %14.0f %14.0f\n",
			row.name, row.gocIndex, row.gcIndex, row.gocNanos, row.gcNanos)
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
		if len(fields) != 5 || fields[0] == "case" {
			continue
		}
		gocIndex, gocError := strconv.ParseFloat(fields[1], 64)
		gcIndex, gcError := strconv.ParseFloat(fields[2], 64)
		if gocError != nil || gcError != nil {
			continue
		}
		rows = append(rows, cryptoBenchRow{name: fields[0], gocIndex: gocIndex, gcIndex: gcIndex})
	}
	return rows
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

	var slower, faster, reference, appeared, vanished []string
	for _, row := range found {
		before, known := acceptedByName[row.name]
		if !known {
			appeared = append(appeared, fmt.Sprintf("%s\n      now index %.4f under goc", row.name, row.gocIndex))
			continue
		}
		if change := relativeChange(before.gocIndex, row.gocIndex); math.Abs(change) > cryptoBenchTolerance {
			line := fmt.Sprintf("%s\n      goc index %.4f -> %.4f (%+.1f%%)",
				row.name, before.gocIndex, row.gocIndex, change*100)
			if change > 0 {
				slower = append(slower, line)
			} else {
				faster = append(faster, line)
			}
		}
		if change := relativeChange(before.gcIndex, row.gcIndex); math.Abs(change) > cryptoBenchTolerance {
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
		"the crypto signing path costs more than the baseline says. This is paid by every ECDSA\n"+
			"operation in every program goc compiles. There are three causes and they are\n"+
			"distinguishable; work down the list before accepting or reverting anything.\n\n"+
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
			"     symbol addresses differ by a constant, this is a code placement flip, and the\n"+
			"     honest answer is that nothing got worse. See TestCryptoSigningBench's doc: the\n"+
			"     index is a square wave in the text offset, period 32 bytes, amplitude 6.1%%, and\n"+
			"     the tolerance is 4%%. This is what happened at 96996b0.\n\n"+
			"Only (1) and (2) are regressions. Then rerun with -update-crypto-bench.\n  %s",
		strings.Join(slower, "\n  "))
	assert.Empty(t, faster,
		"the crypto signing path costs less than the baseline says. That is the good direction and\n"+
			"it is still a change someone has to look at: the cheap way to get it is to stop\n"+
			"heap-allocating something, and from here that is indistinguishable between an escape\n"+
			"analysis that got better and one that got permissive about an object which outlives its\n"+
			"frame. Check the census diff for the sites that moved HEAP -> FRAME and say what proves\n"+
			"each one cannot outlive the frame. Then rerun with -update-crypto-bench.\n  %s",
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
