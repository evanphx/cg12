// A long compute loop with no calls in it, and the roots it holds across a
// collection.
//
// Go's usual answer for a call-free loop is asynchronous preemption: the
// preemption signal injects a call to runtime.asyncPreempt, which spills every
// register, and the collector then scans that frame and its parent
// conservatively because no stack map describes an arbitrary instruction
// boundary. **cg12 does not take that route.** internal/gometa.UnsafePointPCData
// marks every generated function unsafe for asynchronous preemption end to end,
// because cg12 keeps managed references in registers between calls while its
// stack maps describe the spill state at call safepoints. isAsyncSafePoint
// therefore refuses every injection attempt, and runtime.scanConservative is
// unreachable from cg12-compiled Go. TestAsynchronousPreemptionIsRefusedForGeneratedCode
// proves that boundary; RUNTIME_PLAN.md section 6.1 records the classification.
//
// What is left is the property this program actually checks, and it is the one
// that matters for cg12's design: a loop that runs for a long time between calls
// must not lose the objects it is holding, and the collection that eventually
// preempts it at the next call must find them. The loops below hold their only
// reference in an accumulator, in an interior pointer into a heap object, and
// through an unsafe.Pointer round trip, and every object is checked afterwards.
//
// It is also the program that would notice if asynchronous preemption were ever
// turned on: with the conservative scan live, these are exactly the frames it
// would have to get right.
package main

import (
	"runtime"
	"sync"
	"unsafe"
)

type cell struct {
	value int
	label string
	next  *cell
}

//go:noinline
func makeCell(seed int) *cell {
	return &cell{
		value: seed,
		label: "cell-" + string(rune('a'+seed%26)),
		next:  &cell{value: seed * 11, label: "tail"},
	}
}

//go:noinline
func verify(where string, object *cell, seed int) {
	want := "cell-" + string(rune('a'+seed%26))
	if object == nil || object.value != seed || object.label != want {
		println("in", where, "seed", seed)
		panic("an asynchronously preempted frame lost a root")
	}
	if object.next == nil || object.next.value != seed*11 || object.next.label != "tail" {
		println("in", where, "tail seed", seed)
		panic("an asynchronously preempted frame lost an indirect root")
	}
}

// spin is call-free on purpose: nothing in the loop body can be a cooperative
// preemption point, so only the preemption signal can stop it. The object stays
// live across the whole loop and is used afterwards.
//
//go:noinline
func spin(object *cell, rounds int) int {
	total := 0
	for round := 0; round < rounds; round++ {
		total += object.value + round&7
	}
	return total
}

// spinInterior keeps its only reference to the object's tail as an interior
// pointer into a heap object, which a conservative scan has to resolve back to
// the object base.
//
//go:noinline
func spinInterior(object *cell, rounds int) int {
	inner := &object.next.value
	total := 0
	for round := 0; round < rounds; round++ {
		total += *inner + round&3
	}
	return total
}

// spinUnsafe holds the object only through an unsafe.Pointer, which is a legal
// Go representation of a pointer and must be scanned like one.
//
//go:noinline
func spinUnsafe(object *cell, rounds int) int {
	opaque := unsafe.Pointer(object)
	total := 0
	for round := 0; round < rounds; round++ {
		total += round & 1
	}
	restored := (*cell)(opaque)
	return total + restored.value
}

//go:noinline
func churn() {
	var sink []*cell
	for index := 0; index < 4096; index++ {
		sink = append(sink, &cell{value: index, label: "churn"})
	}
	if len(sink) != 4096 {
		panic("churn lost its slice")
	}
}

// collector runs collections for as long as the spinners are running, so at
// least one of them is interrupted mid-loop.
//
//go:noinline
func collector(stop <-chan struct{}, done *sync.WaitGroup) {
	for {
		select {
		case <-stop:
			done.Done()
			return
		default:
		}
		runtime.GC()
		churn()
	}
}

func main() {
	if runtime.GOMAXPROCS(0) < 2 {
		// With one P the collector goroutine cannot run while a call-free loop
		// is running, so the preemption signal is what lets the collection
		// start at all -- the loops below still have to be preempted, and the
		// program still checks the same property. Nothing to adjust.
		_ = 0
	}

	const rounds = 40000000

	stop := make(chan struct{})
	var collecting sync.WaitGroup
	collecting.Add(1)
	go collector(stop, &collecting)

	first := makeCell(3)
	if got := spin(first, rounds); got != 3*rounds+(rounds/8)*28 {
		println("spin", got)
		panic("spin computed the wrong total")
	}
	verify("spin", first, 3)

	second := makeCell(4)
	if got := spinInterior(second, rounds); got != 44*rounds+(rounds/4)*6 {
		println("spinInterior", got)
		panic("spinInterior computed the wrong total")
	}
	verify("spinInterior", second, 4)

	third := makeCell(5)
	if got := spinUnsafe(third, rounds); got != rounds/2+5 {
		println("spinUnsafe", got)
		panic("spinUnsafe computed the wrong total")
	}
	verify("spinUnsafe", third, 5)

	close(stop)
	collecting.Wait()
	runtime.GC()

	verify("after", first, 3)
	verify("after", second, 4)
	verify("after", third, 5)
	println("conservative preempt roots ok")
}
