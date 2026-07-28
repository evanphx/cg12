// Interface and closure pointer write barriers.
//
// RUNTIME_PLAN.md section 6 lists both shapes. They belong together because
// both are aggregates whose pointer words are not written by ordinary field
// assignment:
//
//   - an interface value is two words. The first names the type or itab and the
//     second is the data pointer, and assigning one has to publish the data
//     word through a barrier. A direct-interface type stores the pointer itself
//     in the data word; an indirect one stores a pointer to a copy, so both are
//     covered here.
//   - a closure is a heap object whose first word is the code pointer and whose
//     remaining words are the captured variables. Storing a closure into a heap
//     or global slot publishes that object, and every captured pointer inside it
//     is a pointer word the collector has to find.
//
// The capability runs under GODEBUG=cg12checkwb=2. The semantic check is that
// the value each interface and each closure reaches is still intact after a
// round of collection with nothing else holding it.
package main

import (
	"runtime"
	"runtime/debug"
)

type payload struct {
	tag  int64
	next *payload
}

type namer interface {
	name() int64
}

// pointerNamer is a direct-interface type: the interface's data word is the
// pointer itself.
type pointerNamer struct {
	payload *payload
}

func (n *pointerNamer) name() int64 {
	return n.payload.tag
}

// valueNamer is an indirect-interface type, since it is larger than one word,
// so the interface's data word points at a copy of it.
type valueNamer struct {
	first  *payload
	second *payload
	tag    int64
}

func (n valueNamer) name() int64 {
	return n.tag + n.first.tag + n.second.tag
}

type interfaceHolder struct {
	named namer
	other namer
}

type closureHolder struct {
	callback func() int64
	fallback func() int64
}

const rounds = 300

var interfaceHolders []*interfaceHolder
var closureHolders []*closureHolder
var globalNamed namer
var globalCallback func() int64

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

func makeClosure(tag int64) func() int64 {
	captured := &payload{tag: tag, next: &payload{tag: tag + 1}}
	scalar := tag * 2
	return func() int64 {
		return captured.tag + captured.next.tag + scalar
	}
}

func main() {
	debug.SetGCPercent(1)
	done := make(chan struct{})
	go collectInBackground(done)

	for round := 0; round < rounds; round++ {
		tag := int64(round)

		holder := &interfaceHolder{}
		holder.named = &pointerNamer{payload: &payload{tag: tag}}
		holder.other = valueNamer{
			first:  &payload{tag: tag + 1},
			second: &payload{tag: tag + 2},
			tag:    tag + 3,
		}
		interfaceHolders = append(interfaceHolders, holder)
		globalNamed = holder.named

		closures := &closureHolder{}
		closures.callback = makeClosure(tag)
		closures.fallback = makeClosure(tag + 1000)
		closureHolders = append(closureHolders, closures)
		globalCallback = closures.callback
	}

	done <- struct{}{}
	<-done

	runtime.GC()
	runtime.GC()

	if len(interfaceHolders) != rounds || len(closureHolders) != rounds {
		panic("the holder lists have the wrong length")
	}
	for index, holder := range interfaceHolders {
		tag := int64(index)
		if holder.named == nil || holder.other == nil {
			panic("an interface field was cleared")
		}
		if holder.named.name() != tag {
			panic("a direct interface value lost its referent")
		}
		// tag+3 plus tag+1 plus tag+2.
		if holder.other.name() != 3*tag+6 {
			panic("an indirect interface value lost a referent")
		}
	}
	for index, closures := range closureHolders {
		tag := int64(index)
		if closures.callback == nil || closures.fallback == nil {
			panic("a closure field was cleared")
		}
		// captured.tag plus captured.next.tag plus scalar.
		if closures.callback() != tag+(tag+1)+tag*2 {
			panic("a closure lost a captured value")
		}
		fallbackTag := tag + 1000
		if closures.fallback() != fallbackTag+(fallbackTag+1)+fallbackTag*2 {
			panic("a closure lost a captured value")
		}
	}
	lastTag := int64(rounds - 1)
	if globalNamed == nil || globalNamed.name() != lastTag {
		panic("the global interface lost its referent")
	}
	if globalCallback == nil || globalCallback() != lastTag+(lastTag+1)+lastTag*2 {
		panic("the global closure lost a captured value")
	}
}
