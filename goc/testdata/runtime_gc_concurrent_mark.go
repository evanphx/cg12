// Allocating and mutating a pointer graph while the collector is marking it.
//
// Concurrent marking is correct only because of the write barrier: while
// gcphase is _GCmark, every pointer store buffers the old value and every
// freshly allocated object is allocated black. Break either one and an object
// that was reachable throughout the cycle can still be collected -- the classic
// hidden-pointer case, where the mutator moves the only reference to an object
// from a not-yet-scanned location into an already-scanned one.
//
// That is what the mutators below do on purpose. Each one repeatedly takes the
// only reference to a subtree out of an unscanned node and hangs it under a node
// the collector has probably already scanned, while other goroutines are
// allocating hard enough to keep several cycles running back to back. The graph
// is then walked and every node checked, so a lost subtree is a wrong answer
// rather than a statistic.
//
// The shapes moved are the ones with different barrier paths: a plain pointer
// field, a slice element, a map value, an interface word, and a channel element.
//
// This capability is deliberately allocation-heavy and is marked exclusive; run
// it under GODEBUG=cg12checkwb=2 to have every word the barrier buffers checked
// as it is buffered, so a bad store throws at the store rather than at the next
// collection.
package main

import (
	"runtime"
	"runtime/debug"
	"sync"
)

type subtree struct {
	value    int
	label    string
	children []*subtree
}

type holder struct {
	direct    *subtree
	inSlice   []*subtree
	inMap     map[int]*subtree
	asAny     any
	guard     sync.Mutex
	generated int
}

const (
	mutators    = 6
	rounds      = 300
	fanout      = 4
	treeDepth   = 4
	channelSize = 32
)

//go:noinline
func buildSubtree(seed, depth int) *subtree {
	node := &subtree{
		value: seed,
		label: "sub-" + string(rune('a'+seed%26)),
	}
	if depth == 0 {
		return node
	}
	for child := 0; child < fanout; child++ {
		node.children = append(node.children, buildSubtree(seed*fanout+child+1, depth-1))
	}
	return node
}

//go:noinline
func checkSubtree(node *subtree, seed, depth int) int {
	if node == nil {
		println("nil subtree at seed", seed)
		panic("a subtree moved during marking was collected")
	}
	if node.value != seed || node.label != "sub-"+string(rune('a'+seed%26)) {
		println("subtree", seed, "reads", node.value, node.label)
		panic("a subtree moved during marking was corrupted")
	}
	count := 1
	if depth == 0 {
		if len(node.children) != 0 {
			panic("a leaf subtree grew children")
		}
		return count
	}
	if len(node.children) != fanout {
		println("subtree", seed, "has", len(node.children), "children")
		panic("a subtree moved during marking lost children")
	}
	for child := 0; child < fanout; child++ {
		count += checkSubtree(node.children[child], seed*fanout+child+1, depth-1)
	}
	return count
}

// hide moves the only reference to a fresh subtree through every barrier shape
// in turn, so at least one of the moves happens while the destination has
// already been scanned and the source has not.
//
//go:noinline
func hide(target *holder, channel chan *subtree, seed int) {
	fresh := buildSubtree(seed, treeDepth)

	target.guard.Lock()
	target.direct = fresh
	target.generated++
	target.guard.Unlock()

	target.guard.Lock()
	moved := target.direct
	target.direct = nil
	target.inSlice = append(target.inSlice, moved)
	if len(target.inSlice) > 8 {
		target.inSlice = target.inSlice[1:]
	}
	target.guard.Unlock()

	target.guard.Lock()
	fromSlice := target.inSlice[len(target.inSlice)-1]
	target.inMap[seed%64] = fromSlice
	target.asAny = fromSlice
	target.guard.Unlock()

	select {
	case channel <- fresh:
	default:
	}

	if checkSubtree(fresh, seed, treeDepth) == 0 {
		panic("an empty subtree")
	}
}

//go:noinline
func drain(channel chan *subtree, seeds map[int]bool, guard *sync.Mutex) {
	for {
		select {
		case node := <-channel:
			guard.Lock()
			known := seeds[node.value]
			guard.Unlock()
			if !known {
				println("unknown subtree", node.value)
				panic("a channel element was not one of the subtrees sent")
			}
			checkSubtree(node, node.value, treeDepth)
		default:
			return
		}
	}
}

func main() {
	previous := debug.SetGCPercent(20)
	defer debug.SetGCPercent(previous)

	// A live graph the collector has to trace every cycle, so marking takes
	// long enough for the mutators to run inside it.
	roots := make([]*subtree, 0, 32)
	for index := 0; index < 32; index++ {
		roots = append(roots, buildSubtree(index+1, treeDepth))
	}

	targets := make([]*holder, mutators)
	for index := range targets {
		targets[index] = &holder{inMap: make(map[int]*subtree)}
	}
	channel := make(chan *subtree, channelSize)

	var seedGuard sync.Mutex
	seeds := make(map[int]bool)

	var running sync.WaitGroup
	for worker := 0; worker < mutators; worker++ {
		running.Add(1)
		go func(index int) {
			defer running.Done()
			for round := 0; round < rounds; round++ {
				seed := index*rounds + round + 1000
				seedGuard.Lock()
				seeds[seed] = true
				seedGuard.Unlock()
				hide(targets[index], channel, seed)
			}
		}(worker)
	}

	var collecting sync.WaitGroup
	stop := make(chan struct{})
	collecting.Add(1)
	go func() {
		defer collecting.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.GC()
			drain(channel, seeds, &seedGuard)
		}
	}()

	running.Wait()
	close(stop)
	collecting.Wait()

	runtime.GC()
	drain(channel, seeds, &seedGuard)

	for index, root := range roots {
		checkSubtree(root, index+1, treeDepth)
	}
	for index, target := range targets {
		target.guard.Lock()
		if target.generated != rounds {
			println("target", index, "generated", target.generated)
			target.guard.Unlock()
			panic("a mutator did not run every round")
		}
		for _, node := range target.inSlice {
			checkSubtree(node, node.value, treeDepth)
		}
		for _, node := range target.inMap {
			checkSubtree(node, node.value, treeDepth)
		}
		if node, ok := target.asAny.(*subtree); ok {
			checkSubtree(node, node.value, treeDepth)
		} else {
			target.guard.Unlock()
			panic("the interface word lost its subtree")
		}
		target.guard.Unlock()
	}

	println("concurrent mark ok")
}
