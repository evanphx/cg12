// Checkmark mode: the collector marking the same heap twice and comparing.
//
// GODEBUG=gccheckmark=1 makes gcMarkDone stop the world after the concurrent
// mark phase and re-run the whole mark from the roots with the world stopped,
// this time recording into a separate checkmark bit per object. Any object the
// second, non-concurrent mark reaches that the first one did not is a pointer
// the concurrent phase missed, and the runtime throws "checkmark found
// unmarked object" naming it.
//
// That is the strongest single statement available about stack maps and write
// barriers together, and it is exactly what a program with a green exit status
// cannot tell you: the concurrent phase can miss an object and the program still
// finish, because the object is only lost at the sweep two cycles later. It also
// disables Green Tea's span queue -- tryDeferToSpanScan returns false under
// useCheckmark -- so the second mark walks objects individually and the
// comparison is against the plain marking path.
//
// The heap below is built to give the concurrent phase every chance to be wrong:
// pointers are stored while marking is in progress, through each barrier shape;
// stacks grow and are copied during the cycle; goroutines park and wake; and
// objects are made unreachable from one place and reachable from another in the
// same window. Everything is then checked for contents as well, so a wrong
// answer that checkmark somehow misses is still caught.
package main

import (
	"runtime"
	"runtime/debug"
	"sync"
)

type knot struct {
	value    int
	label    string
	left     *knot
	right    *knot
	slice    []*knot
	mapped   map[int]*knot
	boxed    any
	deferred func() int
}

const (
	knotDepth  = 5
	knotWeaves = 200
	weavers    = 4
)

//go:noinline
func weave(seed, depth int) *knot {
	node := &knot{
		value: seed,
		label: "knot-" + string(rune('a'+seed%26)),
	}
	if depth == 0 {
		return node
	}
	node.left = weave(seed*2+1, depth-1)
	node.right = weave(seed*2+2, depth-1)
	node.slice = []*knot{node.left, node.right}
	node.mapped = map[int]*knot{0: node.left, 1: node.right}
	node.boxed = node.right
	node.deferred = func() int { return node.left.value }
	return node
}

//go:noinline
func check(node *knot, seed, depth int) int {
	if node == nil {
		println("nil knot at", seed)
		panic("checkmark: a knot was lost")
	}
	if node.value != seed || node.label != "knot-"+string(rune('a'+seed%26)) {
		println("knot", seed, "reads", node.value, node.label)
		panic("checkmark: a knot was corrupted")
	}
	if depth == 0 {
		return 1
	}
	if len(node.slice) != 2 || node.slice[0] != node.left || node.slice[1] != node.right {
		panic("checkmark: a knot's slice diverged from its fields")
	}
	if node.mapped[0] != node.left || node.mapped[1] != node.right {
		panic("checkmark: a knot's map diverged from its fields")
	}
	if boxed, ok := node.boxed.(*knot); !ok || boxed != node.right {
		panic("checkmark: a knot's interface diverged from its fields")
	}
	if node.deferred() != node.left.value {
		panic("checkmark: a knot's closure diverged from its fields")
	}
	return 1 + check(node.left, seed*2+1, depth-1) + check(node.right, seed*2+2, depth-1)
}

// deepen grows the stack while a cycle is in progress, so the checkmark pass
// scans a stack that was copied during the concurrent phase.
//
//go:noinline
func deepen(depth int, node *knot) int {
	var scratch [32]uintptr
	scratch[0] = uintptr(depth)
	if depth == 0 {
		runtime.GC()
		return node.value
	}
	return deepen(depth-1, node) + int(scratch[0]&1)
}

func main() {
	previous := debug.SetGCPercent(30)
	defer debug.SetGCPercent(previous)

	roots := make([]*knot, 0, 8)
	for index := 0; index < 8; index++ {
		roots = append(roots, weave(index+1, knotDepth))
	}
	for index, root := range roots {
		check(root, index+1, knotDepth)
	}

	var guard sync.Mutex
	shared := make(map[int]*knot)
	handoff := make(chan *knot, 16)

	var running sync.WaitGroup
	for worker := 0; worker < weavers; worker++ {
		running.Add(1)
		go func(index int) {
			defer running.Done()
			for round := 0; round < knotWeaves; round++ {
				seed := index*knotWeaves + round + 100
				fresh := weave(seed, 3)

				// Publish the only reference through a shape that needs a
				// barrier, then take it out again.
				guard.Lock()
				shared[seed] = fresh
				guard.Unlock()

				select {
				case handoff <- fresh:
				default:
				}

				guard.Lock()
				delete(shared, seed)
				guard.Unlock()

				check(fresh, seed, 3)
			}
		}(worker)
	}

	stop := make(chan struct{})
	var collecting sync.WaitGroup
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
			for {
				select {
				case node := <-handoff:
					check(node, node.value, 3)
					continue
				default:
				}
				break
			}
		}
	}()

	if got := deepen(600, roots[0]); got != 301 {
		println("deepen", got)
		panic("deepen returned the wrong value")
	}

	running.Wait()
	close(stop)
	collecting.Wait()

	runtime.GC()
	for index, root := range roots {
		check(root, index+1, knotDepth)
	}
	for {
		select {
		case node := <-handoff:
			check(node, node.value, 3)
			continue
		default:
		}
		break
	}

	println("checkmark ok")
}
