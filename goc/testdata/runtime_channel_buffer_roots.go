// A buffered channel's elements are GC roots for as long as they sit in the
// buffer.
//
// runtime.makechan decides how to allocate that buffer from the element type
// descriptor the compiler hands it: when the element contains no pointers the
// buffer is carved out of the same no-scan allocation as the hchan, and only
// when it does contain pointers is a separate scannable object allocated. A
// descriptor whose PtrBytes is zero therefore does not merely lose a barrier --
// it puts every buffered element somewhere the mark phase never looks. The same
// descriptor is what chansend, chanrecv and sendDirect give typedmemmove and
// bulkBarrierPreWriteSrcOnly, so it decides the element copy's write barrier
// too.
//
// One element and one collection are not enough to see this: the object is only
// clobbered once the sweeper has reclaimed it and the space has been reused.
// Each shape below fills a buffer, drops every other reference, collects
// repeatedly, and only then drains and checks the contents.
//
// Run this under GODEBUG=clobberfree=1 to turn a use-after-free into a
// deterministic wrong answer rather than a probabilistic one: a reclaimed
// element reads back as the 0xdeadbeef pattern instead of whatever happens to
// have reused the memory.
package main

import "runtime"

type box struct {
	value int
	next  *box
}

type pair struct {
	name string
	box  *box
}

//go:noinline
func computed(index int) string {
	return "element-" + string(rune('a'+index))
}

//go:noinline
func collectRepeatedly() {
	for cycle := 0; cycle < 4; cycle++ {
		runtime.GC()
	}
}

// churn allocates enough garbage between the fill and the drain that a reclaimed
// element's memory is handed out again, so a lost element is a wrong answer even
// without GODEBUG=clobberfree=1.
//
//go:noinline
func churn() {
	var sink []*box
	for index := 0; index < 4096; index++ {
		sink = append(sink, &box{value: index})
	}
	if len(sink) != 4096 {
		panic("churn lost its slice")
	}
}

//go:noinline
func strings(count int) {
	channel := make(chan string, count)
	for index := 0; index < count; index++ {
		channel <- computed(index)
	}
	collectRepeatedly()
	churn()
	collectRepeatedly()
	for index := 0; index < count; index++ {
		got := <-channel
		want := computed(index)
		if got != want {
			println("string element", index, "is", got, "want", want)
			panic("a buffered channel lost a string element")
		}
	}
}

//go:noinline
func pointers(count int) {
	channel := make(chan *box, count)
	for index := 0; index < count; index++ {
		channel <- &box{value: index, next: &box{value: index * 2}}
	}
	collectRepeatedly()
	churn()
	collectRepeatedly()
	for index := 0; index < count; index++ {
		got := <-channel
		if got.value != index || got.next == nil || got.next.value != index*2 {
			println("pointer element", index, "is", got.value)
			panic("a buffered channel lost a pointer element")
		}
	}
}

//go:noinline
func interfaces(count int) {
	channel := make(chan any, count)
	for index := 0; index < count; index++ {
		if index%2 == 0 {
			channel <- &box{value: index}
		} else {
			channel <- computed(index)
		}
	}
	collectRepeatedly()
	churn()
	collectRepeatedly()
	for index := 0; index < count; index++ {
		got := <-channel
		if index%2 == 0 {
			if boxed, ok := got.(*box); !ok || boxed.value != index {
				println("interface element", index, "lost its pointer")
				panic("a buffered channel lost an interface element")
			}
			continue
		}
		if text, ok := got.(string); !ok || text != computed(index) {
			println("interface element", index, "lost its string")
			panic("a buffered channel lost an interface element")
		}
	}
}

//go:noinline
func aggregates(count int) {
	channel := make(chan pair, count)
	for index := 0; index < count; index++ {
		channel <- pair{name: computed(index), box: &box{value: index}}
	}
	collectRepeatedly()
	churn()
	collectRepeatedly()
	for index := 0; index < count; index++ {
		got := <-channel
		if got.name != computed(index) || got.box == nil || got.box.value != index {
			println("struct element", index, "lost a field")
			panic("a buffered channel lost a struct element")
		}
	}
}

//go:noinline
func slices(count int) {
	channel := make(chan []*box, count)
	for index := 0; index < count; index++ {
		channel <- []*box{{value: index}, {value: index + 1}}
	}
	collectRepeatedly()
	churn()
	collectRepeatedly()
	for index := 0; index < count; index++ {
		got := <-channel
		if len(got) != 2 || got[0].value != index || got[1].value != index+1 {
			println("slice element", index, "lost its backing array")
			panic("a buffered channel lost a slice element")
		}
	}
}

// scalars is the control. A channel of a pointer-free element type is the case
// that already worked, and it must keep working: its buffer is supposed to be
// carved out of the hchan's own no-scan allocation.
//
//go:noinline
func scalars(count int) {
	channel := make(chan int, count)
	for index := 0; index < count; index++ {
		channel <- index * 7
	}
	collectRepeatedly()
	churn()
	collectRepeatedly()
	for index := 0; index < count; index++ {
		if got := <-channel; got != index*7 {
			println("scalar element", index, "is", got)
			panic("a buffered channel lost a scalar element")
		}
	}
}

func main() {
	strings(16)
	pointers(16)
	interfaces(16)
	aggregates(16)
	slices(16)
	scalars(16)
	println("channel buffer roots ok")
}
