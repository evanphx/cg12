package lower

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/evanphx/cg12/ir"
)

func TestSequentializeCyclicCopy(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("s")
	t1 := f.NewTemp("t1", ir.ClsW)
	t2 := f.NewTemp("t2", ir.ClsW)
	// A swap: t1<-t2, t2<-t1 — must break the cycle with a fresh temp.
	seq := sequentialize(f, []movePair{{dst: t1, src: t2}, {dst: t2, src: t1}})
	assert.Len(t, seq, 3, "a 2-cycle needs one extra move")
	assert.Greater(t, len(f.Temps), 2, "a fresh temp was introduced")
}

func TestSplitCriticalEdges(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	mid := f.NewBlock("mid")
	join := f.NewBlock("join")
	start.Jnz(cond, mid, join) // start->join is critical (start:2 succ, join:2 pred)
	mid.Goto(join)
	r := join.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(1)}, ir.PhiEdge{From: mid, Val: f.Word(2)})
	join.Ret(r)

	before := len(f.Blocks)
	SplitCriticalEdges(f)
	assert.Greater(t, len(f.Blocks), before, "a split block was inserted")
	// The start->join edge no longer targets join directly.
	assert.NotSame(t, join, start.Jmp.To2)
	assert.Same(t, join, start.Jmp.To2.Jmp.To, "the split block jumps to join")
}

func TestDestructSSARemovesPhis(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	join := f.NewBlock("join")
	start.Jnz(cond, a, b)
	a.Goto(join)
	b.Goto(join)
	join.Ret(join.Phi(ir.ClsW, ir.PhiEdge{From: a, Val: f.Word(1)}, ir.PhiEdge{From: b, Val: f.Word(2)}))

	SplitCriticalEdges(f)
	DestructSSA(f)
	for _, blk := range f.Blocks {
		assert.Empty(t, blk.Phis, "phis must be gone after destruction")
	}
}

func TestCoalescePhisRemovesComputedGotoCopies(t *testing.T) {
	// A phi whose arguments arrive over computed-goto edges is coalesced away, so
	// SSA destruction emits no copy for it -- the case that, left standing, would
	// force a copy in every predecessor of an interpreter's dispatch and explode.
	// Two indirect predecessors feed one merge; after coalescing and destruction
	// the phi is gone and neither predecessor gained a copy.
	f := ir.NewModule().NewFunc("interp", ir.ClsW)
	param := f.Param("n", ir.ClsW)
	e := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	exit := f.NewBlock("exit")

	t0 := e.Add(ir.ClsW, param, f.Word(1))
	e.BrIndirect(e.BlockAddr(a), a, b) // e -> a, e -> b (indirect)

	t1 := b.Add(ir.ClsW, param, f.Word(2))
	b.BrIndirect(b.BlockAddr(a), a, exit) // b -> a, b -> exit (indirect)

	xa := a.Phi(ir.ClsW, ir.PhiEdge{From: e, Val: t0}, ir.PhiEdge{From: b, Val: t1})
	a.Ret(xa)
	exit.Ret(f.Word(0))

	SplitCriticalEdges(f)
	CoalescePhis(f)
	DestructSSA(f)

	for _, blk := range f.Blocks {
		assert.Empty(t, blk.Phis, "phis are gone after destruction")
		for _, in := range blk.Instrs {
			assert.NotEqual(t, ir.OCopy, in.Op,
				"a coalesced computed-goto phi needs no copy in block %s", blk.Name)
		}
	}
}

func TestCoalescePhisKeepsInterferingCopies(t *testing.T) {
	// When a phi's result and an argument are simultaneously live (they interfere),
	// they must not be coalesced -- doing so would clobber one. Here the argument t0
	// is used again after the merge, so it stays live alongside the phi result and a
	// copy is required. A plain (splittable) merge exercises the same interference
	// check that guards the computed-goto path.
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	e := f.Entry()
	a := f.NewBlock("a")
	join := f.NewBlock("join")

	t0 := e.Add(ir.ClsW, cond, f.Word(1))
	e.Jnz(cond, a, join) // e -> join carries t0 to the phi
	t1 := a.Add(ir.ClsW, cond, f.Word(2))
	a.Goto(join)
	x := join.Phi(ir.ClsW, ir.PhiEdge{From: e, Val: t0}, ir.PhiEdge{From: a, Val: t1})
	// t0 lives past the merge, so it interferes with x: x and t0 cannot share a reg.
	join.Ret(join.Add(ir.ClsW, x, t0))

	SplitCriticalEdges(f)
	CoalescePhis(f) // only touches indirect edges; this one is a no-op here
	DestructSSA(f)
	for _, blk := range f.Blocks {
		assert.Empty(t, blk.Phis, "phis are gone after destruction")
	}
}
