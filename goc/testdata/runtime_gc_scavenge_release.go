// Scavenging: returning free pages to the operating system.
//
// The page allocator keeps freed pages mapped and remembers which of them are
// still backed by physical memory. The background scavenger walks the highest
// addresses first and madvises runs of free pages away; debug.FreeOSMemory
// forces the whole heap through the same path synchronously. Both end in
// sysUnused on a range the allocator must be able to hand out again -- the page
// has to fault back in, zeroed, and the allocator's scavenged bitmap has to
// agree with what was released, or a later allocation gets a page it thinks is
// already backed.
//
// The shape here is a large heap that is dropped all at once, which is the case
// that actually produces work: a heap that shrinks gradually is scavenged in
// slivers, and a heap that never shrinks is not scavenged at all. After the
// release the program allocates the same amount again, from the pages that were
// just handed back, and checks that every byte it reads is what it wrote --
// which is what a mismatched scavenged bitmap would break.
//
// HeapReleased is asserted to grow rather than to reach a particular value:
// what the kernel does with the madvise is its business, but the runtime's own
// accounting of what it released is not.
package main

import (
	"runtime"
	"runtime/debug"
)

type block struct {
	value  int
	filler [1024]uintptr
	next   *block
}

const blockCount = 2048

//go:noinline
func buildHeap(seed int) []*block {
	out := make([]*block, 0, blockCount)
	for index := 0; index < blockCount; index++ {
		node := &block{value: seed + index}
		node.filler[0] = uintptr(seed + index)
		node.filler[1023] = uintptr((seed + index) * 3)
		if len(out) > 0 {
			node.next = out[len(out)-1]
		}
		out = append(out, node)
	}
	return out
}

//go:noinline
func verifyHeap(heap []*block, seed int) {
	if len(heap) != blockCount {
		println("heap length", len(heap))
		panic("the heap lost blocks")
	}
	for index, node := range heap {
		if node.value != seed+index {
			println("block", index, "value", node.value)
			panic("a block was corrupted across a scavenge")
		}
		if node.filler[0] != uintptr(seed+index) || node.filler[1023] != uintptr((seed+index)*3) {
			println("block", index, "filler corrupted")
			panic("a block's payload was corrupted across a scavenge")
		}
	}
	for index := blockCount - 1; index > 0; index-- {
		if heap[index].next != heap[index-1] {
			println("block", index, "lost its link")
			panic("a block's link was corrupted across a scavenge")
		}
	}
}

//go:noinline
func released() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapReleased
}

//go:noinline
func heapIdle() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapIdle
}

func main() {
	previous := debug.SetGCPercent(100)
	defer debug.SetGCPercent(previous)

	first := buildHeap(1)
	verifyHeap(first, 1)

	before := released()

	// Drop the whole heap at once and force it back to the operating system.
	first = nil
	runtime.GC()
	debug.FreeOSMemory()

	afterRelease := released()
	if afterRelease <= before {
		println("released before", int64(before), "after", int64(afterRelease))
		panic("dropping and releasing a large heap released nothing")
	}
	if idle := heapIdle(); afterRelease > idle {
		println("released", int64(afterRelease), "idle", int64(idle))
		panic("more heap is reported released than is idle")
	}

	// Take the same amount back. These pages are the ones just released, so a
	// scavenged bitmap that disagrees with what was madvised away shows up as
	// corruption here rather than as a statistic.
	second := buildHeap(1000)
	verifyHeap(second, 1000)

	runtime.GC()
	verifyHeap(second, 1000)

	// A second release cycle, this time with the heap still live, so the
	// scavenger has to find free pages among used ones rather than a single
	// contiguous run.
	third := buildHeap(2000)
	second = nil
	runtime.GC()
	debug.FreeOSMemory()
	verifyHeap(third, 2000)

	println("scavenge release ok")
}
