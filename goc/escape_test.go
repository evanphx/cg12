package goc_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeAllocationEscapeLowering(t *testing.T) {
	module, err := goc.Compile("escape.go", []byte(`
package main

import (
	"runtime"
	"unsafe"
)

type pair struct {
	left  int
	right int
}

func localPair() int {
	value := new(pair)
	value.left = 7
	value.right = 11
	return value.left * value.right
}

func escapingPair() *pair {
	return new(pair)
}

type pointerHolder struct {
	pointer unsafe.Pointer
}

func escapingSliceBacking(holder *pointerHolder) {
	directory := make([]*int, 1)
	holder.pointer = unsafe.Pointer(&directory[0])
}

func localSliceBacking() int {
	values := make([]int, 1)
	pointer := &values[0]
	*pointer = 42
	return values[0]
}

func Test() int {
	runtime.GC()
	return localPair() + escapingPair().left + localSliceBacking()
}
`))
	require.NoError(t, err)

	local := functionWithSuffix(t, module, "localPair")
	escaping := functionWithSuffix(t, module, "escapingPair")
	assert.True(t, containsInstruction(local, func(instruction ir.Instr) bool {
		return instruction.Op.IsAlloc()
	}), "local allocation was not promoted to the stack")
	assert.False(t, callsSymbol(local, "runtime.newobject"))
	assert.True(t, callsSymbol(escaping, "runtime.newobject"), "escaping allocation did not remain on the heap")

	escapingSlice := functionWithSuffix(t, module, "escapingSliceBacking")
	localSlice := functionWithSuffix(t, module, "localSliceBacking")
	assert.True(t, callsSymbol(escapingSlice, "runtime.newobject"), "escaping slice backing did not remain on the heap")
	assert.True(t, containsInstruction(localSlice, func(instruction ir.Instr) bool {
		return instruction.Op.IsAlloc()
	}), "local slice backing was not allocated on the stack")
	assert.False(t, callsSymbol(localSlice, "runtime.newobject"))
}

func TestAddressNestedInReturnedCompositeEscapes(t *testing.T) {
	module, err := goc.Compile("escape.go", []byte(`
package main

import "runtime"

type holder struct {
	pointer *int
}

func escapingHolder() holder {
	value := 42
	return holder{pointer: &value}
}

func useRuntime() {
	runtime.GC()
}
`))
	require.NoError(t, err)

	escaping := functionWithSuffix(t, module, "escapingHolder")
	assert.True(t, callsSymbol(escaping, "runtime.newobject"), "nested address did not escape to the heap")
}

func TestUnsafePointerConvertedToUintptrDoesNotEscapeLocalArray(t *testing.T) {
	module, err := goc.Compile("escape.go", []byte(`
package main

import (
	"runtime"
	"unsafe"
)

func consume(value uintptr) {
}

func pass(values []uintptr) {
	pointer := unsafe.Pointer(&values[0])
	consume(uintptr(pointer))
}

func localArray() {
	var values [128]uintptr
	pass(values[:])
}

func Test() {
	runtime.GC()
	localArray()
}
`))
	require.NoError(t, err)

	local := functionWithSuffix(t, module, "localArray")
	assert.False(t, callsSymbol(local, "runtime.newobject"), "uintptr-only syscall storage escaped to the heap")
}

func TestPointerConversionPassedToNoEscapeFunctionStaysLocal(t *testing.T) {
	module, err := goc.Compile("noescape_conversion.go", []byte(`
package main

import (
	"runtime"
	"unsafe"
)

//go:noescape
func hide(pointer unsafe.Pointer) unsafe.Pointer

type record struct {
	value int
}

func (record *record) touch() {
	_ = hide(unsafe.Pointer(record))
}

func localRecord() {
	var record record
	record.touch()
}

func Test() {
	runtime.GC()
	localRecord()
}
`))
	require.NoError(t, err)

	local := functionWithSuffix(t, module, "localRecord")
	assert.False(t, callsSymbol(local, "runtime.newobject"), "noescape pointer conversion moved local storage to the heap")
}

func TestRuntimeAggregateStorePublishesEveryPointerWord(t *testing.T) {
	module, err := goc.CompileExecutable("aggregate_store.go", []byte(`
package main

import "runtime"

type item struct {
	value int
}

type holder struct {
	name     string
	pointer  *item
	value    any
	callback func() int
}

func assign(destination *holder, source holder) {
	*destination = source
}

func Test() {
	runtime.GC()
	assign(new(holder), holder{})
}

func main() {
	Test()
}
`))
	require.NoError(t, err)

	assign := functionWithSuffix(t, module, "main.assign")
	assert.GreaterOrEqual(t, countCallsSymbol(assign, "goc_storep"), 5)
}

func TestRuntimeSliceBulkOperationsUseWriteBarriers(t *testing.T) {
	module, err := goc.Compile("slice_bulk.go", []byte(`
package main

import "runtime"

type item struct {
	value int
}

func copyPointers(destination, source []*item) int {
	return copy(destination, source)
}

func appendPointers(destination, source []*item) []*item {
	return append(destination, source...)
}

func clearPointers(values []*item) {
	clear(values)
}

func copyBytes(destination, source []byte) int {
	return copy(destination, source)
}

func useRuntime() {
	runtime.GC()
}
`))
	require.NoError(t, err)

	pointerCopy := functionWithSuffix(t, module, "copyPointers")
	assert.True(t, callsSymbol(pointerCopy, "runtime.typedslicecopy"))
	assert.False(t, callsSymbol(pointerCopy, "goc_memmove"))

	pointerAppend := functionWithSuffix(t, module, "appendPointers")
	assert.True(t, callsSymbol(pointerAppend, "runtime.typedslicecopy"))

	pointerClear := functionWithSuffix(t, module, "clearPointers")
	assert.True(t, callsSymbol(pointerClear, "runtime.memclrHasPointers"))

	byteCopy := functionWithSuffix(t, module, "copyBytes")
	assert.True(t, callsSymbol(byteCopy, "goc_memmove"))
	assert.False(t, callsSymbol(byteCopy, "runtime.typedslicecopy"))
}

func TestHiddenResultPointersAreManagedStackReferences(t *testing.T) {
	module, err := goc.Compile("results.go", []byte(`
package main

import "runtime"

func multipleResults() (int, *int, bool) {
	value := 42
	return value, &value, true
}

func useRuntime() {
	runtime.GC()
}
`))
	require.NoError(t, err)

	function := functionWithSuffix(t, module, "multipleResults")
	managedResults := 0
	for _, parameter := range function.Params {
		if strings.HasPrefix(parameter.Name, "result") && parameter.GCRef {
			managedResults++
		}
	}
	assert.Equal(t, 2, managedResults)
}

func TestFunctionLiteralForwardedToGoroutineEscapes(t *testing.T) {
	module, err := goc.Compile("goroutine_closure.go", []byte(`
package main

import "runtime"

var result int

func invoke(callback func()) {
	callback()
}

func launch(callback func()) {
	go invoke(callback)
}

func install() {
	value := 42
	launch(func() {
		result = value
	})
}

func Test() {
	runtime.GC()
	install()
}
`))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	assert.True(t, callsSymbol(install, "runtime.newobject"), "goroutine closure remained on the caller stack")
}

// A defer that runs at most once per frame is frame-scoped: deferreturn calls it
// before the frame is torn down, so its descriptor is a frame slot and it
// captures by reference. Heap-lifting it instead is what made
// runtime.gcAssistAlloc allocate (RUNTIME_PLAN.md 5.2.1).
func TestSingleDirectDeferredFunctionLiteralStaysOnTheFrame(t *testing.T) {
	module, err := goc.Compile("deferred_closure.go", []byte(`
package main

import "runtime"

var result int

func install() {
	value := 42
	defer func() {
		result = value
	}()
	runtime.GC()
}

func Test() {
	install()
}
`))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	assert.False(t, callsSymbol(install, "runtime.newobject"), "a single deferred closure was heap-lifted")
}

func TestDeferredClosureCapturedNamedResultStaysOnTheFrame(t *testing.T) {
	module, err := goc.Compile("deferred_named_result.go", []byte(`
package main

import "runtime"

func compute() (result int) {
	defer func() {
		result++
	}()
	runtime.GC()
	return 41
}

func Test() {
	if compute() != 42 {
		panic("bad result")
	}
}
`))
	require.NoError(t, err)

	compute := functionWithSuffix(t, module, "compute")
	newObjectCalls := countCallsSymbol(compute, "runtime.newobject")
	assert.Equal(t, 0, newObjectCalls, "a single deferred closure and its captured result were heap-lifted")
}

// The shape of runtime.gcAssistAlloc's synctest block: the captured variable is
// declared by an if-statement's init, which runs unconditionally, but the closure
// is only built when the branch is taken. Nothing may be allocated ahead of the
// test, or every call to gcAssistAlloc allocates and the assist path recurses.
func TestConditionallyDeferredFunctionLiteralDoesNotAllocateBeforeItsBranch(t *testing.T) {
	module, err := goc.Compile("conditional_deferred_closure.go", []byte(`
package main

import "runtime"

type bubble struct {
	value int
}

type goroutine struct {
	bubble *bubble
}

var current goroutine

func currentGoroutine() *goroutine {
	return &current
}

func assist() {
	if g := currentGoroutine(); g.bubble != nil {
		saved := g.bubble
		g.bubble = nil
		defer func() {
			g.bubble = saved
		}()
	}
	runtime.GC()
}

func Test() {
	assist()
}
`))
	require.NoError(t, err)

	assist := functionWithSuffix(t, module, "assist")
	assert.Equal(t, 0, countCallsSymbol(assist, "runtime.newobject"), "the conditional deferred capture was heap-lifted")
}

// A defer that can run more than once must keep its heap descriptor: the frame
// holds one slot per defer statement, so a reused stack descriptor would leave
// every registration sharing the last iteration's captures.
func TestRepeatedDeferredFunctionLiteralEscapes(t *testing.T) {
	sources := map[string]string{
		"loop body": `
package main

import "runtime"

var total int

func install() {
	for index := 0; index < 3; index++ {
		value := index
		defer func() {
			total += value
		}()
	}
	runtime.GC()
}

func Test() {
	install()
}
`,
		"backward goto": `
package main

import "runtime"

var total int

func install() {
	index := 0
retry:
	value := index
	defer func() {
		total += value
	}()
	index++
	if index < 3 {
		goto retry
	}
	runtime.GC()
}

func Test() {
	install()
}
`,
	}

	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			module, err := goc.Compile("repeated_deferred_closure.go", []byte(source))
			require.NoError(t, err)

			install := functionWithSuffix(t, module, "install")
			assert.True(t, callsSymbol(install, "runtime.newobject"), "a repeatable deferred closure stayed on the frame")
		})
	}
}

// A goto that only ever jumps forward cannot re-reach the defer, so the defer
// still runs at most once. runtime.gcAssistAlloc depends on this: its synctest
// defer sits above its own retry label.
func TestDeferredFunctionLiteralAboveAForwardGotoStaysOnTheFrame(t *testing.T) {
	module, err := goc.Compile("forward_goto_deferred_closure.go", []byte(`
package main

import "runtime"

var total int

func install(skip bool) {
	value := 1
	defer func() {
		total += value
	}()
	if skip {
		goto done
	}
	value = 2
done:
	runtime.GC()
}

func Test() {
	install(false)
}
`))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	assert.False(t, callsSymbol(install, "runtime.newobject"), "a deferred closure above a forward goto was heap-lifted")
}

func TestFunctionLiteralPassedToEscapingParameterEscapes(t *testing.T) {
	module, err := goc.Compile("escaping_parameter_closure.go", []byte(`
package main

import "runtime"

var saved func()

func save(callback func()) {
	saved = callback
}

func install(value *int) {
	save(func() {
		*value = 42
	})
}

func Test() {
	result := 0
	install(&result)
	runtime.GC()
	saved()
}
`))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	assert.True(t, callsSymbol(install, "runtime.newobject"), "closure passed to escaping parameter remained on the caller stack")
}

func TestSynchronousFunctionLiteralCaptureDoesNotPromoteStackSliceHeader(t *testing.T) {
	module, err := goc.Compile("sync_closure_slice.go", []byte(`
package main

import "runtime"

var result int

func consume(values []int) {
	result += len(values)
}

func invoke(callback func()) {
	callback()
}

func localSliceHeader() {
	var backing [4]int
	values := backing[:2:2]
	invoke(func() {
		consume(values)
	})
}

func Test() {
	runtime.GC()
	localSliceHeader()
}
`))
	require.NoError(t, err)

	local := functionWithSuffix(t, module, "localSliceHeader")
	assert.False(t, callsSymbol(local, "runtime.newobject"), "synchronous closure promoted a stack slice header to the heap")
}

func TestRuntimeSelectShapedSynchronousCaptureDoesNotPromoteStackSliceHeaders(t *testing.T) {
	module, err := goc.Compile("select_shaped_closure.go", []byte(`
package main

import (
	"runtime"
	"unsafe"
)

type scase struct {
	channel *int
	elem    *int
}

func unlock(cases []scase, order []uint16) {
	var item *scase
	for _, index := range order {
		item = &cases[index]
		if item.channel != nil {
			item.elem = nil
		}
	}
}

func recv(unlockf func()) {
	unlockf()
}

func localSelectShape(cas0 *scase, order0 *uint16, ncases int) {
	cas1 := (*[1 << 16]scase)(unsafe.Pointer(cas0))
	order1 := (*[1 << 17]uint16)(unsafe.Pointer(order0))
	cases := cas1[:ncases:ncases]
	order := order1[ncases:][:ncases:ncases]
	order = order[:1]
	recv(func() {
		unlock(cases, order)
	})
}

func Test() {
	runtime.GC()
}
`))
	require.NoError(t, err)

	local := functionWithSuffix(t, module, "localSelectShape")
	assert.False(t, callsSymbol(local, "runtime.newobject"), "select-shaped synchronous closure promoted stack slice headers to the heap")
}

func TestFunctionAssignedGlobalSliceLiteralUsesEscapingBacking(t *testing.T) {
	module, err := goc.Compile("global_slice_literal.go", []byte(`
package main

import "runtime"

type option struct {
	name    string
	enabled *bool
}

var aes bool
var sha bool
var options []option

func initializeOptions() {
	options = []option{
		{name: "aes", enabled: &aes},
		{name: "sha", enabled: &sha},
	}
}

func Test() {
	runtime.GC()
	initializeOptions()
}
`))
	require.NoError(t, err)

	initialize := functionWithSuffix(t, module, "initializeOptions")
	assert.True(t, callsSymbol(initialize, "runtime.newobject"), "global slice literal backing was not promoted out of the init stack")
}

// The runtime's print routines slice a local scratch array and hand it to a
// callee that does not retain it. They must not allocate: they run during mark
// termination and on fatal paths where mallocgc throws. This mirrors the shape
// of runtime.printuint feeding gwrite(buf[i:]).
func TestLocalArraySlicedIntoNonRetainingCalleeStaysOnStack(t *testing.T) {
	module, err := goc.Compile("sliced_scratch.go", []byte(`
package main

import "runtime"

func consume(text []byte) int {
	if len(text) == 0 {
		return 0
	}
	return int(text[0]) + len(text)
}

func format(value uint64) {
	var buf [20]byte
	index := len(buf)
	for {
		index--
		buf[index] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	_ = consume(buf[index:])
}

func Test() {
	runtime.GC()
	format(1234)
}
`))
	require.NoError(t, err)

	formatFunc := functionWithSuffix(t, module, "format")
	assert.False(t, callsSymbol(formatFunc, "runtime.newobject"),
		"scratch array sliced into a non-retaining callee was promoted to the heap")
}

// The counterpart: when the derived slice genuinely outlives the frame, the
// backing array must still be promoted. Guards against over-relaxing the
// slice-expression escape rule.
func TestLocalArraySlicedIntoReturnedSliceEscapes(t *testing.T) {
	module, err := goc.Compile("returned_scratch.go", []byte(`
package main

import "runtime"

func leak() []byte {
	var buf [20]byte
	return buf[:]
}

var sink []byte

func Test() {
	runtime.GC()
	sink = leak()
}
`))
	require.NoError(t, err)

	leakFunc := functionWithSuffix(t, module, "leak")
	assert.True(t, callsSymbol(leakFunc, "runtime.newobject"),
		"returned slice of a local array did not force the backing onto the heap")
}

func TestZeroCaptureFunctionLiteralUsesStaticDescriptor(t *testing.T) {
	module, err := goc.Compile("closure.go", []byte(`
package main

var callback func() int

func install() {
	callback = func() int { return 7 }
}
`))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	assert.False(t, callsSymbol(install, "runtime.newobject"))

	foundDescriptor := false
	for _, data := range module.Data {
		if len(data.Items) != 1 || data.Items[0].Sym == "" {
			continue
		}
		if strings.Contains(data.Items[0].Sym, ".func.") {
			foundDescriptor = true
			break
		}
	}
	assert.True(t, foundDescriptor, "zero-capture function literal has no static descriptor")
}

func functionWithSuffix(t *testing.T, module *ir.Module, suffix string) *ir.Func {
	t.Helper()
	for _, function := range module.Funcs {
		if strings.HasSuffix(function.Name, suffix) {
			return function
		}
	}
	t.Fatalf("function with suffix %q not found", suffix)
	return nil
}

func containsInstruction(function *ir.Func, match func(ir.Instr) bool) bool {
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if match(instruction) {
				return true
			}
		}
	}
	return false
}

func callsSymbol(function *ir.Func, symbol string) bool {
	return countCallsSymbol(function, symbol) != 0
}

func countCallsSymbol(function *ir.Func, symbol string) int {
	count := 0
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
				continue
			}
			callee := instruction.Args[0]
			if callee.Kind != ir.RefConst {
				continue
			}
			constant := function.Consts[callee.ID]
			if constant.Kind == ir.ConstSym && constant.Sym == symbol {
				count++
			}
		}
	}
	return count
}

// unsafe.Pointer is a pointer to the collector, so storing one into the heap
// needs a write barrier exactly as an ordinary pointer does. cg12 chose the
// store width before it chose the barrier, and its width table accepts
// unsafe.Pointer alongside the integer kinds, so every unsafe.Pointer store took
// the width path and lost its barrier. runtime.gostartcall's `buf.ctxt = ctxt`
// is one of them: it publishes a goroutine's funcval into a g the collector may
// already have blackened. RUNTIME_PLAN.md 5.12.
func TestUnsafePointerStoreKeepsWriteBarrier(t *testing.T) {
	module, err := goc.CompileExecutable("unsafe_pointer_store.go", []byte(`
package main

import (
	"runtime"
	"unsafe"
)

type cell struct {
	value int
}

type holder struct {
	word     uintptr
	opaque   unsafe.Pointer
	ordinary *cell
}

func storeOpaque(destination *holder, source unsafe.Pointer) {
	destination.opaque = source
}

func storeOrdinary(destination *holder, source *cell) {
	destination.ordinary = source
}

func storeWord(destination *holder, source uintptr) {
	destination.word = source
}

func Test() {
	runtime.GC()
	storeOpaque(new(holder), nil)
	storeOrdinary(new(holder), nil)
	storeWord(new(holder), 0)
}

func main() {
	Test()
}
`))
	require.NoError(t, err)

	storeOpaque := functionWithSuffix(t, module, "main.storeOpaque")
	assert.GreaterOrEqual(t, countCallsSymbol(storeOpaque, "goc_storep"), 1, "unsafe.Pointer store lost its write barrier")

	storeOrdinary := functionWithSuffix(t, module, "main.storeOrdinary")
	assert.GreaterOrEqual(t, countCallsSymbol(storeOrdinary, "goc_storep"), 1, "ordinary pointer store lost its write barrier")

	// A uintptr field holds no pointer, so it must stay barrier-free: the fix is
	// about which types the collector tracks, not about barriering every word.
	storeWord := functionWithSuffix(t, module, "main.storeWord")
	assert.Equal(t, 0, countCallsSymbol(storeWord, "goc_storep"), "uintptr store went through the write barrier")
}

func TestNotInHeapPointerStoreSkipsWriteBarrier(t *testing.T) {
	module, err := goc.CompileExecutable("not_in_heap_store.go", []byte(`
package main

import (
	"internal/runtime/sys"
	"runtime"
)

type metadata struct {
	_    sys.NotInHeap
	word uintptr
}

type item struct {
	value int
}

type mixedHolder struct {
	meta  *metadata
	thing *item
}

type plainHolder struct {
	other *item
	thing *item
}

func storeMetadata(destination *mixedHolder, source *metadata) {
	destination.meta = source
}

func storeItem(destination *mixedHolder, source *item) {
	destination.thing = source
}

func storeMixed(destination *mixedHolder, source mixedHolder) {
	*destination = source
}

func storePlain(destination *plainHolder, source plainHolder) {
	*destination = source
}

func Test() {
	runtime.GC()
	storeMetadata(new(mixedHolder), nil)
	storeItem(new(mixedHolder), nil)
	storeMixed(new(mixedHolder), mixedHolder{})
	storePlain(new(plainHolder), plainHolder{})
}

func main() {
	Test()
}
`))
	require.NoError(t, err)

	// internal/runtime/sys.NotInHeap promises that a pointer to the type never
	// refers to a collected object, so storing one must not reach the write
	// barrier. cg12's barrier records the destination slot's previous contents
	// as a deleted pointer, so barriering these stores publishes addresses the
	// marker cannot interpret -- the failure behind RUNTIME_PLAN.md 5.7.
	storeMetadata := functionWithSuffix(t, module, "main.storeMetadata")
	assert.Equal(t, 0, countCallsSymbol(storeMetadata, "goc_storep"), "not-in-heap pointer store went through the write barrier")

	storeItem := functionWithSuffix(t, module, "main.storeItem")
	assert.GreaterOrEqual(t, countCallsSymbol(storeItem, "goc_storep"), 1, "ordinary pointer store lost its write barrier")

	// The two holders have the same shape and are copied the same number of
	// times, so the only difference is that one of mixedHolder's two pointer
	// words is not-in-heap. Comparing the counts states that exactly that word
	// dropped out of the barrier, without depending on how many intermediate
	// copies cg12 emits for an aggregate assignment.
	storeMixed := functionWithSuffix(t, module, "main.storeMixed")
	storePlain := functionWithSuffix(t, module, "main.storePlain")
	plainBarriers := countCallsSymbol(storePlain, "goc_storep")
	assert.NotZero(t, plainBarriers, "aggregate store of ordinary pointers lost its write barriers")
	assert.Equal(t, plainBarriers, 2*countCallsSymbol(storeMixed, "goc_storep"), "aggregate store barriered the not-in-heap pointer word")
}

func TestKeepAliveStoresIntoAFrameSlotRatherThanAGlobal(t *testing.T) {
	module, err := goc.CompileExecutable("keepalive_slot.go", []byte(`
package main

import "runtime"

type keptItem struct {
	index int
	next  *keptItem
}

func keepStackItem(index int) int {
	root := &keptItem{
		index: index,
		next:  &keptItem{index: index + 1},
	}
	runtime.GC()
	total := root.index + root.next.index
	runtime.KeepAlive(root)
	return total
}

func main() {
	if keepStackItem(1) != 3 {
		panic("keepalive total mismatch")
	}
}
`))
	require.NoError(t, err)

	// cg12 roots a kept-alive value in a slot of its own, because the value's
	// last ordinary use can precede the runtime.KeepAlive call. That slot must
	// live in the frame. A global would be shared by every goroutine running
	// the function, and -- since escape analysis leaves root on the stack --
	// would publish a goroutine stack address to a permanent GC root that no
	// stack copy relocates and no goroutine exit clears. That is the stale root
	// behind "found bad pointer in Go heap"; see RUNTIME_PLAN.md 5.8.
	for _, data := range module.Data {
		assert.False(t, strings.HasPrefix(data.Name, ".goc.keepalive."),
			"runtime.KeepAlive still uses a global slot: %s", data.Name)
	}

	keepStackItem := functionWithSuffix(t, module, "main.keepStackItem")
	assert.True(t, keepAliveSlotIsAFramePointerWord(keepStackItem),
		"runtime.KeepAlive did not root its value in a frame pointer word")
}

// keepAliveSlotIsAFramePointerWord reports whether the function allocates a
// stack slot that is registered as holding a pointer word and is written by a
// plain store. That is the shape of a keep-alive slot: a frame allocation the
// stack maps describe, so the collector scans it and stack copying relocates
// it, and a direct store rather than a write barrier, because the destination
// is on the stack.
func keepAliveSlotIsAFramePointerWord(function *ir.Func) bool {
	pointerAllocations := make(map[uint32]bool)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if !instruction.Op.IsAlloc() || instruction.To.Kind != ir.RefTemp {
				continue
			}
			if function.StackPointerWords[instruction.To.ID][0] {
				pointerAllocations[instruction.To.ID] = true
			}
		}
	}

	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if !instruction.Op.IsStore() || len(instruction.Args) < 2 {
				continue
			}
			destination := instruction.Args[1]
			if destination.Kind == ir.RefTemp && pointerAllocations[destination.ID] {
				return true
			}
		}
	}
	return false
}

// A parameter whose only escaping use is the function's own result does not
// force its caller's storage onto the heap; the caller's storage escapes
// exactly when the call's result does. Without that summary the four float and
// complex print routines heap-allocated their scratch arrays, which is the
// RUNTIME_PLAN.md 5.3 defect on paths where allocation is forbidden --
// gwrite(strconv.AppendFloat(buf[:0], ...)) passes a derived slice to a callee
// that returns a slice derived from it.
func TestParameterLeakingOnlyToResultDoesNotEscapeItsCaller(t *testing.T) {
	module, err := goc.Compile("leak_to_result.go", []byte(`
package main

import "runtime"

var sink []byte
var pointerSink *int
var slicePointerSink []*int

func passthrough(dst []byte) []byte { return dst }

func retain(dst []byte) []byte {
	sink = dst
	return dst
}

func namedRetain(dst []byte) (out []byte) {
	out = dst
	sink = dst
	return
}

func identity(value *int) *int { return value }

func consume(b []byte) {}

func resultConsumedLocally() {
	var buf [20]byte
	consume(passthrough(buf[:0]))
}

func resultStoredGlobally() {
	var buf [20]byte
	sink = passthrough(buf[:0])
}

func calleeRetainsTheParameter() {
	var buf [20]byte
	consume(retain(buf[:0]))
}

func calleeRetainsThroughANamedResult() {
	var buf [20]byte
	consume(namedRetain(buf[:0]))
}

func appendDestinationConsumedLocally() {
	var buf [20]byte
	consume(append(buf[:0], 'x'))
}

func appendDestinationStoredGlobally() {
	var buf [20]byte
	sink = append(buf[:0], 'x')
}

func appendedElementStoredGlobally() {
	value := 7
	slicePointerSink = append(slicePointerSink, &value)
}

func pointerResultStoredGlobally() {
	value := 3
	pointerSink = identity(&value)
}

func pointerResultDereferencedLocally() int {
	value := 3
	return *identity(&value)
}

func main() {
	runtime.GC()
	resultConsumedLocally()
	resultStoredGlobally()
	calleeRetainsTheParameter()
	calleeRetainsThroughANamedResult()
	appendDestinationConsumedLocally()
	appendDestinationStoredGlobally()
	appendedElementStoredGlobally()
	pointerResultStoredGlobally()
	println(pointerResultDereferencedLocally())
}
`))
	require.NoError(t, err)

	staysOnTheFrame := []string{
		"resultConsumedLocally",
		"appendDestinationConsumedLocally",
		"pointerResultDereferencedLocally",
	}
	for _, name := range staysOnTheFrame {
		function := functionWithSuffix(t, module, "main."+name)
		assert.False(t, callsSymbol(function, "runtime.newobject"),
			"%s: storage consumed within the function was promoted to the heap", name)
	}

	mustReachTheHeap := []string{
		"resultStoredGlobally",
		"calleeRetainsTheParameter",
		"calleeRetainsThroughANamedResult",
		"appendDestinationStoredGlobally",
		"appendedElementStoredGlobally",
		"pointerResultStoredGlobally",
	}
	for _, name := range mustReachTheHeap {
		function := functionWithSuffix(t, module, "main."+name)
		assert.True(t, callsSymbol(function, "runtime.newobject"),
			"%s: a package global can now hold a frame address", name)
	}
}

// A callee that assigns its parameter to its own named result and returns bare
// used to be reported as not letting the parameter escape, because the walk
// consulted the *lowered* function's named results while enumerating the
// callee's body. A package-level variable then held a frame address, which is
// the invariant RUNTIME_PLAN.md 5.8 protects.
func TestParameterAssignedToACalleeNamedResultEscapes(t *testing.T) {
	module, err := goc.Compile("named_result.go", []byte(`
package main

import "runtime"

var pointerSink *int

func namedLeak(value *int) (out *int) {
	out = value
	return
}

func leakViaNamedResult() {
	value := 3
	pointerSink = namedLeak(&value)
}

func main() {
	runtime.GC()
	leakViaNamedResult()
}
`))
	require.NoError(t, err)

	leak := functionWithSuffix(t, module, "main.leakViaNamedResult")
	assert.True(t, callsSymbol(leak, "runtime.newobject"),
		"a callee's named result carried a frame address into a package global")
}

// Taking the address of a field of a local object and letting that address
// escape has to promote the object, because an interior pointer keeps the whole
// object alive. cg12 treated every field selection as a non-escaping use, so
// the object stayed on the frame and a package-level slice held a goroutine
// stack address. Longer chains -- &v.a.b and &v[i].f -- count for the same
// reason, and a chain that steps through a pointer does not, because it names
// storage in some other object.
func TestFieldAddressThatEscapesPromotesItsObject(t *testing.T) {
	module, err := goc.Compile("field_address.go", []byte(`
package main

import "runtime"

type payload struct {
	bytes [64]byte
}

type record struct {
	first payload
	tag   int64
}

type link struct {
	next  *link
	value int64
}

var fieldSink []*payload
var scalarSink []*int64
var elementSink []*record
var nestedSink []*[64]byte
var linkSink []*int64

func leakFieldAddress() {
	object := &record{}
	fieldSink = append(fieldSink, &object.first)
}

func leakScalarFieldAddress() {
	object := &record{}
	scalarSink = append(scalarSink, &object.tag)
}

func leakElementAddress() {
	objects := &[2]record{}
	elementSink = append(elementSink, &objects[1])
}

func leakNestedFieldAddress() {
	object := &record{}
	nestedSink = append(nestedSink, &object.first.bytes)
}

func leakElementFieldAddress() {
	objects := &[2]record{}
	scalarSink = append(scalarSink, &objects[1].tag)
}

func keepFieldAddressLocal() int64 {
	object := &record{}
	pointer := &object.tag
	*pointer = 7
	return object.tag
}

func addressThroughAPointerFieldIsAnotherObject() int64 {
	head := &link{next: &link{value: 9}}
	linkSink = append(linkSink, &head.next.value)
	return head.value
}

func main() {
	runtime.GC()
	leakFieldAddress()
	leakScalarFieldAddress()
	leakElementAddress()
	leakNestedFieldAddress()
	leakElementFieldAddress()
	if keepFieldAddressLocal() != 7 {
		panic("local field address mismatch")
	}
	println(addressThroughAPointerFieldIsAnotherObject())
}
`))
	require.NoError(t, err)

	mustReachTheHeap := []string{
		"leakFieldAddress",
		"leakScalarFieldAddress",
		"leakElementAddress",
		"leakNestedFieldAddress",
		"leakElementFieldAddress",
	}
	for _, name := range mustReachTheHeap {
		function := functionWithSuffix(t, module, "main."+name)
		assert.True(t, callsSymbol(function, "runtime.newobject"),
			"%s: an escaping field address left its object on the frame", name)
	}

	// The counterpart: a field address confined to its function must not force
	// the object onto the heap. Without this the fix would heap-allocate every
	// addressed local, which is the conservatism that makes runtime code
	// allocate on paths where allocation is forbidden.
	keepLocal := functionWithSuffix(t, module, "main.keepFieldAddressLocal")
	assert.False(t, callsSymbol(keepLocal, "runtime.newobject"),
		"a field address confined to its function promoted the object to the heap")
}

// A closure stored in a struct that stays on the frame does not escape, and
// neither do the variables it captures. Treating every use other than a direct
// call as an escape made runtime.hexdumpWords lift its captures, which promoted
// the unwinder values in copystack and scanstack -- functions that must not
// allocate.
func TestClosureStoredInAFrameStructDoesNotEscape(t *testing.T) {
	module, err := goc.Compile("closure_in_struct.go", []byte(`
package main

import "runtime"

type dumper struct {
	mark func(uintptr)
}

func (d *dumper) run(value uintptr) {
	if d.mark != nil {
		d.mark(value)
	}
}

var markSink func(uintptr)

func closureStaysOnTheFrame(base uintptr) {
	annotate := func(value uintptr) {
		println(value + base)
	}
	holder := dumper{mark: annotate}
	holder.run(1)
}

func closureReachesAGlobal(base uintptr) {
	annotate := func(value uintptr) {
		println(value + base)
	}
	markSink = annotate
}

func main() {
	runtime.GC()
	closureStaysOnTheFrame(1)
	closureReachesAGlobal(2)
}
`))
	require.NoError(t, err)

	frameLocal := functionWithSuffix(t, module, "main.closureStaysOnTheFrame")
	assert.False(t, callsSymbol(frameLocal, "runtime.newobject"),
		"a closure held only by a frame-local struct was promoted to the heap")

	global := functionWithSuffix(t, module, "main.closureReachesAGlobal")
	assert.True(t, callsSymbol(global, "runtime.newobject"),
		"a closure stored in a package global stayed on the frame")
}

// TestObjectsTooLargeForAFrameGoToTheHeap covers the bound the escape analysis
// does not answer: an object whose address demonstrably never leaves its
// function, and which still must not be a frame slot because it will not fit in
// one.
//
// The shapes are the three cmd/compile bounds with MaxImplicitStackVarSize, and
// they are decided in two different places -- new(T) and the make backing array
// reach opt.LowerHeapAllocations as candidates, &T{} is placed by the front end
// -- so all three are checked rather than one standing for the others.
//
// The pair just under the bound is what makes this a test of the bound and not
// of "large things go to the heap": 8 192 pointers is exactly 64 KB, which is
// the largest backing array cmd/compile keeps, and it has to stay in the frame.
func TestObjectsTooLargeForAFrameGoToTheHeap(t *testing.T) {
	module, err := goc.Compile("escape.go", []byte(`
package main

import "runtime"

type block struct{ data [65536]int }

func bigNew() int {
	value := new(block)
	value.data[0] = 7
	return value.data[0]
}

func bigLiteral() int {
	value := &block{}
	value.data[1] = 11
	return value.data[1]
}

func bigMake() int {
	values := make([]*int, 0, 65537)
	values = append(values, nil)
	return len(values)
}

func atTheBound() int {
	values := make([]*int, 0, 8192)
	values = append(values, nil)
	return len(values)
}

func main() {
	runtime.GC()
	println(bigNew() + bigLiteral() + bigMake() + atTheBound())
}
`))
	require.NoError(t, err)

	for _, name := range []string{"main.bigNew", "main.bigLiteral", "main.bigMake"} {
		function := functionWithSuffix(t, module, name)
		assert.True(t, callsSymbol(function, "runtime.newobject"),
			"%s kept an object larger than 64 KB in its frame", name)
	}

	bounded := functionWithSuffix(t, module, "main.atTheBound")
	assert.False(t, callsSymbol(bounded, "runtime.newobject"),
		"a backing array of exactly 64 KB was sent to the heap; the bound is inclusive")
	assert.True(t, containsInstruction(bounded, func(instruction ir.Instr) bool {
		return instruction.Op.IsAlloc()
	}), "a backing array of exactly 64 KB did not get a frame slot")
}
