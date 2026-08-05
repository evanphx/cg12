// Reducer, kept for the defect it demonstrates rather than as a capability.
//
// A local of interface type is a frame slot holding the address of the two-word
// descriptor, and here that descriptor is the home `net.Listen`-shaped call
// results are written into -- an allocation in *this* frame. Promoting the slot
// makes the call's own result temporary the value every later read sees, so a
// pointer that used to live in a frame slot the safepoint map described now
// lives in an SSA value nothing describes. When `deepen` grows the goroutine
// stack, `copystack` walks the frame map and never sees it, so `c` is left
// pointing into the old stack; the goroutines then recycle that stack and write
// 0x5e5e5e5e5e5e over it, and `c.value()` dispatches through the scribble:
//
//	unexpected fault address 0x5e5e5e5e60bd
//
// The fault address is the reducer's whole point -- it is the padding pattern
// `deepen` writes, so the read is demonstrably coming out of a freed stack that
// something else has since used.
//
// It passes without `-O`, passes with `-O` when mem2reg does not run, and passes
// under the host Go toolchain. `makeCounter` returns two results because that is
// what stops the front end from copying the descriptor into a fresh alloca
// first: a single-result assignment goes through adaptInterfaceToInterface,
// whose copy is an ordinary frame allocation that the backend readdresses rather
// than a value it must keep somewhere.
//
// See goc/optgcroot_test.go, which runs it, and CCWORK_REPORT.md on the branch
// that found it for the bisection down to the four functions of
// stdlib-netpoll-stress/tcp-churn this is the reduction of.
package main

type counter interface {
	value() int
}

type box struct{ n int }

func (b *box) value() int { return b.n }

//go:noinline
func makeCounter(n int) (counter, int) { return &box{n: n}, n }

// deepen grows the goroutine stack far enough that the runtime has to allocate a
// larger one and copy the live frames onto it, and fills every frame it makes
// with a recognisable pattern so a read from a recycled stack is unmistakable.
//
//go:noinline
func deepen(depth int) int {
	var padding [32]int
	if depth == 0 {
		return 0
	}
	for index := range padding {
		padding[index] = 0x5e5e5e5e5e5e + depth
	}
	return padding[0] + padding[31] + deepen(depth-1)
}

func main() {
	c, n := makeCounter(7)
	if n != 7 {
		panic("makeCounter lost its second result")
	}
	if deepen(600) == 0 {
		panic("deepen returned zero")
	}
	// Enough goroutines, each growing its own stack the same way, that the
	// stacks main's growth freed are handed out again and written over.
	done := make(chan int, 64)
	for round := 0; round < 64; round++ {
		go func() { done <- deepen(600) }()
	}
	for round := 0; round < 64; round++ {
		<-done
	}
	if got := c.value(); got != 7 {
		println("value", got)
		panic("promoted interface lost its descriptor")
	}
	println("opt promoted interface root ok")
}
