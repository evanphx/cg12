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

func TestMem2RegPromotesThroughLifetimeMarkersAndStripsThem(t *testing.T) {
	// A promotable variable bracketed by lifetime.start/end is still promoted (the
	// markers must not count as an escape), and both markers are removed afterward so
	// downstream passes and the backend never see them.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.LifeStart(p)
	e.Store(x, p)
	r := e.Load(ir.ClsW, p)
	e.LifeEnd(p)
	e.Ret(r)

	assert.True(t, Mem2Reg(f))
	assert.Empty(t, e.Instrs, "alloc, store, load, and both lifetime markers all removed")
	assert.Equal(t, x, e.Jmp.Arg, "load result replaced by the stored value")
}

func TestMem2RegKeepsLifetimeMarkersOnNonPromotedAlloca(t *testing.T) {
	// Markers on an alloca that is NOT promoted (its address escapes by being
	// returned) survive mem2reg: they carry the slot's live region to the backend's
	// frame stack-slot coloring. (Dead allocas' markers are cleaned up later by
	// DeadAlloc; this slot is live -- its address is returned.)
	f := ir.NewModule().NewFunc("m", ir.ClsL)
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.LifeStart(p)
	e.LifeEnd(p)
	e.Ret(p) // returning the address escapes it: not promotable

	Mem2Reg(f)
	nLife := 0
	for i := range e.Instrs {
		if e.Instrs[i].Op.IsLifetime() {
			nLife++
		}
	}
	assert.Equal(t, 2, nLife, "both lifetime markers survive on a non-promoted alloca")
}

func TestMem2RegPromotesPointerSlotAccessedAsLong(t *testing.T) {
	// A pointer local written as ClsP and read as a plain ClsL (common from
	// pointer/integer-punning macros) is still one word and must promote: ClsP is the
	// target's word register class, so the two accesses are the same width.
	f := ir.NewModule().NewFunc("m", ir.ClsL)
	p := f.Param("p", ir.ClsP)
	e := f.Entry()
	a := e.Alloc(8, 8)
	e.Store(p, a)           // storep %p, %a
	r := e.Load(ir.ClsL, a) // loadl %a
	e.Ret(r)

	assert.True(t, Mem2Reg(f), "pointer slot touched as ClsP and ClsL promotes")
	assert.Empty(t, e.Instrs, "alloc/store/load removed")
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

func TestMem2RegPromotesComputedGoto(t *testing.T) {
	// A computed-goto function is promoted like any other; lower.CoalescePhis later
	// unifies the mesh phis into one temp per variable, and any residual phi copy
	// (an initial value on the entry's own indirect branch) is placeable before that
	// branch. So mem2reg promotes, replacing the loads with the stored values.
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

	assert.True(t, Mem2Reg(f), "a computed-goto function is promoted")
	for _, blk := range f.Blocks {
		for _, in := range blk.Instrs {
			assert.False(t, in.Op.IsLoad(), "loads are replaced by promoted values")
		}
	}
}

func TestMem2RegPrunesDeadPhi(t *testing.T) {
	// A variable stored on two paths that reach a join, but never loaded from the
	// join onward, is dead there. Minimal SSA would still place a phi at the join
	// (it sits in the dominance frontier of the stores); pruned SSA must not -- a
	// phi for a dead variable only creates a spurious live range. This is what keeps
	// an irreducible computed-goto mesh from sprouting a mesh-wide range for every
	// promoted handler-local, which otherwise swamps the register allocator.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(4, 4)
	e.Store(f.Word(1), p)
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	join := f.NewBlock("join")
	e.Jnz(cond, a, b)
	a.Store(f.Word(2), p) // p is stored on the a-path...
	a.Goto(join)
	b.Goto(join)
	join.Ret(f.Word(7)) // ...but never loaded at or after the join: p is dead here.

	require.True(t, Mem2Reg(f))
	assert.Empty(t, join.Phis, "no phi for a variable that is dead at the join")
}

// TestMem2RegPhiForAManagedSlotIsAGCRoot is the property that makes promotion
// safe for a garbage-collected frontend: the value a promoted pointer slot used
// to hold is still findable by the collector afterwards.
//
// While the slot exists, the frame map describes it and the collector reads the
// pointer out of the frame at every safepoint. Promotion moves that value into
// SSA, where the backend reports it only if its temporary is marked GCRef -- and
// a phi is the one place Mem2Reg has to mint a temporary rather than reuse the
// stored value's own. An unmarked phi is a pointer live across a loop back edge
// that is a root nowhere, which is a collected-while-live object.
//
// The class cannot stand in for the flag. goc types every pointer ClsP and marks
// the managed ones itself; ir.LowerPointers marks only ClsM.
func TestMem2RegPhiForAManagedSlotIsAGCRoot(t *testing.T) {
	f := ir.NewModule().NewFunc("m", ir.ClsP)
	initial := f.ParamRef("initial")
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	slot := f.MarkGCRef(e.Alloc(8, 8)) // a frame slot holding a managed pointer
	e.Store(initial, slot)

	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	done := f.NewBlock("done")
	e.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: e, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), done, body)

	// The body reads the slot across a safepoint and writes a new value back, so
	// the promoted variable needs a phi at the loop header.
	body.Safepoint()
	current := body.Load(ir.ClsP, slot)
	body.Store(body.Add(ir.ClsP, current, f.Long(8)), slot)
	next := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, next)
	done.Ret(done.Load(ir.ClsP, slot))

	require.True(t, Mem2Reg(f))
	var promoted *ir.Phi
	for _, phi := range loop.Phis {
		if phi.Cls == ir.ClsP {
			promoted = phi
		}
	}
	require.NotNil(t, promoted, "the pointer slot got a loop-header phi")
	assert.True(t, f.Temp(promoted.To).GCRef,
		"the phi for a managed slot is a GC root; unmarked, the collector frees what the loop carries")
}

// TestMem2RegPhiForAnUnmanagedSlotStaysUnmarked is the other half: the marking
// follows the slot, it is not applied to every promoted pointer. A GCRef
// temporary live across a safepoint is pinned to a stack slot for its whole life,
// so marking a frame address or a plain integer would cost a spill for nothing.
func TestMem2RegPhiForAnUnmanagedSlotStaysUnmarked(t *testing.T) {
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	slot := e.Alloc(8, 8) // no MarkGCRef: an ordinary counter
	e.Store(f.Word(0), slot)

	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	done := f.NewBlock("done")
	e.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: e, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), done, body)
	body.Safepoint()
	body.Store(body.Add(ir.ClsW, body.Load(ir.ClsW, slot), f.Word(1)), slot)
	next := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, next)
	done.Ret(done.Load(ir.ClsW, slot))

	require.True(t, Mem2Reg(f))
	for _, phi := range loop.Phis {
		assert.False(t, f.Temp(phi.To).GCRef, "an unmanaged slot's phi is not made a root")
	}
}
