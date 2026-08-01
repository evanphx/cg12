// Reducer, kept for the defect it demonstrates rather than as a capability.
//
// With `-O`, a heap pointer held in a loop-carried local is not a GC root: the
// chain below is reclaimed while `current` still points at it, and under
// GODEBUG=clobberfree=1 the walk at the end faults on 0xdeadbeefdeadbeef.
// Without `-O` it passes, and the host Go toolchain passes. It fails identically
// when compiled by a goc built from `main` (0505d90), so it predates the branch
// that found it.
//
// `simple` is the control: the same allocate/collect/use shape with no loop
// passes with `-O`, so it is the loop-carried case specifically.
//
// This file is deliberately **not** registered in the capability matrix.
// `stack-scan/loop-safepoints` already fails on the same defect and carries it
// as a `mustPass` failure in the optimized arm; a second failing capability would
// add noise without adding information. It is in the tree so the reducer outlives
// the job that found it. RUNTIME_PLAN.md section 6.1 records the measurements and
// the hypothesis.
//
// Run it as:
//
//	goc -O -o loop.bin runtime_opt_loop_carried_root.go
//	GOMAXPROCS=1 GODEBUG=clobberfree=1 ./loop.bin
package main

import "runtime"

type node struct {
	value int
	next  *node
}

//go:noinline
func newNode(value int, next *node) *node {
	return &node{value: value, next: next}
}

// churn allocates and drops enough garbage that a reclaimed chain's memory is
// handed out again before it is walked.
//
//go:noinline
func churn() {
	var sink []*node
	for index := 0; index < 20000; index++ {
		sink = append(sink, &node{value: index})
	}
	if len(sink) != 20000 {
		panic("churn lost its slice")
	}
}

// simple is the control: one object, live across two collections, no loop.
//
//go:noinline
func simple() int {
	object := newNode(42, nil)
	runtime.GC()
	churn()
	runtime.GC()
	return object.value
}

// loop keeps the head of a growing chain in a loop-carried local across a
// collection in every iteration.
//
//go:noinline
func loop(rounds int) int {
	current := newNode(0, nil)
	for round := 1; round <= rounds; round++ {
		runtime.GC()
		churn()
		current = newNode(round, current)
	}
	runtime.GC()
	churn()
	runtime.GC()

	depth := 0
	sum := 0
	for walk := current; walk != nil; walk = walk.next {
		depth++
		sum += walk.value
	}
	return depth*1000 + sum
}

func main() {
	if got := simple(); got != 42 {
		println("simple", got)
		panic("simple lost its object")
	}
	if got := loop(6); got != 7021 {
		println("loop", got)
		panic("loop lost its chain")
	}
	println("opt loop carried root ok")
}
