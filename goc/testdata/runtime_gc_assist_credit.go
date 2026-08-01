// Mark assists: the allocator's back-pressure valve.
//
// A goroutine that allocates while a mark phase is running goes into assist
// debt proportional to what it allocated, and gcAssistAlloc makes it do that
// much marking before the allocation completes. If it cannot -- because the
// work queue is empty but the phase has not finished -- it parks on the assist
// queue until a background worker flushes credit to it. That is the path where
// an allocation can recurse into the collector, which is the shape of the defect
// RUNTIME_PLAN.md section 5.2.1 records, and it only happens under enough
// allocation pressure that the pacer falls behind.
//
// Making assists happen takes three things at once: a low GOGC so cycles start
// early, a live heap large enough that marking takes real time, and several
// goroutines allocating pointer-dense objects while it does. This program does
// all three with a deterministic amount of work, then asserts that assist CPU
// was actually charged -- an assist that never ran would leave the metric at
// zero and the program would otherwise look identical.
//
// It also checks that the assisting goroutines' own live data came through: an
// assist runs the mark loop on a mutator's stack, so a bug there corrupts the
// goroutine that was merely trying to allocate.
package main

import (
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
)

type payload struct {
	value  int
	label  string
	linked *payload
}

const (
	assistWorkers = 8
	assistRounds  = 400
	chainLength   = 24
)

//go:noinline
func chain(seed int) *payload {
	var head *payload
	for index := 0; index < chainLength; index++ {
		head = &payload{
			value:  seed + index,
			label:  "assist-" + string(rune('a'+index%26)),
			linked: head,
		}
	}
	return head
}

//go:noinline
func verifyChain(head *payload, seed int) {
	for index := chainLength - 1; index >= 0; index-- {
		if head == nil {
			panic("an assisting goroutine's chain was truncated")
		}
		if head.value != seed+index || head.label != "assist-"+string(rune('a'+index%26)) {
			println("chain node", index, "value", head.value, "want", seed+index)
			panic("an assisting goroutine's chain was corrupted")
		}
		head = head.linked
	}
	if head != nil {
		panic("an assisting goroutine's chain grew a tail")
	}
}

//go:noinline
func assistCPU() float64 {
	sample := []metrics.Sample{{Name: "/cpu/classes/gc/mark/assist:cpu-seconds"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindFloat64 {
		panic("the assist CPU metric is not a float64")
	}
	return sample[0].Value.Float64()
}

func main() {
	previous := debug.SetGCPercent(10)
	defer debug.SetGCPercent(previous)

	// A live heap the mark phase has to walk every cycle. Pointer-dense on
	// purpose: marking a megabyte of bytes costs nothing.
	live := make([]*payload, 0, 4096)
	for index := 0; index < 4096; index++ {
		live = append(live, chain(index))
	}

	var running sync.WaitGroup
	for worker := 0; worker < assistWorkers; worker++ {
		running.Add(1)
		go func(seed int) {
			defer running.Done()
			for round := 0; round < assistRounds; round++ {
				local := chain(seed*assistRounds + round)
				verifyChain(local, seed*assistRounds+round)
			}
		}(worker)
	}
	running.Wait()

	runtime.GC()
	for index, head := range live {
		verifyChain(head, index)
	}

	if charged := assistCPU(); charged <= 0 {
		println("assist cpu nanoseconds:", int(charged*1e9))
		panic("no mark assist CPU was charged, so this program never produced assist work")
	}

	println("assist credit ok")
}
