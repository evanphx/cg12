// Reducer for the "found pointer to free object" / "found bad pointer in Go
// heap" failures described in RUNTIME_PLAN.md 5.11.
//
// cg12 starts every goroutine through a generated closure wrapper, and gave that
// wrapper an 8-byte argument frame holding the closure context register's home
// slot. Only the stack-growth prologue ever writes that slot, so cg12 marked it
// a pointer in the argument stack map at index 0 -- the map the runtime selects
// while a function is in its prologue.
//
// Index 0 is also the map runtime.stkframe.getStackMap hardcodes when a frame's
// pc is exactly the function entry, and that is the state of every goroutine
// runtime.newproc has created but not yet scheduled: sched.pc is the wrapper's
// entry and sched.sp is stack.hi-48, of which newproc1 initialises only the two
// words below sp. The home slot at sp+8 therefore still holds whatever the
// previous user of that recycled stack left in it, and the collector scanned it
// as a root. Depending on what the stale word happened to address, the run died
// with "found bad pointer in Go heap" (a released stack or an unallocated span),
// with "found pointer to free object" one or two collections later (the mark bit
// set on a free heap object, which the sweeper reports as a zombie), or produced
// a wrong answer with no fault at all (the zombie raises the span's allocCount,
// so an object is handed out twice).
//
// Each ingredient earns its place:
//
//   - seasonWorker fills a goroutine stack with pointer-shaped words and then
//     returns, so when that stack is recycled the stale word is a plausible
//     pointer rather than zero. Without it most stale words are zero and
//     findObject ignores them;
//   - the carry burst is created and then left unscheduled -- GOMAXPROCS is 2
//     against 64 new goroutines -- which is what puts goroutines at pc == entry
//     while a collection scans them;
//   - the two runtime.GC calls run while the burst is still queued. One
//     collection marks the stale referent; the second, and the sweep between
//     them, is what turns a marked free object into a reported zombie;
//   - carry allocates, so the heap keeps churning and free objects exist for a
//     stale pointer to land on.
//
// The program sets GOMAXPROCS and GOGC itself rather than relying on the
// capability matrix's -runtime-procs, so that it reproduces at every matrix
// setting. Before the fix roughly 92 runs in 100 fail at -O; after it, none in
// several thousand.
//
// The deterministic guard for the same defect is
// TestGoEntryArgumentMapOmitsTheClosureHomeSlot in arm64/unit_test.go.
package main

import (
	"runtime"
	"runtime/debug"
	"sync"
)

type link struct {
	index int
	next  *link
}

// season leaves pointer-shaped words the length of a grown stack, then returns
// so the goroutine exits and the stack goes back to the allocator with those
// words still in it.
//
//go:noinline
func season(depth int, head *link) *link {
	if depth == 0 {
		return head
	}
	var chain [8]*link
	for index := range chain {
		chain[index] = &link{index: depth + index, next: head}
	}
	return season(depth-1, chain[len(chain)-1])
}

func seasonWorker(wait *sync.WaitGroup) {
	defer wait.Done()

	tail := season(24, nil)
	if tail.index < 0 {
		panic("season produced a negative index")
	}
}

func carry(index int, wait *sync.WaitGroup, results chan int) {
	defer wait.Done()

	root := &link{
		index: index,
		next:  &link{index: index + 1},
	}
	results <- root.index + root.next.index
}

func main() {
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(10)

	const goroutines = 64
	const rounds = 60

	for round := 0; round < rounds; round++ {
		var seasoning sync.WaitGroup
		for index := 0; index < goroutines; index++ {
			seasoning.Add(1)
			go seasonWorker(&seasoning)
		}
		seasoning.Wait()

		var wait sync.WaitGroup
		results := make(chan int, goroutines)
		for index := 0; index < goroutines; index++ {
			wait.Add(1)
			go carry(index, &wait, results)
		}
		// The burst above is mostly still unscheduled here, so these collections
		// scan goroutines stopped at their entry pc on recycled stacks.
		runtime.GC()
		runtime.GC()
		wait.Wait()
		close(results)

		total := 0
		for index := 0; index < goroutines; index++ {
			total += <-results
		}
		if total != goroutines*goroutines {
			panic("carry total mismatch")
		}
	}
}
