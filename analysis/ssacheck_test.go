package analysis

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// TestVerifyDominanceValid accepts a well-formed diamond whose two sides each
// define a value that a phi merges and the join then uses.
func TestVerifyDominanceValid(t *testing.T) {
	f := NewModuleFunc()
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	c := f.NewBlock("c")
	start.Jnz(cond, a, b)
	xa := a.Add(ir.ClsW, cond, cond)
	a.Goto(c)
	xb := b.Sub(ir.ClsW, cond, cond)
	b.Goto(c)
	x := c.Phi(ir.ClsW, ir.PhiEdge{From: a, Val: xa}, ir.PhiEdge{From: b, Val: xb})
	c.Ret(x)

	require.NoError(t, VerifyDominance(f))
}

// TestVerifyDominanceCatchesUndominatedUse is the gap that let the reverted jump
// threading through: a value defined only on one arm of a diamond, used in the
// join. ir.Verify passes it (single def, the use is "defined", no phi to
// disagree with the CFG), but the def does not dominate the use.
func TestVerifyDominanceCatchesUndominatedUse(t *testing.T) {
	f := NewModuleFunc()
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	c := f.NewBlock("c")
	start.Jnz(cond, a, b)
	x := a.Add(ir.ClsW, cond, cond) // defined only on the a-path
	a.Goto(c)
	b.Goto(c)
	y := c.Add(ir.ClsW, x, cond) // uses x, but a does not dominate c
	c.Ret(y)

	require.NoError(t, ir.Verify(f), "the structural verifier does not see this")
	require.Error(t, VerifyDominance(f), "the dominance verifier must")
}

// TestVerifyDominanceSameBlockOrder rejects a use that precedes its definition
// within a single block, which the block-level dominance case must catch.
func TestVerifyDominanceSameBlockOrder(t *testing.T) {
	f := NewModuleFunc()
	p := f.Param("p", ir.ClsL)
	start := f.Entry()
	// Read a temp id that is only defined later in the same block. Build the two
	// instructions, then swap them so the use sits before the def.
	def := start.Add(ir.ClsL, p, p)
	use := start.Add(ir.ClsL, def, p)
	start.Instrs[0], start.Instrs[1] = start.Instrs[1], start.Instrs[0]
	start.Ret(use)

	require.Error(t, VerifyDominance(f))
}

// TestVerifyDominanceAllocaNonDominating does not flag an alloca address used in
// a block the alloca does not dominate: a fixed-size stack slot is reserved for
// the whole frame and its address is a frame-relative constant, which the
// setjmp/EC_TAG restart pattern relies on (a local declared in one switch arm,
// read after a longjmp re-enters another arm).
func TestVerifyDominanceAllocaNonDominating(t *testing.T) {
	f := NewModuleFunc()
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	c := f.NewBlock("c")
	start.Jnz(cond, a, b)
	buf := a.Alloc(4, 4) // stack slot allocated only on the a-arm
	a.Goto(c)
	b.Goto(c)
	x := c.Load(ir.ClsW, buf) // read in the join, which a does not dominate
	c.Ret(x)

	require.NoError(t, VerifyDominance(f))
}

// TestVerifyDominanceUnreachableUseIgnored does not flag a use in an unreachable
// block: it never executes, and SimplifyCFG removes it.
func TestVerifyDominanceUnreachableUseIgnored(t *testing.T) {
	f := NewModuleFunc()
	p := f.Param("p", ir.ClsL)
	start := f.Entry()
	start.Ret(p)
	dead := f.NewBlock("dead")
	x := dead.Add(ir.ClsL, p, p)
	_ = dead.Add(ir.ClsL, x, x)
	dead.RetVoid()

	require.NoError(t, VerifyDominance(f))
}
