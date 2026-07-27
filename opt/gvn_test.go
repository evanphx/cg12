package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func countOp(f *ir.Func, op ir.Op) int {
	n := 0
	for _, b := range f.Blocks {
		for _, in := range b.Instrs {
			if in.Op == op {
				n++
			}
		}
	}
	return n
}

func TestGVNEliminatesRedundant(t *testing.T) {
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	t1 := e.Add(ir.ClsW, a, b)
	t2 := e.Add(ir.ClsW, a, b) // redundant with t1
	e.Ret(e.Add(ir.ClsW, t1, t2))

	assert.True(t, GVN(f))
	assert.Equal(t, 2, countOp(f, ir.OAdd), "one of the two identical adds removed")
}

func TestGVNCommutative(t *testing.T) {
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	t1 := e.Add(ir.ClsW, a, b)
	t2 := e.Add(ir.ClsW, b, a) // same value, swapped operands
	e.Ret(e.Add(ir.ClsW, t1, t2))

	assert.True(t, GVN(f))
	assert.Equal(t, 2, countOp(f, ir.OAdd))
}

func TestGVNReusesDominatingValue(t *testing.T) {
	// A value computed in the entry is reused in a dominated block.
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	c := f.Param("c", ir.ClsW)
	e := f.Entry()
	t0 := e.Add(ir.ClsW, a, b)
	blkA := f.NewBlock("a")
	blkB := f.NewBlock("b")
	e.Jnz(c, blkA, blkB)
	t1 := blkA.Add(ir.ClsW, a, b) // redundant with dominating t0
	blkA.Ret(t1)
	blkB.Ret(b)

	assert.True(t, GVN(f))
	assert.Empty(t, blkA.Instrs, "redundant add in dominated block removed")
	assert.Equal(t, t0, blkA.Jmp.Arg)
}

func TestGVNDoesNotMergeAcrossSiblings(t *testing.T) {
	// Neither arm dominates the other, so identical computations stay put.
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	c := f.Param("c", ir.ClsW)
	e := f.Entry()
	blkA := f.NewBlock("a")
	blkB := f.NewBlock("b")
	join := f.NewBlock("join")
	e.Jnz(c, blkA, blkB)
	t1 := blkA.Add(ir.ClsW, a, b)
	blkA.Goto(join)
	t2 := blkB.Add(ir.ClsW, a, b)
	blkB.Goto(join)
	r := join.Phi(ir.ClsW, ir.PhiEdge{From: blkA, Val: t1}, ir.PhiEdge{From: blkB, Val: t2})
	join.Ret(r)

	GVN(f)
	assert.Equal(t, 2, countOp(f, ir.OAdd), "sibling computations not merged")
}

func TestGVNDistinctOpsKept(t *testing.T) {
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	t1 := e.Add(ir.ClsW, a, b)
	t2 := e.Sub(ir.ClsW, a, b) // different op, not congruent
	e.Ret(e.Add(ir.ClsW, t1, t2))
	assert.False(t, GVN(f))
}

func TestGVNIgnoresLoads(t *testing.T) {
	// Without alias analysis, two loads are not assumed equal.
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	t1 := e.Load(ir.ClsW, p)
	t2 := e.Load(ir.ClsW, p)
	e.Ret(e.Add(ir.ClsW, t1, t2))
	assert.False(t, GVN(f))
	assert.Equal(t, 2, countOp(f, ir.OLoaduw))
}

func TestGVNNoChange(t *testing.T) {
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b))
	assert.False(t, GVN(f))
}

func TestGVNMergesRawPointersPreservesManaged(t *testing.T) {
	// A raw pointer (ClsP) has no GC-root identity, so two identical pointer
	// computations are the same value: GVN merges them.
	f := ir.NewModule().NewFunc("raw", ir.ClsP)
	base := f.Param("base", ir.ClsP)
	entry := f.Entry()
	entry.Add(ir.ClsP, base, f.Long(8))
	second := entry.Add(ir.ClsP, base, f.Long(8))
	entry.Ret(second)
	assert.True(t, GVN(f))
	assert.Equal(t, 1, countOp(f, ir.OAdd), "identical raw-pointer adds merge")

	// A managed pointer (ClsM) carries GC-root identity and stack-map lifetime;
	// GVN must keep the two distinct even though their numeric value is identical.
	g := ir.NewModule().NewFunc("managed", ir.ClsM)
	mbase := g.Param("base", ir.ClsM)
	ge := g.Entry()
	ge.Add(ir.ClsM, mbase, g.Long(8))
	msecond := ge.Add(ir.ClsM, mbase, g.Long(8))
	ge.Ret(msecond)
	assert.False(t, GVN(g))
	assert.Equal(t, 2, countOp(g, ir.OAdd), "managed-pointer adds keep distinct identity")
}

func TestGVNSkipsOverBudgetFunction(t *testing.T) {
	f := ir.NewModule().NewFunc("large", ir.ClsW)
	value := f.Param("value", ir.ClsW)
	entry := f.Entry()
	for i := 0; i <= cfgOptimizationInstructionBudget; i++ {
		value = entry.Add(ir.ClsW, value, f.Word(1))
	}
	entry.Ret(value)

	assert.True(t, gvnOverBudget(f))
	assert.False(t, GVN(f))
}
