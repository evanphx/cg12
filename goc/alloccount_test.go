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
// recorded and not just goc's side of it. Every row where the two differ is goc
// paying more, and each says why; a row that starts paying less is as much a
// thing to look at on a diff as one that starts paying more, because the way an
// allocation stops happening can be an object moving into a frame it should not
// be in.
var gocAllocationCounts = []struct {
	name string
	// count is allocations per call times 100, under goc.
	count int
	// host is what the same row costs under the host toolchain, for reading;
	// TestAllocationCountsAgainstTheHostToolchain is what enforces it.
	host int
	why  string
}{
	// The headline, and parity. Both compilers pay for the result string and
	// nothing else. fmt's doPrintf assigns each element to p.arg, a field of a
	// heap-allocated printer, so the boxed 42 genuinely is retained and goes to
	// the heap -- where runtime.convT64 hands back a pointer into
	// runtime.staticuint64s and allocates nothing. The `...` array does not go
	// with it any more: it is a frame slot, which is where gc has always kept
	// it. See goc.gen.variadicPayloadStorage for why those are two answers now
	// and not one.
	{"sprintf_int", 100, 100, "the result string; the `...` array is a frame slot and the box is in the static table"},
	// Parity: the result string plus one box each, for the same reason.
	{"sprintf_string", 200, 200, "the result string, and the boxed string fmt retains"},
	{"sprintf_struct", 200, 200, "the result string, and the boxed struct fmt retains"},
	// No variadic arguments at all. This row is what proved the third
	// allocation had nothing to do with the `...`: it was the interface
	// sync.Pool.Get returns from newPrinter.
	{"sprintf_no_args", 100, 100, "the result string"},
	// The variadic call to a callee that keeps nothing, which is what the `...`
	// backing array question was really about. Both are now free.
	{"variadic_ints", 0, 0, "the `...` backing array is a frame slot"},
	{"variadic_any", 0, 0, "backing array and boxed payloads, all in the frame"},
	// The retention rows, which are the ones a wrong answer here shows up in.
	// keepElement stores args[0] into a package-level variable, so the boxed
	// payload outlives the call and the `[N]any` array does not. Both compilers
	// pay for the box and neither pays for the array. This is the case an
	// earlier attempt at this got wrong in the dangerous direction -- it
	// promoted the whole combined object, leaving a global holding a pointer
	// into a returned frame -- and the reason it can be answered now is that the
	// array and the payload are separate allocations with separate answers.
	{"variadic_retained_element", 100, 100, "the retained box; the `...` array stays in the frame"},
	// The same retention with a payload the runtime has no conversion helper
	// for, which therefore stays a field of the combined object. The array goes
	// to the heap with it, because they are one object -- the old arithmetic,
	// still exactly right for this shape, and still one allocation rather than
	// two.
	{"variadic_retained_struct_element", 100, 100, "one combined object; the struct payload cannot be split out"},
	// Two convertible payloads in one call, which is where splitting stops
	// paying: two escaping payloads out of a framed array cost two allocations
	// where the combined object costs one, so opt.foldSplitPayloadsBackIn sends
	// the array to the heap and takes them back. goc therefore pays 2 for both
	// rows, and the two host numbers either side of it are what that is being
	// compared against: gc splits unconditionally, which wins when the values
	// are inside runtime.staticuint64s and loses when they are not.
	{"sprintf_two_small_ints", 200, 100, "the result string and the combined object; gc's two boxes are both free"},
	{"sprintf_two_large_ints", 200, 300, "the result string and the combined object; gc allocates a box each"},
	// The six shapes of `any(x)`, and what each one costs once goc builds an
	// escaping payload with runtime.convT* instead of runtime.newobject.
	//
	// The fast path is about the *value*, not the type: convT64 hands back a
	// pointer into runtime.staticuint64s for anything below 256 and calls the
	// allocator for everything else. box_small_int and box_bool are on the free
	// side of that line and box_large_int and box_float64 -- 3.5's bit pattern
	// is nowhere near it -- are not. Nothing here would notice if the fast path
	// were wired to fire on the type instead, which is what the two rows either
	// side of the line are for.
	//
	// The four zeros in the host column that goc does not match are gc's escape
	// analysis, not gc's static table: `-gcflags=-m` says "theLargeInt does not
	// escape" at each of them, because takeAny does not leak its parameter, so
	// gc puts the payload in a frame and never reaches a conversion helper at
	// all. return_any_from_large_int is the same value in a shape gc cannot
	// frame, and there the two agree exactly.
	{"box_small_int", 0, 0, "convT64 returns a pointer into staticuint64s"},
	{"box_large_int", 100, 0, "past the static table, so convT64 allocates; gc frames it"},
	{"box_bool", 0, 0, "a bool is a byte, and every byte is in the static table"},
	{"box_float64", 100, 0, "3.5's bits are past the static table; gc frames it"},
	{"box_string", 100, 0, "a string payload has no register-shaped helper; gc frames it"},
	{"box_pointer", 0, 0, "a pointer is its own interface payload"},
	{"return_any_from_int", 0, 0, "convT64 returns a pointer into staticuint64s"},
	{"return_any_from_large_int", 100, 100, "it escapes and it is past the table, so both allocate"},
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
