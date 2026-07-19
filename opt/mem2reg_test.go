package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMem2RegStraightLine(t *testing.T) {
	// p = alloc; store x, p; r = load p; ret r  ->  ret x
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.Store(x, p)
	r := e.Load(ir.ClsW, p)
	e.Ret(r)

	assert.True(t, Mem2Reg(f))
	assert.Empty(t, e.Instrs, "alloc/store/load all removed")
	assert.Equal(t, x, e.Jmp.Arg, "load result replaced by the stored value")
}

func TestMem2RegInsertsPhi(t *testing.T) {
	// A value stored differently on two paths becomes a phi at the join.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.Store(f.Word(1), p)
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	join := f.NewBlock("join")
	e.Jnz(cond, a, b)
	a.Store(f.Word(2), p)
	a.Goto(join)
	b.Goto(join)
	r := join.Load(ir.ClsW, p)
	join.Ret(r)

	assert.True(t, Mem2Reg(f))
	require.Len(t, join.Phis, 1)
	// The phi merges the two stored constants.
	var vals []int64
	for _, arg := range join.Phis[0].Args {
		v, ok := constInt(f, arg)
		require.True(t, ok)
		vals = append(vals, v)
	}
	assert.ElementsMatch(t, []int64{1, 2}, vals)
	assert.Equal(t, join.Phis[0].To, join.Jmp.Arg)
	assert.Empty(t, join.Instrs, "the load is gone")
}

func TestMem2RegReadBeforeWriteIsZero(t *testing.T) {
	// Loading before any store yields the zero initial value.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.Ret(e.Load(ir.ClsW, p))

	assert.True(t, Mem2Reg(f))
	v, ok := constInt(f, e.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(0), v)
}

func TestMem2RegDoesNotPromoteEscaped(t *testing.T) {
	// Passing the slot address to a call makes it escape.
	f := ir.NewModule().NewFuncVoid("m")
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.CallVoid(f.Sym("use", 0), p)
	e.RetVoid()

	assert.False(t, Mem2Reg(f))
	assert.Len(t, e.Instrs, 2, "alloc and call remain")
}

func TestMem2RegDoesNotPromoteSubword(t *testing.T) {
	// A byte store cannot back a full-width promoted variable.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.StoreSub(ir.SubB, x, p)
	e.Ret(e.LoadSub(ir.ClsW, ir.SubUB, p))

	assert.False(t, Mem2Reg(f))
}

func TestMem2RegFloatSlotZeroInit(t *testing.T) {
	for _, cls := range []ir.Cls{ir.ClsS, ir.ClsD} {
		f := ir.NewModule().NewFunc("m", cls)
		e := f.Entry()
		p := e.Alloc(8, 8)
		e.Ret(e.Load(cls, p)) // read before write -> zero float

		require.True(t, Mem2Reg(f))
		c := f.Consts[e.Jmp.Arg.ID]
		assert.Equal(t, ir.ConstFloat, c.Kind)
		assert.Equal(t, cls, c.Cls)
		assert.Equal(t, 0.0, c.Flt)
	}
}

func TestMem2RegKeepsNonPromotedMemoryOps(t *testing.T) {
	// A promotable slot alongside loads/stores through a pointer parameter: only
	// the slot is promoted; the parameter's memory ops are untouched.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	q := f.Param("q", ir.ClsL)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.Store(f.Word(5), p)
	rv := e.Load(ir.ClsW, p) // promoted away
	e.Store(rv, q)           // store through q: kept, value rewritten to 5
	lq := e.Load(ir.ClsW, q) // load through q: kept
	e.Ret(lq)

	require.True(t, Mem2Reg(f))
	require.Len(t, e.Instrs, 2)
	assert.True(t, e.Instrs[0].Op.IsStore())
	v, ok := constInt(f, e.Instrs[0].Args[0])
	require.True(t, ok)
	assert.Equal(t, int64(5), v, "promoted load value propagated into the kept store")
	assert.True(t, e.Instrs[1].Op.IsLoad())
	assert.Equal(t, e.Instrs[1].To, e.Jmp.Arg)
}

func TestMem2RegRejectsInconsistentClass(t *testing.T) {
	// The slot is written as a word but read as a long: widths disagree, so it
	// cannot be promoted to a single SSA variable.
	f := ir.NewModule().NewFunc("m", ir.ClsL)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.Store(x, p)             // OStorew (word)
	e.Ret(e.Load(ir.ClsL, p)) // OLoadl (long)
	assert.False(t, Mem2Reg(f))
}

func TestMem2RegRejectsAddressInPhi(t *testing.T) {
	// The slot address flows into a phi, so the pointer escapes.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.Store(f.Word(1), p)
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	join := f.NewBlock("join")
	e.Jnz(cond, a, b)
	a.Goto(join)
	b.Goto(join)
	join.Phi(ir.ClsL, ir.PhiEdge{From: a, Val: p}, ir.PhiEdge{From: b, Val: p}) // p escapes
	join.Ret(join.Load(ir.ClsW, p))

	assert.False(t, Mem2Reg(f))
}

func TestMem2RegNoAllocs(t *testing.T) {
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b))
	assert.False(t, Mem2Reg(f))
}

func TestMem2RegSkipsComputedGoto(t *testing.T) {
	// A function with a computed goto is left in memory form. Not for correctness --
	// lower.CoalescePhis can resolve the dispatch phis -- but for speed: promoting a
	// variable live across the dispatch coalesces it into one function-spanning live
	// range that linear scan spills across every handler, which measured slower than
	// the memory form on a real interpreter. So mem2reg declines the whole function.
	f := ir.NewModule().NewFunc("interp", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.Store(f.Word(1), p)
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	e.BrIndirect(e.BlockAddr(a), a, b) // computed goto to a or b
	a.Store(f.Word(2), p)
	a.Ret(a.Load(ir.ClsW, p))
	b.Ret(b.Load(ir.ClsW, p))

	assert.False(t, Mem2Reg(f), "a computed-goto function is left in memory form")
	for _, blk := range f.Blocks {
		assert.Empty(t, blk.Phis, "no phi is created for a computed-goto function")
	}
}
