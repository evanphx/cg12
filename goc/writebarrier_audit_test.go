package goc_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/goc"
)

// barrierShapesSource contains one function per write-barrier shape
// RUNTIME_PLAN.md section 6 lists, plus the two shapes that must NOT be
// barriered. Each function is small enough that the records attributed to it
// are unambiguous.
const barrierShapesSource = `
package main

import "runtime"

type node struct {
	tag  int64
	next *node
}

type aggregate struct {
	before int64
	node   *node
	after  int64
}

type namer interface {
	name() int64
}

func (n *node) name() int64 { return n.tag }

var globalNode *node
var globalAggregate aggregate
var globalInterface namer
var globalSlice []*node
var globalCallback func() int64

func heapToHeap(target *node, value *node) {
	target.next = value
}

func globalToHeap(value *node) {
	globalNode = value
}

func globalAggregateStore(value *node) {
	globalAggregate = aggregate{before: 1, node: value, after: 2}
}

func globalInterfaceStore(value *node) {
	globalInterface = value
}

func globalClosureStore(value *node) {
	globalCallback = func() int64 { return value.tag }
}

func sliceElementStore(elements []*node, value *node) {
	elements[0] = value
}

func sliceCopy(destination []*node, source []*node) {
	copy(destination, source)
}

func sliceClear(elements []*node) {
	clear(elements)
}

func sliceAppendFromSlice(destination []*node, source []*node) []*node {
	return append(destination, source...)
}

func mapStore(entries map[int64]*node, value *node) {
	entries[value.tag] = value
}

func channelSend(messages chan *node, value *node) {
	messages <- value
}

func scalarStore(target *node, value int64) {
	target.tag = value
}

func main() {
	runtime.GC()
	target := &node{}
	heapToHeap(target, &node{tag: 1})
	globalToHeap(target)
	globalAggregateStore(target)
	globalInterfaceStore(target)
	globalClosureStore(target)
	elements := make([]*node, 4)
	sliceElementStore(elements, target)
	sliceCopy(elements, elements)
	sliceClear(elements)
	globalSlice = sliceAppendFromSlice(elements, elements)
	mapStore(make(map[int64]*node), target)
	channelSend(make(chan *node, 1), target)
	scalarStore(target, 9)
}
`

func recordsForFunction(records []goc.WriteBarrierRecord, function string) []goc.WriteBarrierRecord {
	var matched []goc.WriteBarrierRecord
	for _, record := range records {
		if record.Function == function {
			matched = append(matched, record)
		}
	}
	return matched
}

func emittedReasons(records []goc.WriteBarrierRecord) []string {
	var reasons []string
	for _, record := range records {
		if record.Decision == goc.WriteBarrierEmitted {
			reasons = append(reasons, record.Reason)
		}
	}
	return reasons
}

// A missing write barrier is far worse than a spurious one: under concurrent
// marking the collector never learns about the stored pointer, so a live object
// is swept. Section 5.7 and section 5.8 both found the opposite direction, a
// barrier emitted where upstream emits none, and both were visible in the
// emitted IR. An omission is not: an elided barrier is an ordinary store.
//
// This test is the omission detector. It asks the frontend, through the opt-in
// audit, what it decided at each store shape, and requires a barrier at every
// shape that stores a pointer into something the mutator does not own
// exclusively.
//
// What it does and does not cover, stated plainly because the distinction
// matters: it sees every store that goes through gen.store, gen.storeInlineValue
// or the bulk builtins, which is where the frontend's barrier decisions are
// made. It cannot see a pointer store emitted somewhere else in the frontend as
// a bare ir.Block.Store, because such a site records nothing at all and is
// therefore indistinguishable from a shape this test simply does not list. The
// list below is the guard against that: adding a shape here is what makes the
// test cover it.
func TestEveryBarrierShapeEmitsABarrier(t *testing.T) {
	_, records, err := goc.CompileWithWriteBarrierAuditFor(goc.TargetARM64, "barriers.go", []byte(barrierShapesSource))
	require.NoError(t, err)
	require.NotEmpty(t, records)

	shapes := []struct {
		function string
		reason   string
	}{
		{"main.heapToHeap", goc.WriteBarrierReasonPointerStore},
		{"main.globalToHeap", goc.WriteBarrierReasonPointerStore},
		{"main.globalAggregateStore", goc.WriteBarrierReasonAggregateStore},
		{"main.globalInterfaceStore", goc.WriteBarrierReasonAggregateStore},
		{"main.globalClosureStore", goc.WriteBarrierReasonPointerStore},
		{"main.sliceElementStore", goc.WriteBarrierReasonPointerStore},
		{"main.sliceCopy", goc.WriteBarrierReasonTypedSliceCopy},
		{"main.sliceClear", goc.WriteBarrierReasonTypedMemClear},
		{"main.sliceAppendFromSlice", goc.WriteBarrierReasonTypedSliceCopy},
	}
	for _, shape := range shapes {
		matched := recordsForFunction(records, shape.function)
		require.NotEmpty(t, matched, "%s recorded no pointer store at all", shape.function)
		assert.Contains(t, emittedReasons(matched), shape.reason,
			"%s did not emit a write barrier; decisions were %v", shape.function, matched)
	}

	// A store of a scalar is not a pointer store and must record nothing, so a
	// record of it would mean the audit is counting the wrong thing and the
	// assertions above would be measuring noise.
	for _, record := range recordsForFunction(records, "main.scalarStore") {
		assert.NotEqual(t, "int64", record.ValueType,
			"a scalar store was recorded as a pointer store: %v", record)
	}
}

// The map and channel shapes split differently from the others, and the split
// is worth pinning down because it decides where their barrier has to be.
//
// A map assignment asks the map runtime for the element slot and then stores
// into that slot itself, so the store is a frontend store into a heap address
// and must carry a barrier -- the audit sees it. A channel send hands the value
// to runtime.chansend1, which copies it with typedmemmove, so the barrier is
// the runtime's and the audit sees no frontend store of the element.
func TestMapAndChannelStoresReachTheirBarrier(t *testing.T) {
	module, records, err := goc.CompileWithWriteBarrierAuditFor(goc.TargetARM64, "barriers.go", []byte(barrierShapesSource))
	require.NoError(t, err)

	mapStore := recordsForFunction(records, "main.mapStore")
	require.NotEmpty(t, mapStore, "a map assignment recorded no pointer store at all")
	assert.Contains(t, emittedReasons(mapStore), goc.WriteBarrierReasonPointerStore,
		"a map element was stored into the slot the map runtime returned without a barrier: %v", mapStore)

	channelSend := functionWithSuffix(t, module, "main.channelSend")
	assert.True(t, callsSymbol(channelSend, "runtime.chansend1"),
		"a channel send did not reach the channel runtime, so nothing barriers its element")
	for _, record := range recordsForFunction(records, "main.channelSend") {
		assert.NotEqual(t, goc.WriteBarrierReasonPointerStore, record.Reason,
			"a channel send stored its element through the frontend as well as the runtime: %v", record)
	}
}

// The counterpart to the omission test: barriers that must not be emitted.
// Section 5.7 removed the not-in-heap barrier because it was unsound, and a
// store into a frame slot needs none because the collector scans the frame.
// Without this, the omission test above could be satisfied by barriering every
// store.
func TestBarriersAreElidedWhereTheyMustBe(t *testing.T) {
	_, records, err := goc.CompileWithWriteBarrierAuditFor(goc.TargetARM64, "elide.go", []byte(`
package main

import "runtime"

type node struct {
	tag  int64
	next *node
}

func frameLocalStore() int64 {
	var slot *node
	slot = &node{tag: 5}
	return slot.tag
}

func main() {
	runtime.GC()
	if frameLocalStore() != 5 {
		panic("frame local store mismatch")
	}
}
`))
	require.NoError(t, err)

	frameLocal := recordsForFunction(records, "main.frameLocalStore")
	require.NotEmpty(t, frameLocal)
	for _, record := range frameLocal {
		assert.Equal(t, goc.WriteBarrierElided, record.Decision,
			"a store into a frame slot was barriered: %v", record)
		assert.Equal(t, goc.WriteBarrierReasonStackDestination, record.Reason)
	}

	// The runtime's not-in-heap stores must stay unbarriered; section 5.7's
	// failure was exactly one of these reaching goc_storep. runtime.heapBitsSlice
	// is the site that reduced that bug.
	notInHeap := 0
	for _, record := range records {
		if record.Reason == goc.WriteBarrierReasonNotInHeap {
			notInHeap++
			assert.Equal(t, goc.WriteBarrierElided, record.Decision)
		}
	}
	assert.NotZero(t, notInHeap, "no not-in-heap pointer store was recorded, so the elision is untested")
}
