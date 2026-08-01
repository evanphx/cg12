// Proportional sweeping: the spans a cycle leaves behind are swept by the
// allocations that follow it, not by the collector.
//
// At the end of a mark termination the heap is full of spans that still carry
// the previous cycle's alloc bits. Nothing sweeps them eagerly. Instead
// deductSweepCredit charges every span allocation for a proportional share of
// the outstanding sweep work and calls sweepone until the debt is paid, and
// mcentral.cacheSpan sweeps a span on the path that hands it to an mcache. A
// program that forces a collection and then stops allocating never reaches any
// of it.
//
// So this program collects and then keeps allocating, in span-sized bites,
// across several size classes at once, so the sweeper runs on the allocation
// path rather than in the background. It checks two things a broken sweep would
// break: objects allocated before the collection that are still referenced keep
// their contents, and the allocation counters the sweeper maintains stay
// consistent -- Mallocs never below Frees, HeapObjects matching their
// difference, and the sweeper never reporting more live objects after a cycle
// than were allocated during it.
//
// The size classes are deliberately mixed. Spans are swept per size class, and a
// program that only ever allocates 32-byte objects exercises exactly one
// mcentral.
package main

import (
	"runtime"
	"runtime/debug"
)

type small struct {
	value int
	next  *small
}

type medium struct {
	value  int
	label  string
	buffer [12]uintptr
	next   *medium
}

type large struct {
	value  int
	buffer [512]uintptr
	next   *large
}

//go:noinline
func allocateSmall(count int) []*small {
	out := make([]*small, 0, count)
	for index := 0; index < count; index++ {
		out = append(out, &small{value: index})
	}
	return out
}

//go:noinline
func allocateMedium(count int) []*medium {
	out := make([]*medium, 0, count)
	for index := 0; index < count; index++ {
		node := &medium{value: index, label: "medium-" + string(rune('a'+index%26))}
		node.buffer[0] = uintptr(index)
		node.buffer[11] = uintptr(index * 2)
		out = append(out, node)
	}
	return out
}

//go:noinline
func allocateLarge(count int) []*large {
	out := make([]*large, 0, count)
	for index := 0; index < count; index++ {
		node := &large{value: index}
		node.buffer[0] = uintptr(index)
		node.buffer[511] = uintptr(index * 3)
		out = append(out, node)
	}
	return out
}

//go:noinline
func verify(smalls []*small, mediums []*medium, larges []*large) {
	for index, node := range smalls {
		if node.value != index {
			println("small", index, "is", node.value)
			panic("a retained object did not survive the sweep")
		}
	}
	for index, node := range mediums {
		want := "medium-" + string(rune('a'+index%26))
		if node.value != index || node.label != want || node.buffer[11] != uintptr(index*2) {
			println("medium", index, "is", node.value)
			panic("a retained object did not survive the sweep")
		}
	}
	for index, node := range larges {
		if node.value != index || node.buffer[511] != uintptr(index*3) {
			println("large", index, "is", node.value)
			panic("a retained object did not survive the sweep")
		}
	}
}

// dropAndReallocate is what drives the sweeper: it makes a cycle's worth of
// garbage, collects, and then allocates the same shapes again without forcing
// another collection, so the new allocations are the ones that pay off the
// sweep debt.
//
//go:noinline
func dropAndReallocate(rounds int) {
	for round := 0; round < rounds; round++ {
		garbageSmall := allocateSmall(8192)
		garbageMedium := allocateMedium(2048)
		garbageLarge := allocateLarge(64)
		if len(garbageSmall)+len(garbageMedium)+len(garbageLarge) == 0 {
			panic("allocation produced nothing")
		}
		garbageSmall = nil
		garbageMedium = nil
		garbageLarge = nil

		runtime.GC()

		// No forced collection in this half: every span these allocations need
		// has to be swept on the allocation path.
		freshSmall := allocateSmall(8192)
		freshMedium := allocateMedium(2048)
		freshLarge := allocateLarge(64)
		verify(freshSmall, freshMedium, freshLarge)
	}
}

//go:noinline
func checkCounters(where string) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if stats.Frees > stats.Mallocs {
		println(where, "frees", int64(stats.Frees), "mallocs", int64(stats.Mallocs))
		panic("the sweeper freed more objects than were ever allocated")
	}
	if stats.HeapObjects != stats.Mallocs-stats.Frees {
		println(where, "heap objects", int64(stats.HeapObjects), "mallocs-frees", int64(stats.Mallocs-stats.Frees))
		panic("HeapObjects disagrees with Mallocs minus Frees")
	}
	if stats.HeapAlloc > stats.HeapSys {
		println(where, "heap alloc", int64(stats.HeapAlloc), "heap sys", int64(stats.HeapSys))
		panic("more heap is allocated than has been obtained from the OS")
	}
}

func main() {
	previous := debug.SetGCPercent(40)
	defer debug.SetGCPercent(previous)

	retainedSmall := allocateSmall(4096)
	retainedMedium := allocateMedium(1024)
	retainedLarge := allocateLarge(32)

	checkCounters("before")
	dropAndReallocate(4)
	checkCounters("after allocation")

	verify(retainedSmall, retainedMedium, retainedLarge)

	runtime.GC()
	checkCounters("after collection")
	verify(retainedSmall, retainedMedium, retainedLarge)

	println("sweep pacing ok")
}
