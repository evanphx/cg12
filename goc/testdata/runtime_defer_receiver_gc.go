// A deferred method call on a frame-local receiver, run under collector
// pressure.
//
// `defer box.done()` has no arguments, so goc builds the deferred function value
// out of `box.done` -- a *method value*, a descriptor holding the receiver's
// address -- and hands it to runtime.deferproc. While that descriptor was a heap
// object, the address of a frame-local receiver was written into it with a write
// barrier: a frame address inside a heap object, which the collector scans. This
// program fails about half the time at the commit before
// goc.gen.deferredFunctionValueStaysInFrame, with `sink` short of 40, and is
// deterministic under the host toolchain.
//
// opt.FrameEscapes reported the same thing statically --
//
//	main.work: barrier %t4 into memory reached through a call result
//	           $runtime.newobject
//
// -- which is why TestFrameEscapeAudit is the guard that matters for this file
// as much as running it does.
package main

import "runtime"

type deferredReceiverBox struct{ value int }

var deferredReceiverSink int

func (box *deferredReceiverBox) done() { deferredReceiverSink += box.value }

func work(value int) {
	box := deferredReceiverBox{value: value}
	defer box.done()
	for attempt := 0; attempt < 8; attempt++ {
		runtime.GC()
	}
}

func main() {
	for attempt := 0; attempt < 40; attempt++ {
		work(1)
	}
	if deferredReceiverSink != 40 {
		panic("deferred receiver did not survive the collector")
	}
	println("deferred receiver ok")
}
