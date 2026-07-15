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
