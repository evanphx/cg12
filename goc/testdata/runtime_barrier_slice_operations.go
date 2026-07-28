// Slice write barriers: element assignment, append, copy, and clear.
//
// RUNTIME_PLAN.md section 6 lists the slice barrier shape. It is more than one
// path in cg12: assigning one element is an ordinary pointer store, but the
// bulk operations are routed to typed runtime helpers precisely so their
// barriers are right.
//
//   - copy of a slice with pointer elements becomes runtime.typedslicecopy,
//     which runs a bulk barrier over the destination before moving the bytes.
//     A plain memmove here would skip the deletion barrier for every element it
//     overwrites.
//   - clear of a slice with pointer elements becomes runtime.memclrHasPointers
//     for the same reason: the words being zeroed have to reach the barrier
//     before they are lost.
//   - append copies its added elements the same way when it does not grow, and
//     when it does grow the new backing array is published through the slice
//     header's data word.
//
// Every element ever written is also held in a witness list, so a bulk
// operation that skipped its barrier while a value was reachable only from the
// overwritten slot would show up as a freed and reused object.
//
// The capability runs under GODEBUG=cg12checkwb=2.
package main

import (
	"runtime"
	"runtime/debug"
)

type element struct {
	tag int64
}

type sliceHolder struct {
	elements []*element
	grown    []*element
	copied   []*element
	cleared  []*element
}

const rounds = 200
const width = 32

var holders []*sliceHolder
var witnesses []*element
var globalElements []*element

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

func newElement(tag int64) *element {
	value := &element{tag: tag}
	witnesses = append(witnesses, value)
	return value
}

func main() {
	debug.SetGCPercent(1)
	done := make(chan struct{})
	go collectInBackground(done)

	for round := 0; round < rounds; round++ {
		base := int64(round) * 1000
		holder := &sliceHolder{}

		// Element assignment into a heap-allocated backing array.
		holder.elements = make([]*element, width)
		for index := range holder.elements {
			holder.elements[index] = newElement(base + int64(index))
		}

		// Append that grows: the slice header's data word is republished.
		holder.grown = make([]*element, 0, 1)
		for index := 0; index < width; index++ {
			holder.grown = append(holder.grown, newElement(base+100+int64(index)))
		}

		// Overwrite every element once more, so the deletion half of the
		// barrier has a previous value to publish at every slot.
		for index := range holder.elements {
			holder.elements[index] = newElement(base + 200 + int64(index))
		}

		// copy over a fully populated destination: every overwritten slot held
		// a pointer.
		holder.copied = make([]*element, width)
		for index := range holder.copied {
			holder.copied[index] = newElement(base + 300 + int64(index))
		}
		if copy(holder.copied, holder.grown) != width {
			panic("copy did not move every element")
		}

		// clear over a fully populated slice.
		holder.cleared = make([]*element, width)
		for index := range holder.cleared {
			holder.cleared[index] = newElement(base + 400 + int64(index))
		}
		clear(holder.cleared)

		// append from another slice rather than from individual values, which
		// is the typedslicecopy path inside append.
		holder.grown = append(holder.grown, holder.elements...)

		holders = append(holders, holder)
		globalElements = append(globalElements[:0], holder.elements...)
	}

	done <- struct{}{}
	<-done

	runtime.GC()
	runtime.GC()

	if len(holders) != rounds {
		panic("the holder list has the wrong length")
	}
	for round, holder := range holders {
		base := int64(round) * 1000
		if len(holder.elements) != width {
			panic("an element slice has the wrong length")
		}
		for index, value := range holder.elements {
			if value == nil || value.tag != base+200+int64(index) {
				panic("a slice element lost its referent")
			}
		}
		if len(holder.grown) != 2*width {
			panic("a grown slice has the wrong length")
		}
		for index := 0; index < width; index++ {
			value := holder.grown[index]
			if value == nil || value.tag != base+100+int64(index) {
				panic("an appended element lost its referent")
			}
		}
		for index := 0; index < width; index++ {
			value := holder.grown[width+index]
			if value == nil || value.tag != base+200+int64(index) {
				panic("an element appended from another slice lost its referent")
			}
		}
		if len(holder.copied) != width {
			panic("a copied slice has the wrong length")
		}
		for index, value := range holder.copied {
			if value == nil || value.tag != base+100+int64(index) {
				panic("a copied element lost its referent")
			}
		}
		if len(holder.cleared) != width {
			panic("a cleared slice has the wrong length")
		}
		for _, value := range holder.cleared {
			if value != nil {
				panic("clear left a pointer element behind")
			}
		}
	}

	lastBase := int64(rounds-1) * 1000
	if len(globalElements) != width {
		panic("the global element slice has the wrong length")
	}
	for index, value := range globalElements {
		if value == nil || value.tag != lastBase+200+int64(index) {
			panic("a global slice element lost its referent")
		}
	}

	// Every value that any of these operations ever overwrote is still
	// reachable through witnesses, so none of them may have been collected.
	expected := rounds * (5 * width)
	if len(witnesses) != expected {
		panic("the witness list has the wrong length")
	}
	for index, witness := range witnesses {
		if witness == nil {
			panic("a witnessed value became nil")
		}
		round := int64(index / (5 * width))
		position := index % (5 * width)
		group := int64(position / width)
		offset := int64(position % width)
		if witness.tag != round*1000+group*100+offset {
			panic("a value that a bulk slice operation overwrote was freed and reused")
		}
	}
}
