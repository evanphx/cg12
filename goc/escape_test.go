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

func TestDirectDeferredFunctionLiteralEscapesInRuntimeBuild(t *testing.T) {
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
	assert.True(t, callsSymbol(install, "runtime.newobject"), "deferred closure remained on the caller stack")
}

func TestDeferredClosureCapturedNamedResultEscapesInRuntimeBuild(t *testing.T) {
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
	assert.GreaterOrEqual(t, newObjectCalls, 2, "deferred closure and captured result should both escape")
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
