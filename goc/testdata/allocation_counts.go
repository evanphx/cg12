// Allocation counts for the calls a Go program makes most often, measured by
// running them. TestAllocationCounts holds every line here to a committed
// number and fails in both directions; see goc/alloccount_test.go for why.
//
// The counts are printed as allocations per call times 100, so a change of one
// allocation in a hundred calls is visible rather than rounded away. Each
// operation lives in its own //go:noinline function and the measuring loop
// calls that, so the allocation sites are in ordinary straight-line code --
// which is where they are in real programs. The two `_in_loop` rows are the
// deliberate exception: their allocation is written inside the loop body, which
// is what opt.promotionsBlockedByALoop refuses to promote, and they are here so
// the price of that rule is a number somebody can see.
package main

import (
	"fmt"
	"runtime"
	"sync"
)

var (
	sinkString string
	sinkAny    any
	sinkInt    int

	theInt    = 42
	theString = "answer"
	theStruct = pair{1, 2}
	thePtr    = &theInt

	// theLargeInt is past the last entry of runtime.staticuint64s, and
	// theFloat's bit pattern is far past it, so boxing either has to allocate
	// where boxing theInt does not. They are here to hold the fast path to
	// being about the value and not about the type.
	theLargeInt = 1 << 20
	theBool     = true

	// theFloat is assigned in init rather than in its declaration. goc compiles
	// a package-level float64 initialized to a constant to zero -- a defect that
	// predates this file's float row and has nothing to do with boxing -- and
	// zero's bit pattern does fit staticuint64s, so declaring it here would make
	// this row measure any(0.0) and quietly report a fast path that is not
	// running.
	theFloat float64
)

func init() { theFloat = 3.5 }

type pair struct{ a, b int }

//go:noinline
func sprintfInt() { sinkString = fmt.Sprintf("value=%d", theInt) }

//go:noinline
func sprintfString() { sinkString = fmt.Sprintf("value=%s", theString) }

//go:noinline
func sprintfStruct() { sinkString = fmt.Sprintf("value=%v", theStruct) }

//go:noinline
func sprintfNoArgs() { sinkString = fmt.Sprintf("a constant format") }

//go:noinline
func variadicInts(values ...int) int { return len(values) }

//go:noinline
func callVariadicInts() { sinkInt = variadicInts(theInt, theInt) }

//go:noinline
func variadicAny(values ...any) int { return len(values) }

//go:noinline
func callVariadicAny() { sinkInt = variadicAny(theInt, theString) }

//go:noinline
func takeAny(value any) int {
	if value == nil {
		return 0
	}
	return 1
}

//go:noinline
func boxSmallInt() { sinkInt = takeAny(theInt) }

//go:noinline
func boxLargeInt() { sinkInt = takeAny(theLargeInt) }

//go:noinline
func boxBool() { sinkInt = takeAny(theBool) }

//go:noinline
func boxFloat64() { sinkInt = takeAny(theFloat) }

//go:noinline
func boxString() { sinkInt = takeAny(theString) }

//go:noinline
func boxPointer() { sinkInt = takeAny(thePtr) }

//go:noinline
func returnAnyFromInt(value int) any { return value }

//go:noinline
func returnAnyFromLargeInt(value int) any { return value }

//go:noinline
func returnAnyFromPointer(value *int) any { return value }

//go:noinline
func callReturnAnyFromInt() { sinkAny = returnAnyFromInt(theInt) }

//go:noinline
func callReturnAnyFromLargeInt() { sinkAny = returnAnyFromLargeInt(theLargeInt) }

//go:noinline
func callReturnAnyFromPointer() { sinkAny = returnAnyFromPointer(thePtr) }

var pool = sync.Pool{New: func() any { return new(pair) }}

//go:noinline
func poolRoundTrip() {
	value := pool.Get().(*pair)
	value.a = theInt
	pool.Put(value)
}

//go:noinline
func sprintfInLoop(times int) {
	for i := 0; i < times; i++ {
		sinkString = fmt.Sprintf("value=%d", theInt)
	}
}

//go:noinline
func variadicIntsInLoop(times int) {
	for i := 0; i < times; i++ {
		sinkInt = variadicInts(theInt, theInt)
	}
}

const iterations = 1000

// measure runs the operation `iterations` times after a warm-up of the same
// length, so a first-call cost -- a sync.Pool that has never been filled, a
// buffer grown once and kept -- is paid before the counting starts and is not
// mistaken for a per-call allocation.
func measure(name string, operation func(int)) {
	var before, after runtime.MemStats
	operation(iterations)
	runtime.ReadMemStats(&before)
	operation(iterations)
	runtime.ReadMemStats(&after)
	print(name, " ")
	println(int64(after.Mallocs-before.Mallocs) * 100 / iterations)
}

func repeat(operation func()) func(int) {
	return func(times int) {
		for i := 0; i < times; i++ {
			operation()
		}
	}
}

func main() {
	measure("sprintf_int", repeat(sprintfInt))
	measure("sprintf_string", repeat(sprintfString))
	measure("sprintf_struct", repeat(sprintfStruct))
	measure("sprintf_no_args", repeat(sprintfNoArgs))
	measure("variadic_ints", repeat(callVariadicInts))
	measure("variadic_any", repeat(callVariadicAny))
	measure("box_small_int", repeat(boxSmallInt))
	measure("box_large_int", repeat(boxLargeInt))
	measure("box_bool", repeat(boxBool))
	measure("box_float64", repeat(boxFloat64))
	measure("box_string", repeat(boxString))
	measure("box_pointer", repeat(boxPointer))
	measure("return_any_from_int", repeat(callReturnAnyFromInt))
	measure("return_any_from_large_int", repeat(callReturnAnyFromLargeInt))
	measure("return_any_from_pointer", repeat(callReturnAnyFromPointer))
	measure("sync_pool_round_trip", repeat(poolRoundTrip))
	measure("sprintf_in_loop", sprintfInLoop)
	measure("variadic_ints_in_loop", variadicIntsInLoop)
}
