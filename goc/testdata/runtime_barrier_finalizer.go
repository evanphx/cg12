// Finalizer and cleanup pointer write barriers.
//
// RUNTIME_PLAN.md section 6 lists the finalizer barrier shape. It is different
// from the others because the stores are made by the runtime on behalf of the
// program: runtime.SetFinalizer records the object, the finalizer function and
// its argument type in a special record attached to the object's span, and
// runtime.AddCleanup does the same for a cleanup and its argument. Those
// records are heap objects the collector must find, and the queue that carries
// a ready finalizer to the finalizer goroutine is another one.
//
// The barrier-relevant part is that the argument a cleanup captures, and the
// object a finalizer resurrects, are published from runtime code into
// runtime-owned storage while the collector may already be marking. This
// capability runs under GODEBUG=cg12checkwb=2 with a collection in flight, so
// any word those paths buffer is validated.
//
// The asserted semantics:
//
//   - every finalizer and every cleanup registered on an unreachable object
//     eventually runs, and each runs exactly once;
//   - a finalizer that resurrects its object gets an intact object, and the
//     object stays intact afterwards;
//   - a cleanup's argument survives until the cleanup runs, and the value it
//     carries is the one that was registered with it.
package main

import (
	"runtime"
	"runtime/debug"
	"sync"
)

type finalized struct {
	tag     int64
	payload [32]byte
	inner   *note
}

type note struct {
	tag int64
}

const objects = 256

var mutex sync.Mutex
var finalizerTags []int64
var cleanupTags []int64
var resurrected []*finalized

func fill(object *finalized, tag int64) {
	object.tag = tag
	object.inner = &note{tag: tag + 1}
	for index := range object.payload {
		object.payload[index] = byte(tag) + byte(index)
	}
}

func check(object *finalized, tag int64) {
	if object.tag != tag {
		panic("a finalized object lost its tag")
	}
	if object.inner == nil || object.inner.tag != tag+1 {
		panic("a finalized object lost its referent")
	}
	for index := range object.payload {
		if object.payload[index] != byte(tag)+byte(index) {
			panic("a finalized object lost its payload")
		}
	}
}

func registerFinalized(tag int64) {
	object := &finalized{}
	fill(object, tag)
	runtime.SetFinalizer(object, func(dying *finalized) {
		check(dying, dying.tag)
		mutex.Lock()
		finalizerTags = append(finalizerTags, dying.tag)
		if dying.tag%64 == 0 {
			// Resurrection: publish the dying object into a live global from
			// inside the finalizer, which is a heap store made while the
			// collector has already decided the object was unreachable.
			resurrected = append(resurrected, dying)
		}
		mutex.Unlock()
	})
}

func registerCleanup(tag int64) {
	object := &finalized{}
	fill(object, tag)
	carried := &note{tag: tag + 2}
	runtime.AddCleanup(object, func(argument *note) {
		if argument == nil || argument.tag != tag+2 {
			panic("a cleanup argument lost its value")
		}
		mutex.Lock()
		cleanupTags = append(cleanupTags, argument.tag-2)
		mutex.Unlock()
	}, carried)
}

func collectUntil(reached func() bool) bool {
	for attempt := 0; attempt < 200; attempt++ {
		runtime.GC()
		runtime.Gosched()
		mutex.Lock()
		done := reached()
		mutex.Unlock()
		if done {
			return true
		}
	}
	return false
}

func main() {
	debug.SetGCPercent(1)

	for index := 0; index < objects; index++ {
		registerFinalized(int64(index))
		registerCleanup(int64(index) + 100000)
	}

	if !collectUntil(func() bool { return len(finalizerTags) == objects }) {
		panic("not every finalizer ran")
	}
	if !collectUntil(func() bool { return len(cleanupTags) == objects }) {
		panic("not every cleanup ran")
	}

	runtime.GC()
	runtime.GC()

	mutex.Lock()
	defer mutex.Unlock()

	seenFinalizer := make(map[int64]bool, objects)
	for _, tag := range finalizerTags {
		if seenFinalizer[tag] {
			panic("a finalizer ran twice")
		}
		seenFinalizer[tag] = true
	}
	for index := 0; index < objects; index++ {
		if !seenFinalizer[int64(index)] {
			panic("a finalizer did not run")
		}
	}

	seenCleanup := make(map[int64]bool, objects)
	for _, tag := range cleanupTags {
		if seenCleanup[tag] {
			panic("a cleanup ran twice")
		}
		seenCleanup[tag] = true
	}
	for index := 0; index < objects; index++ {
		if !seenCleanup[int64(index)+100000] {
			panic("a cleanup did not run")
		}
	}

	if len(resurrected) != objects/64 {
		panic("the resurrected object list has the wrong length")
	}
	for _, object := range resurrected {
		if object == nil {
			panic("a resurrected object became nil")
		}
		check(object, object.tag)
		if object.tag%64 != 0 {
			panic("an object that should not have been resurrected was")
		}
	}
}
