package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markAtomicPointerStore gives a module the attribute goc's registerSymAttrs
// sets, so the barrier helper is recognised the same way here as in a real
// compilation.
func markAtomicPointerStore(module *ir.Module) {
	if module.SymAttrs == nil {
		module.SymAttrs = map[string]ir.SymAttr{}
	}
	module.SymAttrs["goc_storep"] |= ir.SymAtomicPointerStore
}

func TestFrameEscapesReportsAFrameAddressStoredIntoAHeapObject(t *testing.T) {
	module := ir.NewModule()
	markAtomicPointerStore(module)
	function := module.NewFuncVoid("publish")
	block := function.Entry()
	local := block.Alloc(8, 8)
	object := block.Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym("type.ptr", 0))
	block.CallVoid(function.Sym("goc_storep", 0), object, local)
	block.RetVoid()

	escapes := FrameEscapes(module)
	require.Len(t, escapes, 1)
	assert.Equal(t, "publish", escapes[0].Func)
	assert.Equal(t, FrameEscapeBarrier, escapes[0].Kind)
	assert.Equal(t, FrameEscapeIntoCallResult, escapes[0].Destination)
	assert.Equal(t, "runtime.newobject", escapes[0].Callee)
}

func TestFrameEscapesReportsAFrameAddressStoredIntoAGlobal(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("publish")
	block := function.Entry()
	local := block.Alloc(8, 8)
	block.Store(local, function.Sym("keepalive.root", 0))
	block.RetVoid()

	escapes := FrameEscapes(module)
	require.Len(t, escapes, 1)
	assert.Equal(t, FrameEscapeStore, escapes[0].Kind)
	assert.Equal(t, FrameEscapeIntoGlobal, escapes[0].Destination)
}

func TestFrameEscapesReportsAReturnedFrameAddress(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("leak", ir.ClsP)
	block := function.Entry()
	block.Ret(block.Alloc(8, 16))

	escapes := FrameEscapes(module)
	require.Len(t, escapes, 1)
	assert.Equal(t, FrameEscapeReturn, escapes[0].Kind)
	assert.Equal(t, FrameEscapeIntoCaller, escapes[0].Destination)
}

func TestFrameEscapesAllowsAFrameAddressStoredIntoAnotherFrameSlot(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("keep")
	block := function.Entry()
	slot := block.Alloc(8, 8)
	local := block.Alloc(8, 16)
	block.Store(local, slot)
	block.RetVoid()

	assert.Empty(t, FrameEscapes(module))
}

// A field of a frame allocation is still the same frame. The destination here
// is a constant offset from the slot, which is how every struct-field store
// looks after lowering.
func TestFrameEscapesAllowsAFrameAddressStoredIntoAFrameField(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("keep")
	block := function.Entry()
	aggregate := block.Alloc(8, 32)
	local := block.Alloc(8, 8)
	block.Store(local, block.Add(ir.ClsP, aggregate, function.Long(16)))
	block.RetVoid()

	assert.Empty(t, FrameEscapes(module))
}

// The address is derived through pointer arithmetic and a copy before it is
// stored, which is what an interior pointer to a frame object looks like.
func TestFrameEscapesFollowsPointerArithmeticAndCopies(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("publish")
	block := function.Entry()
	local := block.Alloc(8, 32)
	interior := block.Copy(ir.ClsP, block.Add(ir.ClsP, local, function.Long(8)))
	block.Store(interior, function.Sym("global.root", 0))
	block.RetVoid()

	escapes := FrameEscapes(module)
	require.Len(t, escapes, 1)
	assert.Equal(t, FrameEscapeIntoGlobal, escapes[0].Destination)
}

// A frame address parked in a frame slot and read back is still a frame
// address; storing what came out of the slot into a heap object publishes it
// just as directly.
func TestFrameEscapesFollowsAFrameAddressThroughAFrameSlot(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("publish")
	block := function.Entry()
	slot := block.Alloc(8, 8)
	local := block.Alloc(8, 16)
	block.Store(local, slot)
	object := block.Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym("type.ptr", 0))
	block.Store(block.Load(ir.ClsP, slot), object)
	block.RetVoid()

	escapes := FrameEscapes(module)
	require.Len(t, escapes, 1)
	assert.Equal(t, FrameEscapeStore, escapes[0].Kind)
	assert.Equal(t, FrameEscapeIntoCallResult, escapes[0].Destination)
}

func TestFrameEscapesFollowsAFrameAddressThroughAPhi(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("publish")
	entry := function.Entry()
	other := function.NewBlock("other")
	join := function.NewBlock("join")
	local := entry.Alloc(8, 8)
	heap := entry.Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym("type.ptr", 0))
	entry.Jnz(function.Word(1), other, join)
	other.Goto(join)
	merged := join.Phi(ir.ClsP, ir.PhiEdge{From: entry, Val: local}, ir.PhiEdge{From: other, Val: heap})
	join.Store(merged, function.Sym("global.root", 0))
	join.RetVoid()

	escapes := FrameEscapes(module)
	require.Len(t, escapes, 1)
	assert.Equal(t, FrameEscapeIntoGlobal, escapes[0].Destination)
}

// Passing &local to a callee is how Go is written, and whether the callee
// retains it is a question about the callee's own body. The audit reports the
// store the callee makes, not the argument.
func TestFrameEscapesDoesNotReportAFrameAddressPassedToACall(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("pass")
	block := function.Entry()
	local := block.Alloc(8, 8)
	block.CallVoid(function.Sym("helper", 0), local)
	block.RetVoid()

	assert.Empty(t, FrameEscapes(module))
}

// Storing something that is not a frame address into a heap object is the
// ordinary case and must stay silent.
func TestFrameEscapesIgnoresAHeapPointerStoredIntoAHeapObject(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("store")
	block := function.Entry()
	object := block.Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym("type.ptr", 0))
	other := block.Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym("type.ptr", 0))
	block.Store(other, object)
	block.RetVoid()

	assert.Empty(t, FrameEscapes(module))
}

// The memory helpers return the destination they were handed, so a frame
// address survives goc_memset unchanged and must still be tracked.
func TestFrameEscapesFollowsAFrameAddressThroughAMemoryHelper(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("publish")
	block := function.Entry()
	local := block.Alloc(8, 16)
	zeroed := block.Call(ir.ClsP, function.Sym("goc_memset", 0), local, function.Word(0), function.Long(16))
	block.Store(zeroed, function.Sym("global.root", 0))
	block.RetVoid()

	escapes := FrameEscapes(module)
	require.Len(t, escapes, 1)
	assert.Equal(t, FrameEscapeIntoGlobal, escapes[0].Destination)
}

// A finding's identity must survive renumbering: the same defect emitted after
// an extra unrelated instruction is the same key.
func TestFrameEscapeKeyIgnoresTemporaryNumbering(t *testing.T) {
	build := func(padding int) FrameEscape {
		module := ir.NewModule()
		function := module.NewFuncVoid("publish")
		block := function.Entry()
		for pad := 0; pad < padding; pad++ {
			block.Add(ir.ClsL, function.Long(1), function.Long(2))
		}
		local := block.Alloc(8, 8)
		block.Store(local, function.Sym("global.root", 0))
		block.RetVoid()

		escapes := FrameEscapes(module)
		require.Len(t, escapes, 1)
		return escapes[0]
	}

	plain := build(0)
	padded := build(3)
	assert.Equal(t, plain.Key(), padded.Key())
	assert.NotEqual(t, plain.Alloc, padded.Alloc)
}
