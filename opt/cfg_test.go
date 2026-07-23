package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSimplifyConstantBranch(t *testing.T) {
	f := ir.NewModule().NewFunc("b", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	start.Jnz(f.Word(1), a, b) // always takes a
	a.Ret(f.Word(10))
	b.Ret(f.Word(20))

	assert.True(t, SimplifyCFG(f))
	// jnz(1) folds to jmp a; b became unreachable and was removed; a (now start's
	// sole successor with one predecessor) coalesced into start.
	assert.Len(t, f.Blocks, 1)
	assert.Same(t, start, f.Blocks[0])
	assert.Equal(t, ir.JmpRet, start.Jmp.Kind)
	for _, blk := range f.Blocks {
		assert.NotSame(t, b, blk)
	}
}

func TestSimplifyThreadsEmptyForwardingArms(t *testing.T) {
	// `cond ? {} : {}` where both empty arms continue to the same join: the branch
	// threads past the empty forwarding blocks, its two targets coincide, and the
	// conditional collapses to an unconditional jump -- so the condition is dead.
	// This is the shape (a flag whose two arms reach one block) that filled Ruby's
	// dispatch loop with pointless compare-and-branch pairs.
	f := ir.NewModule().NewFunc("b", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	thenB := f.NewBlock("then")
	elseB := f.NewBlock("else")
	join := f.NewBlock("join")
	start.Jnz(cond, thenB, elseB)
	thenB.Goto(join) // empty forwarding block
	elseB.Goto(join) // empty forwarding block
	join.Ret(f.Word(7))

	assert.True(t, SimplifyCFG(f))
	// Threaded to jnz(c, join, join) -> jmp join; then/else unreachable and removed;
	// join coalesced into start, which now just returns.
	assert.Len(t, f.Blocks, 1)
	assert.Same(t, start, f.Blocks[0])
	assert.Equal(t, ir.JmpRet, start.Jmp.Kind)
}

func TestSimplifyConstantBranchFalse(t *testing.T) {
	f := ir.NewModule().NewFunc("b", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	start.Jnz(f.Word(0), a, b) // always takes b
	a.Ret(f.Word(10))
	b.Ret(f.Word(20))

	SimplifyCFG(f)
	// The branch takes b; a is removed and b coalesces into start.
	assert.Len(t, f.Blocks, 1)
	assert.Same(t, start, f.Blocks[0])
	assert.Equal(t, ir.JmpRet, start.Jmp.Kind)
}

func TestSimplifyDegenerateBranch(t *testing.T) {
	f := ir.NewModule().NewFunc("b", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	only := f.NewBlock("only")
	start.Jnz(cond, only, only) // both edges identical
	only.Ret(f.Word(1))

	assert.True(t, SimplifyCFG(f))
	// The degenerate branch folds to an unconditional jump, then `only` — its sole
	// successor, with a single predecessor — is coalesced into start.
	assert.Len(t, f.Blocks, 1)
	assert.Same(t, start, f.Blocks[0])
	assert.Equal(t, ir.JmpRet, start.Jmp.Kind)
}

func TestSimplifyPhiRepairAfterDeadBranch(t *testing.T) {
	// The false arm becomes unreachable; the join's phi must drop that operand
	// and collapse to the surviving value.
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	join := f.NewBlock("join")
	start.Jnz(f.Word(1), a, b)
	a.Goto(join)
	b.Goto(join)
	r := join.Phi(ir.ClsW, ir.PhiEdge{From: a, Val: f.Word(10)}, ir.PhiEdge{From: b, Val: f.Word(20)})
	join.Ret(r)

	SimplifyCFG(f)
	assert.Empty(t, join.Phis, "single-edge phi collapsed")
	v, ok := constInt(f, join.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(10), v)
}

func TestSimplifyTrivialPhi(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	start := f.Entry()
	join := f.NewBlock("join")
	start.Goto(join)
	r := join.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(5)})
	join.Ret(r)

	assert.True(t, SimplifyCFG(f))
	assert.Empty(t, join.Phis)
	v, ok := constInt(f, join.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(5), v)
}

func TestSimplifyNoChange(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	start.Jnz(cond, a, b)
	a.Ret(f.Word(1))
	b.Ret(f.Word(2))
	assert.False(t, SimplifyCFG(f))
}
