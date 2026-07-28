// Heap-to-heap and stack-to-heap pointer write barriers.
//
// RUNTIME_PLAN.md section 6 asks for both shapes. The capability runs under
// GODEBUG=cg12checkwb=2, which validates the slot's previous contents and the
// stored value at every buffered barrier and throws at the store rather than at
// the flush, so a bad word names the writer.
//
// A barrier only runs while the collector's mark phase is on, so this program
// keeps a collection in flight: the GC percentage is set to 1 and a helper
// goroutine forces collections while the stores happen. Without that the stores
// take the barrier-disabled path and the diagnostic sees nothing.
//
// The semantic check is deletion-barrier shaped. Each slot is overwritten many
// times, and every value ever stored into it is also recorded in a list that
// keeps it reachable, so the collector must not free any of them; the values
// are read back at the end. A store that skipped its barrier while the slot's
// previous value was the only reference would show up as a freed and reused
// object, which the recorded values would then no longer match.
package main

import (
	"runtime"
	"runtime/debug"
)

type barrierNode struct {
	tag  int64
	next *barrierNode
	side *barrierNode
}

const rounds = 512
const chainLength = 24

// heapRoots holds the objects whose fields receive the heap-to-heap stores.
var heapRoots []*barrierNode

// witnesses keeps every value that was ever stored reachable, so nothing the
// stores overwrite may be collected.
var witnesses []*barrierNode

func collectInBackground(done chan struct{}) {
	for {
		select {
		case <-done:
			close(done)
			return
		default:
			runtime.GC()
		}
	}
}

// storeHeapToHeap writes a heap pointer into a field of another heap object.
// Both the slot and the value are in the heap, which is the ordinary barrier
// case.
func storeHeapToHeap(root *barrierNode, tag int64) {
	for step := 0; step < chainLength; step++ {
		value := &barrierNode{tag: tag*int64(chainLength) + int64(step)}
		witnesses = append(witnesses, value)
		root.next = value
		root.side = value
	}
}

// storeStackToHeap writes a pointer whose referent the caller holds on its
// stack into a heap object's field. The value is only reachable through the
// frame until the store commits, so the store is the moment the collector has
// to learn about it.
func storeStackToHeap(root *barrierNode, tag int64) {
	local := &barrierNode{tag: tag}
	witnesses = append(witnesses, local)
	root.next = local
	runtime.KeepAlive(local)
}

func main() {
	debug.SetGCPercent(1)
	done := make(chan struct{})
	go collectInBackground(done)

	for round := 0; round < rounds; round++ {
		root := &barrierNode{tag: int64(round)}
		heapRoots = append(heapRoots, root)
		storeHeapToHeap(root, int64(round))
		storeStackToHeap(root, int64(round)*1000000)
	}

	done <- struct{}{}
	<-done

	runtime.GC()
	runtime.GC()

	if len(heapRoots) != rounds {
		panic("the heap root list has the wrong length")
	}
	for index, root := range heapRoots {
		if root.tag != int64(index) {
			panic("a heap root was disturbed")
		}
		if root.next == nil || root.side == nil {
			panic("a barriered field was cleared")
		}
		if root.next.tag != int64(index)*1000000 {
			panic("the last stack-to-heap store did not survive")
		}
		if root.side.tag != int64(index)*int64(chainLength)+int64(chainLength-1) {
			panic("the last heap-to-heap store did not survive")
		}
	}
	expectedWitnesses := rounds * (chainLength + 1)
	if len(witnesses) != expectedWitnesses {
		panic("the witness list has the wrong length")
	}
	for index, witness := range witnesses {
		if witness == nil {
			panic("a witnessed value became nil")
		}
		round := int64(index / (chainLength + 1))
		step := index % (chainLength + 1)
		var expected int64
		if step == chainLength {
			expected = round * 1000000
		} else {
			expected = round*int64(chainLength) + int64(step)
		}
		if witness.tag != expected {
			panic("a value that a barriered store overwrote was freed and reused")
		}
	}
}
