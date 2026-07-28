// Minimal reproducer for the GC-assist allocation recursion described in
// RUNTIME_PLAN.md 5.2.1.
//
// Several goroutines call runtime.GC() at the same time, so each of them
// allocates while gcBlackenEnabled is set and therefore enters
// runtime.gcAssistAlloc. cg12 emits a runtime.newobject call at the top of
// gcAssistAlloc, for the variable captured by that function's synctest defer,
// so the assist path itself allocates and
//
//	mallocgc -> deductAssistCredit -> gcAssistAlloc -> newobject -> mallocgc
//
// recurses without bound: every level takes on fresh assist debt before any
// level performs scan work. Each level costs a stack frame, so the goroutine
// stack doubles repeatedly until the mark phase happens to end. The enlarged
// stacks then make the next mark phase longer, which lets the recursion go
// deeper still, which is why the cost per GC cycle grows superlinearly.
//
// Host Go holds StackInuse at roughly 288 KiB for this program. cg12 is past
// 4 MiB inside the first GC cycle and reaches gigabytes within seconds.
//
// Keep the runtime.GC() calls concurrent. A single goroutine calling GC in a
// loop never assists -- it is parked in gcWaitOnMark while the dedicated mark
// worker does the marking -- and does not reproduce this at all.
package main

import (
	"runtime"
	"sync"
)

// Host Go stays two orders of magnitude below this bound, and the failing cg12
// build crosses it during the first cycle, so the threshold does not need to be
// tight to separate the two.
const stackInuseLimit = 4 << 20

func assistWorker(wait *sync.WaitGroup) {
	defer wait.Done()

	var stats runtime.MemStats
	for round := 0; round < 4; round++ {
		runtime.GC()
		runtime.ReadMemStats(&stats)
		if stats.StackInuse > stackInuseLimit {
			println("stackInuse", int(stats.StackInuse), "limit", stackInuseLimit)
			panic("goroutine stacks grew without bound during concurrent GC")
		}
	}
}

func main() {
	const goroutines = 8

	var wait sync.WaitGroup
	wait.Add(goroutines)
	for index := 0; index < goroutines; index++ {
		go assistWorker(&wait)
	}
	wait.Wait()
}
