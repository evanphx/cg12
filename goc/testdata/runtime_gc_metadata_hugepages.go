// The GC metadata huge-page transition.
//
// gcMarkTermination ends with:
//
//	if gcController.heapGoal() > minHeapForMetadataHugePages {
//	    systemstack(func() { mheap_.enableMetadataHugePages() })
//	}
//
// minHeapForMetadataHugePages is 1 GiB. Below it nothing happens; above it the
// page allocator's chunk bitmaps and the heap arena L2 tables are madvised
// MADV_HUGEPAGE, once, and every chunk mapped afterwards inherits the setting.
// The transition walks the whole mapped address space under the heap lock, so it
// is a real operation on live metadata rather than a flag flip, and it runs
// exactly once in a program's life.
//
// Crossing the threshold does not require a gigabyte of live data: the heap goal
// is live bytes times (1 + GOGC/100), so a modest live heap and a large GOGC
// reach it while the process stays small. That is what this program does, and it
// asserts the precondition it is responsible for -- that the heap goal really did
// cross the threshold -- with runtime/metrics rather than assuming it.
//
// Whether the kernel then backs the metadata with huge pages is a property of the
// host (transparent huge pages may be off, and madvise is advisory either way),
// so it is not asserted. That the transition executed is asserted separately by
// TestMetadataHugePageTransitionIsReached against the runtime coverage bitmap.
//
// What this program checks itself is that the heap keeps working across the
// transition: metadata is remapped underneath a live heap, so every object
// allocated before it must still be intact after, and allocation must continue
// to work on both sides.
package main

import (
	"runtime"
	"runtime/debug"
	"runtime/metrics"
)

type record struct {
	value  int
	label  string
	filler [64]uintptr
	next   *record
}

const liveRecords = 24576

//go:noinline
func buildLive(seed int) []*record {
	out := make([]*record, 0, liveRecords)
	var previous *record
	for index := 0; index < liveRecords; index++ {
		node := &record{
			value: seed + index,
			label: "record-" + string(rune('a'+index%26)),
			next:  previous,
		}
		node.filler[0] = uintptr(seed + index)
		node.filler[63] = uintptr((seed + index) * 7)
		out = append(out, node)
		previous = node
	}
	return out
}

//go:noinline
func verifyLive(live []*record, seed int) {
	if len(live) != liveRecords {
		println("live length", len(live))
		panic("the live set lost records across the metadata transition")
	}
	for index, node := range live {
		want := "record-" + string(rune('a'+index%26))
		if node.value != seed+index || node.label != want {
			println("record", index, "value", node.value)
			panic("a record was corrupted across the metadata transition")
		}
		if node.filler[0] != uintptr(seed+index) || node.filler[63] != uintptr((seed+index)*7) {
			println("record", index, "filler corrupted")
			panic("a record's payload was corrupted across the metadata transition")
		}
	}
}

//go:noinline
func heapGoal() uint64 {
	sample := []metrics.Sample{{Name: "/gc/heap/goal:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		panic("the heap goal metric is not a uint64")
	}
	return sample[0].Value.Uint64()
}

func main() {
	const threshold = 1 << 30

	live := buildLive(1)
	verifyLive(live, 1)

	// Push the heap goal past the threshold without pushing the live heap
	// anywhere near it.
	previous := debug.SetGCPercent(14000)
	defer debug.SetGCPercent(previous)

	runtime.GC()
	if goal := heapGoal(); goal <= threshold {
		println("heap goal", int64(goal), "threshold", int64(threshold))
		panic("the heap goal did not cross the metadata huge-page threshold")
	}

	// A second cycle so that the transition, which runs at the end of a
	// mark termination whose goal is already above the threshold, has run
	// before anything below is checked.
	runtime.GC()
	verifyLive(live, 1)

	// Allocate on the far side of the transition: new chunks are the ones that
	// inherit the huge-page setting, and their bitmaps are what the allocator
	// reads to find free pages.
	more := buildLive(1000000)
	verifyLive(more, 1000000)
	verifyLive(live, 1)

	runtime.GC()
	verifyLive(live, 1)
	verifyLive(more, 1000000)

	if goal := heapGoal(); goal <= threshold {
		println("heap goal fell back to", int64(goal))
		panic("the heap goal fell back below the threshold")
	}

	println("metadata hugepages ok")
}
