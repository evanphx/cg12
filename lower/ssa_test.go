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
