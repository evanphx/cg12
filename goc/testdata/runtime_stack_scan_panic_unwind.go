// A stack that is in the middle of a panic must still describe its roots.
//
// While a panic runs, the frames below the deferred function have not been
// popped: gopanic walks the defer chain and calls each deferred function on top
// of the panicking frames, and a collection during that window scans the whole
// tower. The frames underneath are stopped at their call to the function that
// panicked, so their stack maps are read at a PC that ordinary control flow
// never returns to, and the deferred function's own frame sits above a
// runtime.gopanic frame rather than above its caller.
//
// Each level below keeps its objects only in its own frame, the deferred
// function collects several times before recovering, and every level checks its
// objects after the recovery -- so a root lost during the unwind is observed by
// the frame that owned it, not inferred.
//
// The shapes covered are a deep tower of pointer-bearing frames, a defer that
// itself panics and is recovered one level up, a panic carrying a pointer as its
// value, and a recover that returns normally so the frames below resume.
package main

import "runtime"

type carried struct {
	value int
	label string
	inner *carried
}

//go:noinline
func makeCarried(seed int) *carried {
	return &carried{
		value: seed,
		label: "carried-" + string(rune('a'+seed%26)),
		inner: &carried{value: seed * 9, label: "inner"},
	}
}

//go:noinline
func verify(where string, object *carried, seed int) {
	want := "carried-" + string(rune('a'+seed%26))
	if object == nil || object.value != seed || object.label != want {
		println("in", where, "at depth", seed)
		panic("a frame under an in-flight panic lost a root")
	}
	if object.inner == nil || object.inner.value != seed*9 || object.inner.label != "inner" {
		println("in", where, "inner at depth", seed)
		panic("a frame under an in-flight panic lost an indirect root")
	}
}

//go:noinline
func churn() {
	var sink []*carried
	for index := 0; index < 1024; index++ {
		sink = append(sink, &carried{value: index, label: "churn"})
	}
	if len(sink) != 1024 {
		panic("churn lost its slice")
	}
}

//go:noinline
func collectDuringUnwind() {
	for cycle := 0; cycle < 3; cycle++ {
		runtime.GC()
		churn()
		runtime.GC()
	}
}

// tower recurses, keeping one object live per frame, and panics at the bottom.
// Every frame verifies its own object in a deferred function, which runs while
// the panic is still in flight.
//
//go:noinline
func tower(depth int) {
	object := makeCarried(depth)
	defer func() {
		verify("tower defer", object, depth)
	}()
	if depth == 0 {
		collectDuringUnwind()
		panic(makeCarried(100))
	}
	tower(depth - 1)
	verify("tower return", object, depth)
}

//go:noinline
func panicValueSurvives() {
	defer func() {
		recovered := recover()
		collectDuringUnwind()
		object, ok := recovered.(*carried)
		if !ok {
			panic("the panic value changed type")
		}
		verify("panic value", object, 100)
	}()
	tower(8)
}

// deferPanics replaces the in-flight panic with a new one from inside a deferred
// function, so the second panic unwinds a stack that already has a gopanic frame
// on it.
//
//go:noinline
func deferPanics() {
	object := makeCarried(21)
	defer func() {
		recovered := recover()
		collectDuringUnwind()
		if recovered != "first" {
			println("recovered", recovered)
			panic("the wrong panic reached the outer recover")
		}
		verify("outer recover", object, 21)
	}()
	inner := makeCarried(22)
	defer func() {
		collectDuringUnwind()
		verify("replacing defer", inner, 22)
		_ = recover()
		panic("first")
	}()
	panic("second")
}

// resumed recovers and returns normally, so the frames the panic did not reach
// keep running afterwards with the roots they had before it started.
//
//go:noinline
func resumed() int {
	object := makeCarried(31)
	total := func() (result int) {
		defer func() {
			if recover() != nil {
				collectDuringUnwind()
				result = 7
			}
		}()
		inner := makeCarried(32)
		collectDuringUnwind()
		verify("resumed inner before", inner, 32)
		panic("stop")
	}()
	collectDuringUnwind()
	verify("resumed after", object, 31)
	return total + object.value
}

func main() {
	panicValueSurvives()
	deferPanics()
	if got := resumed(); got != 38 {
		println("resumed", got)
		panic("the recovered value was wrong")
	}
	println("panic unwind roots ok")
}
