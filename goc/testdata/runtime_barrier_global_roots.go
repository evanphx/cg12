// Global-to-heap pointer write barriers.
//
// RUNTIME_PLAN.md section 6 asks for the global-to-heap barrier shape, and
// section 5.8 is the reason it needs its own capability: a package-level
// pointer word is a permanent GC root, nothing relocates it when a stack moves,
// and nothing clears it when a goroutine exits, so a global that receives a
// goroutine stack address is a stale root that faults some collections later.
//
// The capability runs under GODEBUG=cg12checkwb=2, whose second level rejects
// exactly that store: a barrier whose slot is in a module's data or bss and
// whose value is a goroutine stack address throws at the store. So this program
// is both a semantic test of the barrier and a sweep for that class of defect
// over every shape of global a Go program can have.
//
// The globals below cover the shapes separately because they reach different
// store paths in the compiler: a bare pointer is a scalar pointer store, a
// struct is an aggregate store that publishes each pointer word, a slice and a
// string carry a data pointer beside scalars, an interface is two words, and an
// array of pointers is a repeated bitmap.
package main

import (
	"runtime"
	"runtime/debug"
)

type globalNode struct {
	tag   int64
	left  *globalNode
	right *globalNode
}

type globalAggregate struct {
	before int64
	node   *globalNode
	middle int64
	other  *globalNode
	after  int64
}

type globalNamer interface {
	name() int64
}

func (n *globalNode) name() int64 {
	return n.tag
}

var globalPointer *globalNode
var globalAggregateValue globalAggregate
var globalSlice []*globalNode
var globalArray [8]*globalNode
var globalInterface globalNamer
var globalMapValue map[int64]*globalNode
var globalString string

const rounds = 400

// witnesses keeps every value a global ever held reachable, so that a store
// which skipped its deletion barrier cannot be excused by the value having been
// unreachable anyway.
var witnesses []*globalNode

func collectInBackground(done chan struct{}) {
	for {
		select {
		case <-done:
			close(done)
			return
		default:
			runtime.GC()
		}
	}
}

func newNode(tag int64) *globalNode {
	node := &globalNode{tag: tag}
	witnesses = append(witnesses, node)
	return node
}

func main() {
	debug.SetGCPercent(1)
	globalMapValue = make(map[int64]*globalNode)
	done := make(chan struct{})
	go collectInBackground(done)

	for round := 0; round < rounds; round++ {
		tag := int64(round)

		globalPointer = newNode(tag)
		globalPointer.left = newNode(tag + 1)
		globalPointer.right = newNode(tag + 2)

		globalAggregateValue = globalAggregate{
			before: tag,
			node:   newNode(tag + 3),
			middle: tag + 1,
			other:  newNode(tag + 4),
			after:  tag + 2,
		}

		globalSlice = append(globalSlice[:0], newNode(tag+5), newNode(tag+6))
		globalArray[round%len(globalArray)] = newNode(tag + 7)
		globalInterface = newNode(tag + 8)
		globalMapValue[tag%16] = newNode(tag + 9)
		globalString = string(rune('a' + round%26))
	}

	done <- struct{}{}
	<-done

	runtime.GC()
	runtime.GC()

	last := int64(rounds - 1)
	if globalPointer == nil || globalPointer.tag != last {
		panic("the global pointer lost its last value")
	}
	if globalPointer.left == nil || globalPointer.left.tag != last+1 {
		panic("a field of the object in the global pointer lost its value")
	}
	if globalPointer.right == nil || globalPointer.right.tag != last+2 {
		panic("a field of the object in the global pointer lost its value")
	}
	if globalAggregateValue.node == nil || globalAggregateValue.node.tag != last+3 {
		panic("a pointer word of the global aggregate lost its value")
	}
	if globalAggregateValue.other == nil || globalAggregateValue.other.tag != last+4 {
		panic("a pointer word of the global aggregate lost its value")
	}
	if globalAggregateValue.before != last ||
		globalAggregateValue.middle != last+1 ||
		globalAggregateValue.after != last+2 {
		panic("a scalar word of the global aggregate was disturbed")
	}
	if len(globalSlice) != 2 {
		panic("the global slice has the wrong length")
	}
	if globalSlice[0] == nil || globalSlice[0].tag != last+5 {
		panic("the global slice lost an element")
	}
	if globalSlice[1] == nil || globalSlice[1].tag != last+6 {
		panic("the global slice lost an element")
	}
	for index, element := range globalArray {
		if element == nil {
			panic("the global array lost an element")
		}
		expectedRound := int64(rounds - len(globalArray) + index)
		if element.tag != expectedRound+7 {
			panic("the global array element holds the wrong value")
		}
	}
	if globalInterface == nil || globalInterface.name() != last+8 {
		panic("the global interface lost its value")
	}
	if len(globalMapValue) != 16 {
		panic("the global map has the wrong size")
	}
	for key, value := range globalMapValue {
		if value == nil {
			panic("the global map lost a value")
		}
		// The stored value's tag is its round plus nine, and the key was the
		// round modulo sixteen.
		if (value.tag-9)%16 != key {
			panic("the global map value does not match its key")
		}
	}
	if globalString != string(rune('a'+(rounds-1)%26)) {
		panic("the global string lost its value")
	}

	if len(witnesses) != rounds*10 {
		panic("the witness list has the wrong length")
	}
	for index, witness := range witnesses {
		if witness == nil {
			panic("a witnessed value became nil")
		}
		round := int64(index / 10)
		offset := int64(index % 10)
		if witness.tag != round+offset {
			panic("a value a global once held was freed and reused")
		}
	}
}
