package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLowerHeapAllocationsPromotesLocalObject(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("local", ir.ClsL)
	block := function.Entry()
	object := block.HeapAlloc(function.Sym("runtime.newobject", 0), function.Sym("type.int", 0), 8, 8)
	block.Store(function.Long(42), object)
	block.Ret(block.Load(ir.ClsL, object))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OAlloc8, block.Instrs[0].Op)
	assert.Len(t, block.Instrs[0].Args, 1)
	assert.Equal(t, int64(8), function.Consts[block.Instrs[0].Arg(0).ID].Int)
	require.Equal(t, ir.OCall, block.Instrs[1].Op)
	zeroHelper := function.Consts[block.Instrs[1].Arg(0).ID]
	assert.Equal(t, "goc_memset", zeroHelper.Sym)
	assert.Equal(t, object, block.Instrs[1].Arg(1))
}

func TestLowerHeapAllocationsKeepsEscapingObjectOnHeap(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("escape", ir.ClsP)
	block := function.Entry()
	typeDescriptor := function.Sym("type.int", 0)
	object := block.HeapAlloc(function.Sym("runtime.newobject", 0), typeDescriptor, 8, 8)
	block.Ret(object)

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OCall, block.Instrs[0].Op)
	assert.Equal(t, []ir.Ref{function.Sym("runtime.newobject", 0), typeDescriptor}, block.Instrs[0].Args)
}

func TestLowerHeapAllocationsAllowsPointerInLocalSlot(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("local", ir.ClsL)
	block := function.Entry()
	slot := block.Alloc(8, 8)
	object := block.HeapAlloc(function.Sym("runtime.newobject", 0), function.Sym("type.int", 0), 8, 8)
	block.Store(object, slot)
	block.Store(function.Long(42), object)
	block.Ret(block.Load(ir.ClsL, block.Load(ir.ClsP, slot)))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OAlloc8, block.Instrs[1].Op)
}

func TestLowerHeapAllocationsTracksEscapeThroughLocalSlot(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("escape", ir.ClsP)
	block := function.Entry()
	slot := block.Alloc(8, 8)
	typeDescriptor := function.Sym("type.int", 0)
	object := block.HeapAlloc(function.Sym("runtime.newobject", 0), typeDescriptor, 8, 8)
	block.Store(object, slot)
	block.Ret(block.Load(ir.ClsP, slot))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OCall, block.Instrs[1].Op)
	assert.Equal(t, []ir.Ref{function.Sym("runtime.newobject", 0), typeDescriptor}, block.Instrs[1].Args)
}

func TestLowerHeapAllocationsAllowsLocalPhi(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("localPhi", ir.ClsL)
	condition := function.Param("condition", ir.ClsW)
	entry := function.Entry()
	left := function.NewBlock("left")
	right := function.NewBlock("right")
	join := function.NewBlock("join")
	object := entry.HeapAlloc(
		function.Sym("runtime.newobject", 0),
		function.Sym("type.int", 0),
		8,
		8,
	)
	entry.Jnz(condition, left, right)
	left.Goto(join)
	right.Goto(join)
	pointer := join.Phi(
		ir.ClsP,
		ir.PhiEdge{From: left, Val: object},
		ir.PhiEdge{From: right, Val: function.Sym("global.int", 0)},
	)
	join.Ret(join.Load(ir.ClsL, pointer))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OAlloc8, entry.Instrs[0].Op)
}

func TestLowerHeapAllocationsTracksEscapeThroughPhi(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("escapingPhi", ir.ClsP)
	condition := function.Param("condition", ir.ClsW)
	entry := function.Entry()
	left := function.NewBlock("left")
	right := function.NewBlock("right")
	join := function.NewBlock("join")
	typeDescriptor := function.Sym("type.int", 0)
	object := entry.HeapAlloc(
		function.Sym("runtime.newobject", 0),
		typeDescriptor,
		8,
		8,
	)
	entry.Jnz(condition, left, right)
	left.Goto(join)
	right.Goto(join)
	pointer := join.Phi(
		ir.ClsP,
		ir.PhiEdge{From: left, Val: object},
		ir.PhiEdge{From: right, Val: function.Sym("global.int", 0)},
	)
	join.Ret(pointer)

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OCall, entry.Instrs[0].Op)
	assert.Equal(t, []ir.Ref{function.Sym("runtime.newobject", 0), typeDescriptor}, entry.Instrs[0].Args)
}

func TestLowerHeapAllocationsAllowsPointerComparison(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("compareLocalPointer", ir.ClsW)
	block := function.Entry()
	object := block.HeapAlloc(
		function.Sym("runtime.newobject", 0),
		function.Sym("type.int", 0),
		8,
		8,
	)
	nilPointer := function.ConstInt(ir.ClsP, 0)
	comparison := block.Cmp(ir.CmpEq, ir.ClsW, object, nilPointer)
	block.Ret(comparison)

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OAlloc8, block.Instrs[0].Op)
}

func TestLowerHeapAllocationsAllowsDynamicPointerOffset(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("indexLocalObject", ir.ClsL)
	offset := function.Param("offset", ir.ClsL)
	block := function.Entry()
	object := block.HeapAlloc(
		function.Sym("runtime.newobject", 0),
		function.Sym("type.array", 0),
		16,
		8,
	)
	element := block.Add(ir.ClsP, object, offset)
	block.Store(function.Long(42), element)
	block.Ret(block.Load(ir.ClsL, element))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OAlloc8, block.Instrs[0].Op)
}

func TestLowerHeapAllocationsAllowsMemsetInitialization(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("local", ir.ClsL)
	block := function.Entry()
	object := block.HeapAlloc(function.Sym("runtime.newobject", 0), function.Sym("type.pair", 0), 16, 8)
	block.Call(ir.ClsP, function.Sym("memset", 0), object, function.Word(0), function.Long(16))
	block.Store(function.Long(42), object)
	block.Ret(block.Load(ir.ClsL, object))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OAlloc8, block.Instrs[0].Op)
}

func TestLowerHeapAllocationsAllowsCompilerMemoryHelpers(t *testing.T) {
	for _, helper := range []string{"goc_memset", "goc_memcpy", "goc_memmove", "goc_memcmp"} {
		t.Run(helper, func(t *testing.T) {
			module := ir.NewModule()
			function := module.NewFunc("local", ir.ClsL)
			block := function.Entry()
			object := block.HeapAlloc(function.Sym("runtime.newobject", 0), function.Sym("type.pair", 0), 16, 8)
			block.Call(ir.ClsL, function.Sym(helper, 0), object, object, function.Long(16))
			block.Ret(block.Load(ir.ClsL, object))

			require.True(t, LowerHeapAllocations(module))
			assert.Equal(t, ir.OAlloc8, block.Instrs[0].Op)
		})
	}
}

func TestLowerHeapAllocationsTracksPointerCopiedIntoGlobalMemory(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("copyPointerIntoGlobal")
	block := function.Entry()
	objectType := function.Sym("type.object", 0)
	object := block.HeapAlloc(function.Sym("runtime.newobject", 0), objectType, 8, 8)
	descriptor := block.Alloc(8, 24)
	block.Store(object, descriptor)
	copy := block.Alloc(8, 24)
	block.CallVoid(
		function.Sym("goc_memcpy", 0),
		copy,
		descriptor,
		function.Long(24),
	)
	block.CallVoid(
		function.Sym("goc_memcpy", 0),
		function.Sym("global.descriptor", 0),
		copy,
		function.Long(24),
	)
	block.RetVoid()

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OCall, block.Instrs[0].Op)
	assert.Equal(t, []ir.Ref{function.Sym("runtime.newobject", 0), objectType}, block.Instrs[0].Args)
}

func TestLowerHeapAllocationsAllowsPointerCopiedWithinLocalMemory(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("copyPointerLocally", ir.ClsL)
	block := function.Entry()
	object := block.HeapAlloc(
		function.Sym("runtime.newobject", 0),
		function.Sym("type.object", 0),
		8,
		8,
	)
	descriptor := block.Alloc(8, 24)
	block.Store(object, descriptor)
	copy := block.Alloc(8, 24)
	block.CallVoid(
		function.Sym("goc_memcpy", 0),
		copy,
		descriptor,
		function.Long(24),
	)
	copiedObject := block.Load(ir.ClsP, copy)
	block.Store(function.Long(42), copiedObject)
	block.Ret(block.Load(ir.ClsL, copiedObject))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OAlloc8, block.Instrs[0].Op)
}

func TestLowerHeapAllocationsAllowsPointerStoreIntoCandidate(t *testing.T) {
	for _, storeFunction := range []string{"runtime.atomicstorep", "goc_storep"} {
		t.Run(storeFunction, func(t *testing.T) {
			module := ir.NewModule()
			module.SymAttrs = map[string]ir.SymAttr{storeFunction: ir.SymAtomicPointerStore}
			function := module.NewFunc("localPointerField", ir.ClsL)
			block := function.Entry()
			object := block.HeapAlloc(function.Sym("runtime.newobject", 0), function.Sym("type.pointerHolder", 0), 8, 8)
			block.CallVoid(function.Sym(storeFunction, 0), object, function.Sym("global.pointer", 0))
			block.Ret(block.Load(ir.ClsL, object))

			require.True(t, LowerHeapAllocations(module))
			assert.Equal(t, ir.OAlloc8, block.Instrs[0].Op)
		})
	}
}

func TestLowerHeapAllocationsKeepsPointerLoadedFromLocalSlotWhenStoredIntoCandidate(t *testing.T) {
	module := ir.NewModule()
	module.SymAttrs = map[string]ir.SymAttr{"goc_storep": ir.SymAtomicPointerStore}
	function := module.NewFunc("capturePointer", ir.ClsP)
	block := function.Entry()

	objectType := function.Sym("type.object", 0)
	object := block.HeapAlloc(function.Sym("runtime.newobject", 0), objectType, 8, 8)
	objectSlot := block.Alloc(8, 8)
	block.Store(object, objectSlot)

	descriptorType := function.Sym("type.descriptor", 0)
	descriptor := block.HeapAlloc(function.Sym("runtime.newobject", 0), descriptorType, 16, 8)
	receiverField := block.Add(ir.ClsP, descriptor, function.Long(8))
	loadedObject := block.Load(ir.ClsP, objectSlot)
	block.CallVoid(function.Sym("goc_storep", 0), receiverField, loadedObject)
	block.Ret(descriptor)

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OCall, block.Instrs[0].Op)
	assert.Equal(t, ir.OCall, block.Instrs[3].Op)
}

func TestLowerHeapAllocationsAllowsGrowSliceToObserveCandidate(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("growLocalSlice", ir.ClsL)
	block := function.Entry()
	object := block.HeapAlloc(function.Sym("runtime.newobject", 0), function.Sym("type.array", 0), 16, 8)
	block.Call(
		ir.ClsP,
		function.Sym("runtime.growslice", 0),
		object,
		function.Long(2),
		function.Long(1),
		function.Long(1),
		function.Sym("type.int", 0),
	)
	block.Store(function.Long(42), object)
	block.Ret(block.Load(ir.ClsL, object))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, ir.OAlloc8, block.Instrs[0].Op)
}
