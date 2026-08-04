// Command gcpressbench times allocation and collection: making garbage, keeping
// a live heap while making it, and the write barrier that keeps a mutating live
// heap correct.
//
// It is here because this repository's allocation instruments count allocations
// and say where they land, and none of them says what one costs. A change that
// moves a site from the heap to the frame shows up in the census as a line; what
// it bought shows up here and nowhere else. It is also the workload where goc's
// own allocator and collector, rather than the code goc emits, decide the
// answer -- so a movement here with no census movement points at the runtime.
//
// The cases split the three costs apart on purpose, because a regression in one
// looks nothing like a regression in another:
//
//   - gc/alloc-churn allocates small objects that die immediately, against a
//     nearly empty live heap. It is the allocator's fast path and almost no
//     marking.
//   - gc/live-heap-churn does the same churn while a large tree stays reachable,
//     so every collection has real marking work. The difference between this and
//     the case above is the collector.
//   - gc/pointer-write walks a live tree writing pointers into it, which is the
//     write barrier's cost with the collector mostly idle.
//
// # The case that is not here
//
// There was a fourth: gc/slice-grow, appending without a size hint, which is the
// growth path -- allocate, copy, drop, repeatedly. It is gone because under goc
// it cannot be measured, and that is a result rather than a nuisance.
//
// Growing a 4 MB slice four times a round put the case's one-repetition spread at
// 19%. Rewritten to the same total number of appends over forty ten-times-smaller
// growths, it got *worse*: 52% on the ratio, and 71% on the null -- which is the
// same goc binary against itself, so it is not a comparison artefact. The goc
// binary's own cost for this case moves by tens of percent from one process to
// the next.
//
// That says something about goc's collector pacing on the growth path and nothing
// about any compiler change, and a row whose noise is 52% gets a 157% tolerance,
// which is a row that passes everything. It was dropped rather than carried,
// because the suite's ceiling on per-row noise exists precisely to stop a
// meaningless row from riding along looking green. Anyone who fixes the pacing
// should put this case back; the sizes that were tried are in this comment so the
// next attempt starts somewhere.
//
// Sizes are chosen so the goc-built binary finishes a round in well under a
// second. That is a real constraint rather than an aesthetic one: the suite runs
// each program three times per repetition, ten repetitions deep.
package main

import (
	"fmt"
	"time"
)

// rounds is how many timed rounds each case gets; the fastest is reported.
const rounds = 3

// sink keeps the control loop and every case's result from being optimised away.
var sink uint64

// keep holds the live heap across the timed rounds of the cases that need one,
// so the collector has something to mark that the compiler cannot prove dead.
var keep *treeNode

// control is the fixed amount of integer arithmetic every case is divided by,
// so the machine's speed and its load divide out of the reported index. It is
// the same loop the crypto signing benchmark uses.
func control() {
	accumulator := uint64(1)
	for i := 0; i < 20_000_000; i++ {
		accumulator = accumulator*6364136223846793005 + 1442695040888963407
	}
	sink = accumulator
}

// measure returns the fastest of rounds timed rounds, in nanoseconds. Noise can
// only ever make a round slower, so the fastest is the least contaminated.
func measure(body func()) time.Duration {
	body()
	best := time.Duration(1<<63 - 1)
	for round := 0; round < rounds; round++ {
		start := time.Now()
		body()
		if elapsed := time.Since(start); elapsed < best {
			best = elapsed
		}
	}
	return best
}

func report(name string, body func()) {
	fmt.Printf("%s\t%d\n", name, int64(measure(body)))
}

// payload is a 48-byte object with one pointer in it, so an allocation of one is
// a size class the allocator has a fast path for and an object the collector has
// to scan.
type payload struct {
	next   *payload
	values [5]uint64
}

// treeNode is the live heap's shape: a binary tree, which gives the collector a
// deep pointer graph to walk rather than one flat array it can stride through.
type treeNode struct {
	left, right *treeNode
	value       uint64
}

func buildTree(depth int, value uint64) *treeNode {
	if depth == 0 {
		return &treeNode{value: value}
	}
	return &treeNode{
		left:  buildTree(depth-1, value*2+1),
		right: buildTree(depth-1, value*2+2),
		value: value,
	}
}

func sumTree(node *treeNode) uint64 {
	if node == nil {
		return 0
	}
	return node.value + sumTree(node.left) + sumTree(node.right)
}

const (
	// churnObjects is how many short-lived objects one round allocates.
	churnObjects = 250_000
	// liveDepth is the depth of the tree that stays reachable during the live
	// case: 2^17 nodes, a few megabytes, enough that a collection has to walk a
	// real graph.
	liveDepth = 17
	// writeDepth is the tree the write-barrier case mutates. Smaller, because
	// what is being counted is the barrier per write rather than the marking.
	writeDepth = 15
	// writeRepeats is how many times the write-barrier case walks its tree. One
	// walk is only about 20 microseconds, which is too short a round to time.
	writeRepeats = 100
)

func main() {
	report("control/spin-fixed-work", control)

	// Churn with a nearly empty live heap: allocator fast path, little marking.
	// The chain keeps the most recent objects alive for a moment so that the
	// compiler cannot decide the allocation is dead on arrival.
	report("gc/alloc-churn", func() {
		var head *payload
		for i := 0; i < churnObjects; i++ {
			head = &payload{next: head, values: [5]uint64{uint64(i)}}
			if i&15 == 0 {
				head = nil
			}
		}
		if head != nil {
			sink += head.values[0]
		}
	})

	// The same churn with a large live heap. The difference between this row and
	// the one above is what the collector costs when it has something to mark.
	keep = buildTree(liveDepth, 1)
	report("gc/live-heap-churn", func() {
		var head *payload
		for i := 0; i < churnObjects; i++ {
			head = &payload{next: head, values: [5]uint64{uint64(i)}}
			if i&15 == 0 {
				head = nil
			}
		}
		if head != nil {
			sink += head.values[0]
		}
		sink += sumTree(keep) & 1
	})

	// Pointer writes into a live tree: the write barrier, with the collector
	// otherwise idle. Every iteration stores a heap pointer into a heap object,
	// which is exactly what a barrier has to intercept.
	writeTree := buildTree(writeDepth, 1)
	spare := buildTree(3, 7)
	report("gc/pointer-write", func() {
		for repeat := 0; repeat < writeRepeats; repeat++ {
			rewire(writeTree, spare, 12)
		}
		sink += writeTree.value
	})
}

// rewire walks the tree to the given depth swapping each node's children for the
// spare's and back again, so the net shape is unchanged and the number of
// pointer stores is fixed.
func rewire(node *treeNode, spare *treeNode, depth int) {
	if node == nil || depth == 0 {
		return
	}
	left, right := node.left, node.right
	node.left = spare
	node.right = spare
	node.left = left
	node.right = right
	rewire(left, spare, depth-1)
	rewire(right, spare, depth-1)
}
