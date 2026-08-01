// A pointer that is live across a loop back edge must be a GC root at every
// safepoint inside the loop body, not only at the safepoints the entry block
// dominates.
//
// The property is about liveness, not about a single stack map: a loop rotates
// through several blocks, and the value a variable holds when control reaches a
// safepoint was stored by a different iteration than the one that reads it. A
// stack map computed for the entry block, or for the block that last wrote the
// slot, describes the wrong iteration. Each shape below keeps its object
// reachable only through a frame slot the loop rewrites, and reaches a safepoint
// -- runtime.GC(), an allocation that can trigger one, a channel operation --
// with the current value live.
//
// Premature collection is observed rather than inferred. Every object carries a
// runtime.AddCleanup that pushes its label onto a channel, the program records
// which labels are still required to be live, and the loop drains that channel
// on every iteration: a collected-while-live object names itself. The detector
// is proved non-vacuous by a positive control at the end, which drops an object
// and requires its label to arrive.
//
// Run under GODEBUG=cg12scanroots=1, which prints every heap object the precise
// stack scan retains together with the frame and stack-map slot that retains it.
// A frame slot that stops naming its object between iterations is the failure
// this program is looking for.
package main

import (
	"runtime"
	"sync"
)

type node struct {
	value int
	next  *node
	label string
}

var collected = make(chan string, 64)

// live is the set of labels whose objects must not be collected yet. The worker
// goroutine in blocked() releases labels too, so it is guarded.
var live struct {
	sync.Mutex
	labels map[string]bool
}

func init() {
	live.labels = make(map[string]bool)
}

//go:noinline
func newNode(value int, label string) *node {
	object := &node{value: value, label: label}
	live.Lock()
	live.labels[label] = true
	live.Unlock()
	runtime.AddCleanup(object, func(name string) {
		collected <- name
	}, label)
	return object
}

//go:noinline
func release(label string) {
	live.Lock()
	delete(live.labels, label)
	live.Unlock()
}

//go:noinline
func stillLive(label string) bool {
	live.Lock()
	defer live.Unlock()
	return live.labels[label]
}

// drain fails the program the moment an object that is still required to be
// live is collected.
//
//go:noinline
func drain(where string) {
	for {
		select {
		case label := <-collected:
			if stillLive(label) {
				println("collected while live:", label, "at", where)
				panic("a stack slot live across a loop back edge was not a GC root")
			}
		default:
			return
		}
	}
}

// carried keeps one pointer live across the back edge and rewrites it every
// iteration. The safepoint is runtime.GC() in the middle of the body, so the
// value scanned is the one the previous iteration stored.
//
//go:noinline
func carried(rounds int) int {
	total := 0
	current := newNode(0, "carried-0")
	for round := 1; round <= rounds; round++ {
		runtime.GC()
		drain("carried before rewrite")
		total += current.value
		next := newNode(round, "carried-"+string(rune('a'+round)))
		next.next = current
		current = next
		drain("carried after rewrite")
	}
	runtime.GC()
	drain("carried after loop")
	depth := 0
	for walk := current; walk != nil; walk = walk.next {
		depth++
		release(walk.label)
	}
	return total + depth
}

// accumulated keeps a slice of pointers live across the back edge and grows it,
// so the backing array is reallocated inside the loop and the allocation is
// itself the safepoint that matters.
//
//go:noinline
func accumulated(rounds int) int {
	var items []*node
	for round := 0; round < rounds; round++ {
		items = append(items, newNode(round, "acc-"+string(rune('a'+round))))
		runtime.GC()
		drain("accumulated")
	}
	total := 0
	for _, item := range items {
		total += item.value
		release(item.label)
	}
	return total
}

// blocked reaches its safepoint through a channel operation rather than through
// runtime.GC(): the loop hands its live pointer to another goroutine, and the
// sender is parked with held live in its frame while the receiver collects.
//
//go:noinline
func blocked(rounds int) int {
	work := make(chan *node)
	results := make(chan int)
	go func() {
		total := 0
		for item := range work {
			runtime.GC()
			total += item.value
			release(item.label)
		}
		results <- total
	}()

	held := newNode(1000, "blocked-held")
	for round := 0; round < rounds; round++ {
		item := newNode(round, "blocked-"+string(rune('a'+round)))
		work <- item
		drain("blocked")
		if held.value != 1000 {
			panic("a pointer live across a blocking channel send lost its object")
		}
	}
	close(work)
	total := <-results
	drain("blocked after close")
	if held.value != 1000 {
		panic("a pointer live across a loop with channel safepoints lost its object")
	}
	release(held.label)
	return total
}

// nested keeps one pointer live across the outer back edge and another across
// the inner one, with the safepoint in the inner body, so the outer variable's
// slot is scanned from a stack map selected for an inner-loop PC.
//
//go:noinline
func nested(outer, inner int) int {
	total := 0
	for outerRound := 0; outerRound < outer; outerRound++ {
		outerNode := newNode(outerRound, "nested-outer-"+string(rune('a'+outerRound)))
		for innerRound := 0; innerRound < inner; innerRound++ {
			innerLabel := "nested-inner-" + string(rune('a'+outerRound)) + string(rune('a'+innerRound))
			innerNode := newNode(innerRound, innerLabel)
			runtime.GC()
			drain("nested")
			total += innerNode.value + outerNode.value
			release(innerNode.label)
		}
		if outerNode.value != outerRound {
			panic("an outer-loop pointer lost its object across an inner safepoint")
		}
		release(outerNode.label)
	}
	return total
}

// dropped is the positive control. It allocates an object, registers the same
// cleanup every other object here uses, and returns without keeping it, so the
// label must arrive on the channel. Without this the whole detector could be
// silently dead and every other check above would still pass.
//
//go:noinline
func dropped() {
	object := &node{value: -1, label: "dropped"}
	runtime.AddCleanup(object, func(name string) {
		collected <- name
	}, object.label)
}

//go:noinline
func awaitDropped() {
	for attempt := 0; attempt < 200; attempt++ {
		runtime.GC()
		select {
		case label := <-collected:
			if label == "dropped" {
				return
			}
			if stillLive(label) {
				println("collected while live:", label, "at awaitDropped")
				panic("a live object was collected while waiting for the control")
			}
		default:
		}
	}
	panic("the positive control was never collected: this program cannot detect a lost root")
}

func main() {
	carriedTotal := carried(6)
	if carriedTotal != 22 {
		println("carried", carriedTotal)
		panic("wrong carried total")
	}

	accumulatedTotal := accumulated(8)
	if accumulatedTotal != 28 {
		println("accumulated", accumulatedTotal)
		panic("wrong accumulated total")
	}

	blockedTotal := blocked(6)
	if blockedTotal != 15 {
		println("blocked", blockedTotal)
		panic("wrong blocked total")
	}

	nestedTotal := nested(4, 3)
	if nestedTotal != 30 {
		println("nested", nestedTotal)
		panic("wrong nested total")
	}

	runtime.GC()
	drain("final")

	dropped()
	awaitDropped()

	println("loop safepoint roots ok")
}
