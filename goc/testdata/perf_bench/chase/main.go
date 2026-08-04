// Command chasebench times dependent loads: work whose speed is decided by
// memory latency and by how few instructions the compiler puts between one load
// and the load that depends on it.
//
// It is here because nothing else in the suite is bound by memory. Every other
// workload is bound by how many instructions get executed; this one is bound by
// how long each one waits. A code generator can be excellent at the first and
// bad at the second -- an extra bounds check on the critical path costs almost
// nothing when the data is in L1 and costs a whole cache miss of pipelining when
// it is not.
//
// Three cases, because they answer different questions:
//
//   - chase/l1-resident walks a 128 KiB ring, which fits in this core's private
//     cache. Every load hits, so the case is a pure measure of the instructions
//     the compiler puts on the dependency chain.
//   - chase/dram walks a 64 MiB ring, which fits in nothing on this box. Every
//     load is a miss and the chain is stalled on the memory system. A compiler
//     cannot make this much faster or slower, so the goc/host ratio here should
//     be close to 1.00 -- and that is what makes the row useful: a row that is
//     *supposed* to read 1.00 says whether the instrument is telling the truth.
//   - chase/pointer-node walks a 16 MiB chain of real pointers instead of
//     indices, which is the same access pattern with a dereference in place of
//     a bounds-checked index.
//
// The ring is a full-period linear congruential permutation: ring[i] is
// (i*a + c) mod n with n a power of two, a = 1 mod 4 and c odd, which are the
// Hull-Dobell conditions and make the walk one cycle through every element. So
// it cannot terminate early, and because the multiplier scatters the high bits
// -- the ones that pick the cache line -- no stride prefetcher tracks it. It is
// built with one multiply-add per element rather than by shuffling, because the
// setup runs under goc too and a shuffle of 16 Mi elements cost more than every
// timed round in this program put together.
//
// The index cases use uint32 rather than pointers on purpose: they are about
// load latency, not about the garbage collector, and a 64 MiB linked structure
// of real pointers would be measuring the collector's scan instead.
package main

import (
	"fmt"
	"time"
)

// rounds is how many timed rounds each case gets; the fastest is reported.
const rounds = 3

// sink keeps the control loop and the chases from being optimised away.
var sink uint64

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

const (
	// multiplier and increment are the same constants the control loop uses.
	// multiplier is 1 mod 4 and increment is odd, which is what makes the
	// permutation below a single cycle.
	multiplier = 6364136223846793005
	increment  = 1442695040888963407
)

// singleCycle returns a permutation of [0, n) that is one cycle of length n. n
// must be a power of two.
func singleCycle(n int) []uint32 {
	mask := uint64(n) - 1
	ring := make([]uint32, n)
	for i := range ring {
		ring[i] = uint32((uint64(i)*multiplier + increment) & mask)
	}
	return ring
}

// node is the pointer form of the same walk. It is 32 bytes so that two
// successive nodes never share a cache line and the walk is one miss per step.
type node struct {
	next    *node
	payload [3]uint64
}

// pointerRing links n nodes into one cycle in the order singleCycle gives, so
// the pointer walk and the index walk have the same access pattern and differ
// only in what the compiler has to emit to follow it.
func pointerRing(n int) *node {
	order := singleCycle(n)
	nodes := make([]node, n)
	for i := 0; i < n; i++ {
		nodes[i].next = &nodes[order[i]]
	}
	return &nodes[0]
}

const (
	// l1Elements is 32 Ki uint32s, 128 KiB: inside this core's private cache.
	l1Elements = 32 * 1024
	// dramElements is 16 Mi uint32s, 64 MiB: past the shared last level on this
	// box, so every step is a miss.
	dramElements = 16 * 1024 * 1024
	// pointerNodes is 512 Ki 32-byte nodes, 16 MiB: past this core's private
	// cache, at a size whose setup is affordable under goc.
	pointerNodes = 512 * 1024

	// steps is how many dependent loads one timed round of an index case makes.
	// The same count for both, so the two rows differ only in where the data is.
	steps = 1024 * 1024
	// pointerSteps was a quarter of steps and is now equal to it, because at a
	// quarter this row had the shortest timed region in the program and was the
	// noisiest with it. The row is compared against its own baseline, not against
	// the index rows, so the count is free to change.
	pointerSteps = 1024 * 1024
)

func walk(ring []uint32, count int) uint32 {
	position := uint32(0)
	for i := 0; i < count; i++ {
		position = ring[position]
	}
	return position
}

func main() {
	report("control/spin-fixed-work", control)

	l1Ring := singleCycle(l1Elements)
	report("chase/l1-resident", func() {
		sink += uint64(walk(l1Ring, steps))
	})

	dramRing := singleCycle(dramElements)
	report("chase/dram", func() {
		sink += uint64(walk(dramRing, steps))
	})

	head := pointerRing(pointerNodes)
	report("chase/pointer-node", func() {
		current := head
		for i := 0; i < pointerSteps; i++ {
			current = current.next
		}
		sink += current.payload[0]
	})
}
