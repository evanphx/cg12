// Reproducer for the "sweep increased allocation count" failure described in
// RUNTIME_PLAN.md 5.7.
//
// runtime.heapBitsSlice builds a notInHeapSlice whose data word points at a
// span's own heap-bits region, past the span's last object. cg12 used to route
// that store through its dynamic write barrier, so mark termination pulled the
// address out of the write barrier buffer and set a mark bit for object index
// nelems. The following sweep then counted more marked objects than the span's
// allocCount and threw.
//
// The failure needs a span of a scan size class to be grown while the mark
// phase is running, on a thread whose g0 stack is the process stack rather than
// a runtime-managed stack span -- that is, from newm/allocm/malg on the initial
// thread. That combination only appears with enough Ps to keep creating Ms, so
// this program raises GOMAXPROCS itself instead of relying on the capability
// matrix's -runtime-procs, which only goes up to 4. The workload is the
// gc_struct.go shape: allocate hard enough to grow spans, collect, then check
// that a live object survived. Reproduction is high but not certain (roughly
// three runs in four before the fix); the deterministic guard for the same bug
// is TestNotInHeapPointerStoreSkipsWriteBarrier in goc/escape_test.go.
package main

import "runtime"

type spanRecord struct {
	id     int
	weight int
	left   *spanRecord
	right  *spanRecord
}

var spanGarbage *spanRecord

func allocateSpanGarbage() {
	for i := 0; i < 100000; i++ {
		spanGarbage = &spanRecord{
			id:     i,
			weight: i * 3,
		}
	}
	spanGarbage = nil
}

func main() {
	runtime.GOMAXPROCS(48)

	root := new(spanRecord)
	root.id = 7
	root.weight = 11
	root.left = &spanRecord{id: 13, weight: 17}
	root.right = &spanRecord{id: 19, weight: 23}

	allocateSpanGarbage()
	runtime.GC()

	result := root.id * root.weight
	result += root.left.id * root.left.weight
	result += root.right.id * root.right.weight
	runtime.KeepAlive(root)
	if result != 735 {
		panic("garbage collector corrupted a live object")
	}
}
