// Low-memory pressure: a soft memory limit the workload keeps bumping into.
//
// debug.SetMemoryLimit makes the pacer target total runtime-managed memory
// rather than a multiple of the live heap. Under a limit the heap goal is
// computed backwards from what is left after stacks, spans and off-heap
// metadata, the scavenger is told to give pages back rather than keep them, and
// the CPU limiter starts throttling if the collector's share climbs too far. A
// workload whose live set is a large fraction of the limit therefore runs cycles
// back to back, with assists, scavenging and marking all overlapping -- which is
// the interaction this exercises and no comfortable heap does.
//
// The rule the program checks is the one the limit is supposed to give: a soft
// limit is not a hard cap and may be exceeded, but a program that stays under it
// in live bytes must not be killed, must keep making progress, and must not lose
// data. So it holds a live set sized against the limit, churns garbage on top of
// it, and verifies the live set after every phase.
//
// It also checks that the limit did something. Without a limit this workload
// runs a handful of collections; under one it runs many more, so the cycle count
// under the limit must exceed the count without it. Otherwise the program would
// pass just as well on a runtime that ignored SetMemoryLimit entirely.
package main

import (
	"runtime"
	"runtime/debug"
)

type tenant struct {
	value  int
	label  string
	filler [24]uintptr
	next   *tenant
}

const (
	tenantCount = 20000
	churnRounds = 40
	churnSize   = 8192
)

//go:noinline
func buildTenants() []*tenant {
	out := make([]*tenant, 0, tenantCount)
	var previous *tenant
	for index := 0; index < tenantCount; index++ {
		node := &tenant{
			value: index,
			label: "tenant-" + string(rune('a'+index%26)),
			next:  previous,
		}
		node.filler[0] = uintptr(index)
		node.filler[23] = uintptr(index * 5)
		out = append(out, node)
		previous = node
	}
	return out
}

//go:noinline
func verifyTenants(live []*tenant) {
	if len(live) != tenantCount {
		println("tenants", len(live))
		panic("the live set lost tenants under the memory limit")
	}
	for index, node := range live {
		want := "tenant-" + string(rune('a'+index%26))
		if node.value != index || node.label != want {
			println("tenant", index, "reads", node.value)
			panic("a tenant was corrupted under the memory limit")
		}
		if node.filler[0] != uintptr(index) || node.filler[23] != uintptr(index*5) {
			println("tenant", index, "filler corrupted")
			panic("a tenant's payload was corrupted under the memory limit")
		}
	}
}

//go:noinline
func churn(rounds int) int {
	total := 0
	for round := 0; round < rounds; round++ {
		garbage := make([]*tenant, 0, churnSize)
		for index := 0; index < churnSize; index++ {
			node := &tenant{value: index, label: "garbage"}
			node.filler[0] = uintptr(index)
			garbage = append(garbage, node)
		}
		if len(garbage) != churnSize {
			panic("the churn allocation was truncated")
		}
		total += int(garbage[churnSize-1].filler[0])
	}
	return total
}

//go:noinline
func cycles() uint32 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.NumGC
}

func main() {
	// A comfortable configuration first, to establish how many cycles this
	// workload costs when nothing is squeezing it.
	previousPercent := debug.SetGCPercent(400)
	previousLimit := debug.SetMemoryLimit(-1)

	live := buildTenants()
	verifyTenants(live)
	runtime.GC()

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	liveBytes := stats.HeapAlloc

	before := cycles()
	churn(churnRounds)
	comfortable := cycles() - before
	verifyTenants(live)

	// Now a limit only a little above the live set. GOGC off, so the limit is
	// the only thing pacing the collector.
	limit := int64(liveBytes) * 3
	debug.SetGCPercent(-1)
	debug.SetMemoryLimit(limit)

	before = cycles()
	churn(churnRounds)
	constrained := cycles() - before
	verifyTenants(live)

	runtime.ReadMemStats(&stats)
	if stats.HeapAlloc > uint64(limit) {
		println("heap alloc", int64(stats.HeapAlloc), "limit", limit)
		panic("the live heap ended above the memory limit")
	}

	// Restore before the final checks so that a failure below is not itself
	// running under the constrained configuration.
	debug.SetMemoryLimit(previousLimit)
	debug.SetGCPercent(previousPercent)

	if constrained <= comfortable {
		println("comfortable cycles", int64(comfortable), "constrained cycles", int64(constrained))
		panic("the memory limit did not change how often the collector ran")
	}

	runtime.GC()
	verifyTenants(live)
	println("memory limit ok")
}
