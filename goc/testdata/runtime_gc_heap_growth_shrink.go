// Heap growth and shrink cycles, and the pacer's response to both.
//
// A heap that only grows exercises one direction of the page allocator: new
// arenas, new chunks, spans carved out of fresh memory. A heap that shrinks
// exercises the other: spans returned to the page allocator, coalesced with
// their neighbours, and handed back out at a different size class. The pacer
// recomputes its goal from live bytes at the end of every cycle, so a heap that
// oscillates makes it grow the goal and then cut it, which is when a
// disagreement between the allocator's idea of what is free and the sweeper's
// shows up.
//
// This program runs several growth and shrink cycles at a deliberately low GOGC
// so the pacer reacts to each one, holds a permanent live set across all of
// them, and checks that set after every phase. It also checks the accounting
// invariants that a growth/shrink disagreement would break: HeapObjects tracking
// Mallocs minus Frees, HeapAlloc never exceeding HeapSys, HeapInuse plus
// HeapIdle equal to HeapSys, and the live set's own size tracking what was
// allocated.
//
// The allocation sizes cross a span boundary on purpose: the small objects share
// spans and the large ones get their own, so shrinking frees whole spans in one
// case and leaves partially-used spans in the other.
package main

import (
	"runtime"
	"runtime/debug"
)

type resident struct {
	value int
	label string
	next  *resident
}

type bulk struct {
	value  int
	filler [4096]uintptr
}

const (
	permanentCount = 8192
	growthRounds   = 5
)

//go:noinline
func buildPermanent() []*resident {
	out := make([]*resident, 0, permanentCount)
	var previous *resident
	for index := 0; index < permanentCount; index++ {
		node := &resident{
			value: index,
			label: "resident-" + string(rune('a'+index%26)),
			next:  previous,
		}
		out = append(out, node)
		previous = node
	}
	return out
}

//go:noinline
func verifyPermanent(live []*resident) {
	if len(live) != permanentCount {
		println("permanent length", len(live))
		panic("the permanent live set lost entries")
	}
	for index, node := range live {
		want := "resident-" + string(rune('a'+index%26))
		if node.value != index || node.label != want {
			println("resident", index, "reads", node.value)
			panic("the permanent live set was corrupted by a growth or shrink cycle")
		}
	}
	for index := permanentCount - 1; index > 0; index-- {
		if live[index].next != live[index-1] {
			println("resident", index, "lost its link")
			panic("the permanent live set's links were corrupted")
		}
	}
}

// grow allocates a transient heap several times the size of the permanent one
// and then drops it, so the next collection has a large amount to reclaim.
//
//go:noinline
func grow(round int) int {
	transient := make([]*bulk, 0, 512)
	for index := 0; index < 512; index++ {
		node := &bulk{value: round*512 + index}
		node.filler[0] = uintptr(node.value)
		node.filler[4095] = uintptr(node.value * 3)
		transient = append(transient, node)
	}
	total := 0
	for index, node := range transient {
		if node.value != round*512+index {
			panic("a transient object was corrupted while it was live")
		}
		if node.filler[4095] != uintptr(node.value*3) {
			panic("a transient object's payload was corrupted while it was live")
		}
		total += node.value & 1
	}
	small := make([]*resident, 0, 65536)
	for index := 0; index < 65536; index++ {
		small = append(small, &resident{value: index, label: "transient"})
	}
	if len(small) != 65536 {
		panic("the transient small allocation was truncated")
	}
	return total
}

//go:noinline
func checkAccounting(where string) uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if stats.HeapObjects != stats.Mallocs-stats.Frees {
		println(where, "objects", int64(stats.HeapObjects), "mallocs-frees", int64(stats.Mallocs-stats.Frees))
		panic("HeapObjects disagrees with Mallocs minus Frees")
	}
	if stats.HeapAlloc > stats.HeapSys {
		println(where, "alloc", int64(stats.HeapAlloc), "sys", int64(stats.HeapSys))
		panic("more heap is allocated than has been obtained from the OS")
	}
	if stats.HeapInuse+stats.HeapIdle != stats.HeapSys {
		println(where, "inuse", int64(stats.HeapInuse), "idle", int64(stats.HeapIdle), "sys", int64(stats.HeapSys))
		panic("HeapInuse plus HeapIdle does not equal HeapSys")
	}
	if stats.HeapReleased > stats.HeapIdle {
		println(where, "released", int64(stats.HeapReleased), "idle", int64(stats.HeapIdle))
		panic("more heap is released than is idle")
	}
	return stats.HeapAlloc
}

func main() {
	previous := debug.SetGCPercent(50)
	defer debug.SetGCPercent(previous)

	permanent := buildPermanent()
	verifyPermanent(permanent)

	runtime.GC()
	baseline := checkAccounting("baseline")

	peak := baseline
	for round := 0; round < growthRounds; round++ {
		grow(round)
		if grown := checkAccounting("grown"); grown > peak {
			peak = grown
		}
		verifyPermanent(permanent)

		runtime.GC()
		shrunk := checkAccounting("shrunk")
		verifyPermanent(permanent)

		if shrunk > peak {
			println("round", round, "shrunk to", int64(shrunk), "peak", int64(peak))
			panic("the heap did not shrink after the transient set was dropped")
		}
	}

	if peak <= baseline {
		println("peak", int64(peak), "baseline", int64(baseline))
		panic("the heap never grew, so this program measured nothing")
	}

	runtime.GC()
	runtime.GC()
	final := checkAccounting("final")
	verifyPermanent(permanent)

	if final > peak {
		println("final", int64(final), "peak", int64(peak))
		panic("the heap ended larger than its peak")
	}

	println("heap growth shrink ok")
}
