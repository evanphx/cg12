package goc_test

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The allocation counts of the calls Go programs make most often, measured by
// compiling testdata/allocation_counts.go, linking it against the
// cg12-compiled Go runtime, running it, and reading what it printed.
//
// # Why this exists as well as TestAllocationCensus
//
// The census records where each allocation *site* landed. It cannot say what a
// call *costs*, and the two are not the same question:
//
//   - One site can be several allocations. `fmt.Sprintf("value=%d", 42)` cost
//     goc three where gc costs one, and the census shows one line for the
//     variadic backing object. The other two -- the interface sync.Pool.Get
//     handed back, and the result string -- are inside fmt, spread over sites
//     the census lists but nobody would think to connect to Sprintf.
//
//   - A site can go away without the cost going away, and the reverse. The
//     census does not record ordinary front-end frame slots at all, so a
//     placement that moves out of its jurisdiction reads as "vanished".
//
// So this measures the thing that is actually being paid for. The numbers below
// were produced by running the program, not by reasoning about it, and the
// counts are allocations per call times 100.
//
// # It fails in both directions, deliberately
//
// More allocations is the regression this exists to prevent. Fewer is not a
// free win to be pocketed silently: an allocation that stops happening because
// an object moved into a frame is the correctness-critical direction -- the
// same one TestAllocationCensus's first review question is about -- and it
// should be looked at and then written down here, not absorbed. Both are an
// exact-equality failure, and the fix for either is to understand the change
// and edit the table.
//
// # The gc column
//
// TestAllocationCountsAgainstTheHostToolchain runs the same program through the
// host `go` and holds it to the host column of this table, so the *gap* is
// recorded and
// not just goc's side of it. Two rows below are goc paying less than gc, which
// is as much a thing to notice on a diff as paying more.
var gocAllocationCounts = []struct {
	name string
	// count is allocations per call times 100, under goc.
	count int
	// host is what the same row costs under the host toolchain, for reading;
	// TestAllocationCountsAgainstTheHostToolchain is what enforces it.
	host int
	why  string
}{
	// The headline. gc keeps the `...` array in the frame and boxes 42 into
	// runtime.staticuint64s; goc keeps the array *and* the box in one frame
	// object. Both then pay for the result string, and that is the 1.
	{"sprintf_int", 100, 100, "the result string, and nothing else"},
	// goc pays less than gc here: gc's `...` array is in a frame but the string
	// still boxes onto the heap, where goc's front end packs the box into the
	// same frame object as the array.
	{"sprintf_string", 100, 200, "the result string; the box rides in the frame"},
	{"sprintf_struct", 100, 200, "the result string; the box rides in the frame"},
	// No variadic arguments at all. This row is what proved the third
	// allocation had nothing to do with the `...`: it was the interface
	// sync.Pool.Get returns from newPrinter.
	{"sprintf_no_args", 100, 100, "the result string"},
	{"variadic_ints", 0, 0, "the `...` backing array is a frame slot"},
	{"variadic_any", 0, 0, "backing array and boxed payloads, one frame object"},
	// goc has no convT64/staticuint64s fast path: every interface conversion of
	// a non-pointer value is a runtime.newobject. gc returns a pointer into a
	// static table for a small integer and allocates nothing.
	{"box_small_int", 100, 0, "no staticuint64s fast path in goc"},
	{"box_pointer", 0, 0, "a pointer is its own interface payload"},
	{"return_any_from_int", 100, 0, "the box; the descriptor is a frame slot"},
	{"return_any_from_pointer", 0, 0, "nothing to box, nothing to allocate"},
	{"sync_pool_round_trip", 0, 0, "Get's interface result is returned in registers"},
	// The two rows written inside a loop body. opt.promotionsBlockedByALoop
	// refuses to promote an allocation in a loop, because a frame slot is one
	// object and each iteration may need its own -- see
	// TestLoopBodyAllocationsAreDistinctPerIteration for what that rule is
	// protecting. The rule is blunt: it does not ask whether anything retains
	// the object past the iteration, and for a variadic backing array nothing
	// does. These numbers are the price of that bluntness, and lowering them is
	// a real optimisation someone could do.
	{"sprintf_in_loop", 200, 100, "loop rule blocks the `...` promotion"},
	{"variadic_ints_in_loop", 100, 0, "loop rule blocks the `...` promotion"},
}

// TestAllocationCounts compiles, links and runs testdata/allocation_counts.go
// and holds every measured count to the table above.
func TestAllocationCounts(t *testing.T) {
	for _, optimized := range []bool{false, true} {
		name := "allocation_counts.go"
		if optimized {
			name += " -O"
		}
		t.Run(name, func(t *testing.T) {
			counts := parseAllocationCounts(t,
				runCorpusProgramOutput(t, filepath.Join("testdata", "allocation_counts.go"), optimized))
			for _, expected := range gocAllocationCounts {
				measured, reported := counts[expected.name]
				require.True(t, reported, "%s printed no %s row", "allocation_counts.go", expected.name)
				require.Equal(t, expected.count, measured,
					"%s costs %d allocations per 100 calls under goc, and this table says %d (%s).\n"+
						"More is the regression this test exists to catch. Fewer is not automatically\n"+
						"good: an allocation stops happening when an object moves into a frame, which is\n"+
						"the direction that can leave the caller holding a dead pointer. Find out which\n"+
						"of the two it is, read TestFrameEscapeAudit and TestAllocationCensus alongside\n"+
						"it, and then edit the table.",
					expected.name, measured, expected.count, expected.why)
			}
			require.Len(t, counts, len(gocAllocationCounts),
				"the program printed rows this table does not list, or the reverse")
		})
	}
}

// TestAllocationCountsAgainstTheHostToolchain runs the same program through the
// host `go` and holds it to the host column of the table.
//
// Without it the host numbers would be a belief about what gc does rather than
// a measurement of it, and the gap they exist to express -- which rows goc pays
// more for, and which it pays less -- would quietly stop meaning anything as
// the host toolchain changed. It skips where no toolchain is installed, which
// is why the goc column is enforced separately above.
func TestAllocationCountsAgainstTheHostToolchain(t *testing.T) {
	toolchain, err := exec.LookPath("go")
	if err != nil {
		t.Skip("host Go toolchain unavailable")
	}
	command := exec.Command(toolchain, "run", filepath.Join("testdata", "allocation_counts.go"))
	output, err := command.CombinedOutput()
	require.NoError(t, err, "go run: %s", output)

	counts := parseAllocationCounts(t, string(output))
	for _, expected := range gocAllocationCounts {
		measured, reported := counts[expected.name]
		require.True(t, reported, "the host toolchain printed no %s row", expected.name)
		require.Equal(t, expected.host, measured,
			"the host toolchain costs %d allocations per 100 calls for %s, and this table's\n"+
				"host column says %d. Either the toolchain changed or the column was wrong;\n"+
				"either way the gap goc is being compared against has moved.",
			measured, expected.name, expected.host)
	}
}

// parseAllocationCounts reads the program's "name count" lines. The counts are
// allocations per call times 100.
func parseAllocationCounts(t *testing.T, output string) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		require.Len(t, fields, 2, "unreadable line %q in:\n%s", line, output)
		count, err := strconv.Atoi(fields[1])
		require.NoError(t, err, "unreadable count in line %q", line)
		counts[fields[0]] = count
	}
	return counts
}
