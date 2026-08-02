package goc_test

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// measureSlogAllocations asks for the slog allocation comparison to be run. It
// is opt-in for the same reason TestEscapeDifferentialAgainstGC is: it needs the
// host Go toolchain, which cannot be part of this repository's contract. It is
// committed, and its output is committed, so the numbers can be diffed later
// instead of being reconstructed.
var measureSlogAllocations = flag.Bool("slog-allocations", false,
	"build goc/testdata/slog_allocations with goc and with the host Go toolchain and compare what each one allocates")

// updateSlogAllocations rewrites the checked-in output from this run.
var updateSlogAllocations = flag.Bool("update-slog-allocations", false,
	"rewrite testdata/slog_allocations_baseline.txt from this run")

const (
	slogAllocationsProgram  = "testdata/slog_allocations/main.go"
	slogAllocationsBaseline = "testdata/slog_allocations_baseline.txt"
)

// The tolerances the comparison uses, and why they are the size they are.
//
// Allocation counts per operation are integers: a case either allocates or it
// does not, and the measurement divides an exact malloc count by an exact
// iteration count. The smallest real change is therefore 1.00, and a tolerance
// of 0.05 cannot hide one -- it exists only so that a stray background
// allocation somewhere in a 2000-iteration round, which is 0.0005, is not
// reported as a regression.
//
// Bytes are the same argument at a different scale. The smallest interesting
// change is a size class, which is 8 bytes.
const (
	allocationsTolerance = 0.05
	bytesTolerance       = 0.5
)

const slogAllocationsHeader = `# What log/slog's allocation-avoidance designs actually cost, measured under goc
# and under the host Go toolchain, one case per line. Regenerate with
#
#     go test ./goc -run TestSlogAllocationsAgainstGC \
#         -slog-allocations -update-slog-allocations
#
# and read the diff. The run needs a Go toolchain and a system linker; it takes
# about a minute, most of which is goc compiling log/slog once.
#
# Both columns come from the same program -- goc/testdata/slog_allocations --
# compiled by both compilers and run once per case, so the two answers are
# produced by the same measurement and not by two benchmarks that happen to be
# named alike. That program's doc comment is where the method is written down.
# The short version: each case is a closure called through a func value, run to
# warm up and then measured over several rounds of runtime.MemStats deltas,
# keeping the round with the fewest mallocs.
#
# "crash" is not a zero. It means the compiled program died before it printed
# the row; the reason is listed under the table. A row that changes from a
# number to "crash" is the most serious diff this file can carry.
#
# gc's numbers are the reference, so they should not move unless the host
# toolchain did; this file records the toolchain it was produced against for
# exactly that reason.
#
`

// TestSlogAllocationsAgainstGC measures how much of log/slog's designed
// allocation avoidance survives compilation by goc.
//
// # Why slog
//
// Most packages benefit from escape analysis. log/slog's data structures were
// designed around it, and three of them are bets on the compiler rather than on
// the algorithm: Record.front is an inline [5]Attr so a small record needs no
// slice (record.go's nAttrsInline), Value packs int64, bool, Duration and
// float64 into a uint64 so those four are not boxed, and Logger.log returns
// before doing anything when the level is disabled so a disabled call is
// supposed to be free. Each one pays off only if the compiler keeps a variadic
// []any off the heap, resolves a constant interface conversion to a static
// symbol, and does not allocate for a value that never leaves the frame. A
// compiler that is more pessimistic than the reference implementation pays for
// all three designs and gets nothing back, which is what makes slog the place
// the gap shows up worst.
//
// # Why it is a baseline and not a threshold
//
// There is no number of allocations that is correct here, exactly as there is no
// allocation census that is correct. The file exists so that a person looks at
// what moved. Both directions are news:
//
//   - More allocations is a regression. Something in goc got more pessimistic,
//     and slog is a hot path in real programs.
//
//   - Fewer allocations is a change someone should look at, not a free win. It
//     is what a fix to interface boxing or variadic placement is supposed to
//     look like, so the diff is the evidence that the fix worked -- and if
//     nobody was working on one, an allocation that stopped happening needs the
//     same question asked of it as one that started.
//
//   - A row that stops crashing, or starts, is neither. It is a correctness
//     change and the diff should say which bug.
//
// The gc columns are the reference. They move when the host toolchain does and
// otherwise should not move at all; a diff confined to them is a toolchain
// diff, not a goc one.
func TestSlogAllocationsAgainstGC(t *testing.T) {
	if !*measureSlogAllocations && !*updateSlogAllocations {
		t.Skip("pass -slog-allocations to compile goc/testdata/slog_allocations with both compilers and compare")
	}

	goVersion := hostGoVersion(t)
	t.Logf("host toolchain: %s", goVersion)

	scratch := t.TempDir()
	gcBinary := buildWithHostToolchain(t, scratch)
	gocBinary := buildWithGoc(t, scratch)

	names := listSlogCases(t, gcBinary)
	require.NotEmpty(t, names)
	t.Logf("%d cases", len(names))

	rows := make([]slogRow, 0, len(names))
	var method string
	for _, name := range names {
		row := slogRow{name: name}
		row.goc, row.gocCrash, method = runSlogCase(t, gocBinary, name, method)
		row.gc, row.gcCrash, method = runSlogCase(t, gcBinary, name, method)
		rows = append(rows, row)
	}

	rendered := renderSlogAllocations(goVersion, method, rows)
	if *updateSlogAllocations {
		require.NoError(t, os.WriteFile(slogAllocationsBaseline, []byte(rendered), 0o644))
		t.Skip("baseline rewritten; rerun without -update-slog-allocations to check it")
	}

	// The instrument, checked before anything measured with it is believed.
	// These two cases are in the program for this: an empty body must cost
	// nothing and a new [64]byte must cost exactly one 64-byte allocation, under
	// both compilers. If either is wrong the run is measuring the measurement
	// and every other row in it is worthless.
	requireInstrument(t, rows, "control/empty-body", 0, 0)
	requireInstrument(t, rows, "control/new-64-byte-object", 1, 64)

	accepted, err := os.ReadFile(slogAllocationsBaseline)
	require.NoError(t, err)
	compareSlogAllocations(t, parseSlogBaseline(string(accepted)), rows)
}

// slogRow is one case's pair of answers.
type slogRow struct {
	name     string
	goc, gc  measurement
	gocCrash string
	gcCrash  string
}

// measurement is what one binary reported for one case.
type measurement struct {
	allocations float64
	bytes       float64
}

// buildWithHostToolchain compiles the benchmark with the host go.
func buildWithHostToolchain(t *testing.T, scratch string) string {
	t.Helper()

	binary := filepath.Join(scratch, "slogalloc.gc")
	build := exec.Command("go", "build", "-o", binary, slogAllocationsProgram)
	build.Env = append(os.Environ(), "GOFLAGS=")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "the host toolchain would not build %s:\n%s", slogAllocationsProgram, output)
	return binary
}

// buildWithGoc compiles the benchmark with goc, building the driver first.
//
// The driver is built rather than `go run`, because `go run` reports its own
// exit status for the program it ran and this test needs to tell a compile
// failure apart from a program that died.
func buildWithGoc(t *testing.T, scratch string) string {
	t.Helper()

	driver := filepath.Join(scratch, "goc")
	build := exec.Command("go", "build", "-o", driver, "github.com/evanphx/cg12/cmd/goc")
	build.Env = append(os.Environ(), "GOFLAGS=")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "could not build the goc driver:\n%s", output)

	binary := filepath.Join(scratch, "slogalloc.goc")
	compile := exec.Command(driver, "-o", binary, slogAllocationsProgram)
	output, err = compile.CombinedOutput()
	require.NoError(t, err, "goc would not build %s:\n%s", slogAllocationsProgram, output)
	return binary
}

// listSlogCases asks the program which cases it has, so the table's contents
// live in the program and not in two places.
func listSlogCases(t *testing.T, binary string) []string {
	t.Helper()

	output, err := exec.Command(binary, "-list").Output()
	require.NoError(t, err)
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

// runSlogCase runs one case in its own process.
//
// One process per case is not a convenience. goc currently dies with a fatal
// runtime error on some of these cases, and a process per case means such a
// case costs one row rather than every row after it. It also keeps each
// measurement from inheriting a heap that previous cases shaped.
//
// method is the "# iterations=... rounds=..." line the program prints. It is
// threaded through so that every run in a comparison can be checked to have used
// the same parameters: a baseline whose two columns were measured differently
// would be worse than no baseline.
func runSlogCase(t *testing.T, binary, name, method string) (measurement, string, string) {
	t.Helper()

	command := exec.Command(binary, name)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	runError := command.Run()

	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "#") {
			if method == "" {
				method = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			}
			require.Equal(t, method, strings.TrimSpace(strings.TrimPrefix(line, "#")),
				"%s measured %s with different parameters than an earlier run used", binary, name)
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] != name {
			continue
		}
		allocations, err := strconv.ParseFloat(fields[1], 64)
		require.NoError(t, err)
		bytes, err := strconv.ParseFloat(fields[2], 64)
		require.NoError(t, err)
		return measurement{allocations: allocations, bytes: bytes}, "", method
	}

	reason := fatalReason(stderr.String())
	if reason == "" && runError != nil {
		reason = runError.Error()
	}
	if reason == "" {
		reason = "printed no row and said nothing"
	}
	return measurement{}, reason, method
}

// hexAddress matches an address in a runtime diagnostic, which changes from run
// to run and would make the baseline rewrite itself every time.
var hexAddress = regexp.MustCompile(`0x[0-9a-f]{6,}`)

// fatalReason reduces a dead program's output to the one line worth recording.
func fatalReason(stderr string) string {
	var runtimeNote string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "fatal error:"), strings.HasPrefix(line, "panic:"):
			if runtimeNote != "" {
				return line + " (" + runtimeNote + ")"
			}
			return line
		case strings.HasPrefix(line, "runtime:"), strings.HasPrefix(line, "cg12:"):
			// The line above a fatal error is usually the one that says what was
			// actually wrong; "fatal error: invalid pointer found on stack" does
			// not say which pointer or which frame, and the line before it does.
			runtimeNote = hexAddress.ReplaceAllString(line, "0x?")
		}
	}
	return runtimeNote
}

// requireInstrument fails the whole run when a calibration case does not measure
// what it is defined to measure.
func requireInstrument(t *testing.T, rows []slogRow, name string, allocations, bytes float64) {
	t.Helper()

	for _, row := range rows {
		if row.name != name {
			continue
		}
		require.Empty(t, row.gocCrash, "the instrument case %s did not run under goc", name)
		require.Empty(t, row.gcCrash, "the instrument case %s did not run under gc", name)
		for _, side := range []struct {
			compiler string
			measured measurement
		}{{"goc", row.goc}, {"gc", row.gc}} {
			require.InDelta(t, allocations, side.measured.allocations, allocationsTolerance,
				"%s is a calibration case: %s must report %.2f allocations for it or the run is\n"+
					"measuring the measurement rather than the program, and no other row means anything.",
				name, side.compiler, allocations)
			require.InDelta(t, bytes, side.measured.bytes, bytesTolerance,
				"%s is a calibration case: %s must report %.1f bytes for it or the run is measuring\n"+
					"the measurement rather than the program, and no other row means anything.",
				name, side.compiler, bytes)
		}
		return
	}
	require.Fail(t, "the calibration case "+name+" is not in the program any more")
}

func renderSlogAllocations(goVersion, method string, rows []slogRow) string {
	var out strings.Builder
	out.WriteString(slogAllocationsHeader)
	fmt.Fprintf(&out, "host toolchain: %s\n", goVersion)
	fmt.Fprintf(&out, "measurement:    %s\n\n", method)

	fmt.Fprintf(&out, "%-30s %10s %10s %10s %10s\n", "case", "goc a/op", "gc a/op", "goc B/op", "gc B/op")
	for _, row := range rows {
		fmt.Fprintf(&out, "%-30s %10s %10s %10s %10s\n", row.name,
			formatAllocations(row.goc.allocations, row.gocCrash),
			formatAllocations(row.gc.allocations, row.gcCrash),
			formatBytes(row.goc.bytes, row.gocCrash),
			formatBytes(row.gc.bytes, row.gcCrash))
	}

	crashes := false
	for _, row := range rows {
		for _, crash := range []struct{ compiler, reason string }{{"goc", row.gocCrash}, {"gc", row.gcCrash}} {
			if crash.reason == "" {
				continue
			}
			if !crashes {
				out.WriteString("\ncases that did not run:\n\n")
				crashes = true
			}
			fmt.Fprintf(&out, "  %-30s %-4s %s\n", row.name, crash.compiler, crash.reason)
		}
	}
	return out.String()
}

func formatAllocations(value float64, crash string) string {
	if crash != "" {
		return "crash"
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

func formatBytes(value float64, crash string) string {
	if crash != "" {
		return "crash"
	}
	return strconv.FormatFloat(value, 'f', 1, 64)
}

// parseSlogBaseline reads the table back out of the checked-in file. Only the
// table is parsed: the crash reasons under it are for a person, and are
// reproduced from the run rather than compared, so that a runtime diagnostic
// being reworded is not a test failure.
func parseSlogBaseline(text string) []slogRow {
	var rows []slogRow
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "  ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[0] == "case" {
			continue
		}
		row := slogRow{name: fields[0]}
		row.goc, row.gocCrash = parseSlogCell(fields[1], fields[3])
		row.gc, row.gcCrash = parseSlogCell(fields[2], fields[4])
		// Prose in the file can have five words on a line -- "cases that did not
		// run:" does -- and a line whose cells are neither a number nor "crash"
		// is prose, not a row that vanished.
		if row.gocCrash == "unparseable" || row.gcCrash == "unparseable" {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

func parseSlogCell(allocations, bytes string) (measurement, string) {
	if allocations == "crash" || bytes == "crash" {
		return measurement{}, "crash"
	}
	parsedAllocations, allocationsError := strconv.ParseFloat(allocations, 64)
	parsedBytes, bytesError := strconv.ParseFloat(bytes, 64)
	if allocationsError != nil || bytesError != nil {
		return measurement{}, "unparseable"
	}
	return measurement{allocations: parsedAllocations, bytes: parsedBytes}, ""
}

// compareSlogAllocations sorts the difference between this run and the accepted
// baseline into the things a reviewer has to answer separately, and fails with
// the question each one raises. It is deliberately not a single "the file
// changed" assertion: more allocations, fewer allocations, a new crash and a
// crash that stopped happening are four different pieces of news, and a diff
// that does not say which is which gets read as noise.
func compareSlogAllocations(t *testing.T, accepted, found []slogRow) {
	t.Helper()

	acceptedByName := make(map[string]slogRow, len(accepted))
	for _, row := range accepted {
		acceptedByName[row.name] = row
	}
	foundByName := make(map[string]slogRow, len(found))
	for _, row := range found {
		foundByName[row.name] = row
	}

	var worse, better, crashed, recovered, reference, appeared, vanished []string
	for _, row := range found {
		before, known := acceptedByName[row.name]
		if !known {
			appeared = append(appeared, fmt.Sprintf("%s\n      now %s", row.name, describe(row.goc, row.gocCrash)))
			continue
		}
		switch {
		case before.gocCrash == "" && row.gocCrash != "":
			crashed = append(crashed, fmt.Sprintf("%s\n      was %s under goc, now dies: %s",
				row.name, describe(before.goc, ""), row.gocCrash))
		case before.gocCrash != "" && row.gocCrash == "":
			recovered = append(recovered, fmt.Sprintf("%s\n      used to die under goc, now %s",
				row.name, describe(row.goc, "")))
		case moved(before.goc, row.goc):
			line := fmt.Sprintf("%s\n      goc: %s -> %s", row.name, describe(before.goc, ""), describe(row.goc, ""))
			if row.goc.allocations > before.goc.allocations+allocationsTolerance ||
				row.goc.bytes > before.goc.bytes+bytesTolerance {
				worse = append(worse, line)
			} else {
				better = append(better, line)
			}
		}
		if before.gcCrash != row.gcCrash || moved(before.gc, row.gc) {
			reference = append(reference, fmt.Sprintf("%s\n      gc: %s -> %s",
				row.name, describe(before.gc, before.gcCrash), describe(row.gc, row.gcCrash)))
		}
	}
	for _, row := range accepted {
		if _, ok := foundByName[row.name]; !ok {
			vanished = append(vanished, fmt.Sprintf("%s\n      was %s under goc", row.name, describe(row.goc, row.gocCrash)))
		}
	}
	sort.Strings(worse)
	sort.Strings(better)

	assert.Empty(t, crashed,
		"a case that used to run under goc now kills the program. This is a correctness\n"+
			"regression, not a performance one, and it should be triaged before anything else\n"+
			"in this file is looked at.\n  %s", strings.Join(crashed, "\n  "))
	assert.Empty(t, worse,
		"goc allocates more than the baseline says. log/slog is a hot path in real programs\n"+
			"and every one of these is paid per log call. For each case below, say which cause\n"+
			"it is -- interface boxing with no static table, a variadic []any that had to be\n"+
			"heaped, or something new -- then rerun with -update-slog-allocations.\n  %s",
		strings.Join(worse, "\n  "))
	assert.Empty(t, better,
		"goc allocates less than the baseline says. That is the good direction and it is still\n"+
			"a change someone has to look at: it is what a fix to interface boxing or variadic\n"+
			"placement is supposed to look like, so check that a fix is what caused it and not a\n"+
			"case that quietly stopped doing its work. Then rerun with -update-slog-allocations.\n  %s",
		strings.Join(better, "\n  "))
	assert.Empty(t, recovered,
		"a case that used to kill the program now runs. Find the change that fixed it and say\n"+
			"so, then rerun with -update-slog-allocations.\n  %s", strings.Join(recovered, "\n  "))
	assert.Empty(t, reference,
		"cmd/compile's numbers moved. goc did not necessarily change at all: this is the\n"+
			"reference column, and it moves when the host toolchain does. Check the toolchain\n"+
			"recorded at the top of %s first.\n  %s",
		slogAllocationsBaseline, strings.Join(reference, "\n  "))
	assert.Empty(t, appeared,
		"the program measures a case %s does not list. Expected when a case is added, in which\n"+
			"case rerun with -update-slog-allocations.\n  %s",
		slogAllocationsBaseline, strings.Join(appeared, "\n  "))
	assert.Empty(t, vanished,
		"%s lists a case the program no longer measures. Expected when a case is removed;\n"+
			"otherwise the program and the baseline have drifted apart.\n  %s",
		slogAllocationsBaseline, strings.Join(vanished, "\n  "))
}

func moved(before, after measurement) bool {
	return math.Abs(before.allocations-after.allocations) > allocationsTolerance ||
		math.Abs(before.bytes-after.bytes) > bytesTolerance
}

func describe(measured measurement, crash string) string {
	if crash != "" {
		return "crash"
	}
	return fmt.Sprintf("%.2f allocs/op, %.1f B/op", measured.allocations, measured.bytes)
}
