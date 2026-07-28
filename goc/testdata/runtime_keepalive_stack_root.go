// Reducer for the "found bad pointer in Go heap" failure described in
// RUNTIME_PLAN.md 5.8.
//
// cg12 used to give runtime.KeepAlive a process-global pointer word: every
// assignment to a kept-alive variable stored the value into a .goc.keepalive.N
// data symbol declared with one pointer word, and the KeepAlive call cleared
// it. Escape analysis leaves root on this goroutine's stack, so that store
// published a goroutine stack address to a permanent GC root. Nothing
// relocates a global when a stack is copied and nothing clears it when a
// goroutine exits, so the word went stale as soon as the stack moved, and a
// later collection found a pointer into a span the stack allocator had already
// released.
//
// Each ingredient earns its place:
//
//   - the nested composite literal keeps root off the heap, which is what makes
//     the published address a stack address;
//   - growStack forces a stack copy while the global still names the old frame,
//     which is what makes the global's word stale;
//   - the first runtime.GC returns the freed stack span from the stack pool to
//     the heap, where its state becomes mSpanDead; the second one scans the
//     stale global and faults on it. With one collection the span is usually
//     still mSpanManual, which findObject accepts;
//   - the goroutines make several stacks move per collection.
//
// The program raises GOMAXPROCS and lowers GOGC itself rather than relying on
// the capability matrix's -runtime-procs, which only goes up to 4, so that it
// reproduces at every matrix setting. Reproduction is probabilistic but high:
// before the fix roughly 32 runs in 100 optimized and 27 in 100 unoptimized, at
// -runtime-procs=1 and 4 alike; after the fix none in several thousand.
//
// The deterministic guard for the same defect is
// TestKeepAliveStoresIntoAFrameSlotRatherThanAGlobal in goc/compile_test.go,
// and GODEBUG=cg12checkwb=2 turns the invariant violation itself into a throw
// at the store that commits it.
package main

import (
	"runtime"
	"runtime/debug"
)

type keptItem struct {
	index int
	next  *keptItem
}

//go:noinline
func growStack(depth int) int {
	if depth == 0 {
		return 0
	}
	var padding [256]byte
	padding[0] = byte(depth)
	return int(padding[0]) - int(padding[0]) + growStack(depth-1)
}

func keepStackItem(index int, results chan int) {
	root := &keptItem{
		index: index,
		next:  &keptItem{index: index + 1},
	}
	grown := growStack(32)
	runtime.GC()
	runtime.GC()
	results <- root.index + root.next.index + grown
	runtime.KeepAlive(root)
}

func main() {
	runtime.GOMAXPROCS(4)
	debug.SetGCPercent(10)

	const goroutines = 24

	results := make(chan int, goroutines)
	for index := 0; index < goroutines; index++ {
		go keepStackItem(index, results)
	}

	total := 0
	for index := 0; index < goroutines; index++ {
		total += <-results
	}

	if total != goroutines*goroutines {
		panic("keepalive stack root total mismatch")
	}
}
