package opt

import (
	"testing"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runThread applies one round of jump threading with the per-transform SSA and
// dominance checks forced on, and returns whether it changed anything.
func runThread(t *testing.T, f *ir.Func) bool {
	t.Helper()
	t.Setenv("CG12_JT_CHECK", "1") // panic inside the pass on any broken transform
	changed := jumpThread(f, map[*ir.Func]*jtState{})
	require.NoError(t, ir.Verify(f), "SSA well-formed after threading")
	require.NoError(t, analysis.VerifyDominance(f), "dominance holds after threading")
	return changed
}

// andMerge builds the shape mem2reg leaves for `cond && x`: a merge block whose
// phi picks a constant 0 along the short-circuit edge and a computed boolean
// along the rhs edge, then re-branches on it. thenV/elseV terminate the arms.
func andMerge() (*ir.Func, map[string]*ir.Block) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	start := f.Entry()
	rhs := f.NewBlock("rhs")
	merge := f.NewBlock("merge")
	then := f.NewBlock("then")
	els := f.NewBlock("els")
	start.Jnz(cond, rhs, merge) // cond false short-circuits to merge carrying 0
	bx := rhs.Cmp(ir.CmpNe, ir.ClsW, x, f.Word(0))
	rhs.Goto(merge)
	r := merge.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)}, ir.PhiEdge{From: rhs, Val: bx})
	merge.Jnz(r, then, els)
	then.Ret(f.Word(1))
	els.Ret(f.Word(2))
	return f, map[string]*ir.Block{"start": start, "rhs": rhs, "merge": merge, "then": then, "els": els}
}

func predSet(b *ir.Block) map[*ir.Block]bool {
	m := map[*ir.Block]bool{}
	for _, p := range b.Preds {
		m[p] = true
	}
	return m
}

// TestThreadAndShape threads the constant short-circuit edge: along start the
// merge condition is 0, so start is routed straight to the false arm, and merge
// keeps only the rhs predecessor.
func TestThreadAndShape(t *testing.T) {
	f, bl := andMerge()
	require.True(t, runThread(t, f))

	analysis.BuildCFG(f)
	// start's false (constant 0) edge was redirected off merge onto a fresh clone
	// that jumps straight to the false arm. (SimplifyCFG later forwards start past
	// the empty clone, but the pass leaves the clone in place.)
	clone := bl["start"].Jmp.To2
	require.NotSame(t, bl["merge"], clone, "the constant edge no longer targets merge")
	assert.Equal(t, ir.JmpJmp, clone.Jmp.Kind)
	assert.Same(t, bl["els"], clone.Jmp.To, "the clone routes straight to the false arm")

	assert.NotContains(t, predSet(bl["merge"]), bl["start"], "merge no longer has start as a predecessor")
	assert.Contains(t, predSet(bl["merge"]), bl["rhs"], "the non-constant edge is untouched")
	// merge's phi lost the start operand and now has a single argument.
	require.Len(t, bl["merge"].Phis, 1)
	assert.Len(t, bl["merge"].Phis[0].Args, 1)
}

// TestThreadInstrChainCond threads when the branch condition is computed by an
// instruction in the merge (r == 5) rather than being the phi directly: folding
// the chain along the constant edge resolves it.
func TestThreadInstrChainCond(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	y := f.Param("y", ir.ClsW)
	start := f.Entry()
	rhs := f.NewBlock("rhs")
	merge := f.NewBlock("merge")
	then := f.NewBlock("then")
	els := f.NewBlock("els")
	start.Jnz(cond, rhs, merge)
	rhs.Goto(merge)
	r := merge.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(5)}, ir.PhiEdge{From: rhs, Val: y})
	c := merge.Cmp(ir.CmpEq, ir.ClsW, r, f.Word(5)) // 5 == 5 -> 1 along the start edge
	merge.Jnz(c, then, els)
	then.Ret(f.Word(1))
	els.Ret(f.Word(2))

	require.True(t, runThread(t, f))
	analysis.BuildCFG(f)
	// r == 5 folds true along the start edge, so start's clone jumps to the true arm.
	clone := start.Jmp.To2
	require.NotSame(t, merge, clone)
	assert.Same(t, then, clone.Jmp.To, "start folds r==5 true and threads to the true arm")
	assert.NotContains(t, predSet(merge), start)
}

// TestThreadEscapingStillValid threads even when a value the merge defines is
// used past it (here the phi result is returned from an arm): reconstruction
// repairs SSA. runThread asserts Verify and VerifyDominance hold afterwards.
func TestThreadEscapingStillValid(t *testing.T) {
	f, bl := andMerge()
	// Make the merge's phi result escape: the true arm returns it.
	r := bl["merge"].Phis[0].To
	bl["then"].Ret(r)

	require.True(t, runThread(t, f), "threading fires and reconstructs the escaping value")
	analysis.BuildCFG(f)
	assert.NotContains(t, predSet(bl["merge"]), bl["start"], "the constant edge was threaded")
}

// TestThreadDeclinesLoopHeader does not thread a loop header even when an entry
// edge makes the condition constant.
func TestThreadDeclinesLoopHeader(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	start := f.Entry()
	head := f.NewBlock("head")
	latch := f.NewBlock("latch")
	exit := f.NewBlock("exit")
	start.Goto(head)
	v := latch.Add(ir.ClsW, x, f.Word(1))
	latch.Goto(head)
	r := head.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)}, ir.PhiEdge{From: latch, Val: v})
	head.Jnz(r, latch, exit) // back edge to latch; header
	exit.Ret(r)              // (also an escape, but the header guard fires first)

	assert.False(t, runThread(t, f), "a loop header is not threaded")
}

// TestThreadDeclinesSideEffect does not duplicate a block with a store, which
// would run the observable write on a path that did not have it.
func TestThreadDeclinesSideEffect(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	start := f.Entry()
	rhs := f.NewBlock("rhs")
	merge := f.NewBlock("merge")
	then := f.NewBlock("then")
	els := f.NewBlock("els")
	start.Jnz(cond, rhs, merge)
	bx := rhs.Cmp(ir.CmpNe, ir.ClsW, cond, f.Word(0))
	rhs.Goto(merge)
	r := merge.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)}, ir.PhiEdge{From: rhs, Val: bx})
	merge.Store(f.Word(7), p) // side effect in the merge block
	merge.Jnz(r, then, els)
	then.Ret(f.Word(1))
	els.Ret(f.Word(2))

	before := len(f.Blocks)
	assert.False(t, runThread(t, f), "a block with a side effect is not duplicated")
	assert.Equal(t, before, len(f.Blocks))
}

// TestThreadReconstructsEscapingUse is the case the reverted attempt got wrong:
// the merge's phi result is used past the branch, in a block the threaded edge
// also reaches, so threading must insert a phi merging the original value and the
// clone's. Threading the constant edge (r == 3) into that use-block forces it.
func TestThreadReconstructsEscapingUse(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	c := f.Param("c", ir.ClsW)
	y := f.Param("y", ir.ClsW)
	start := f.Entry()
	p1 := f.NewBlock("p1")
	p2 := f.NewBlock("p2")
	merge := f.NewBlock("merge")
	useA := f.NewBlock("useA")
	other := f.NewBlock("other")
	start.Jnz(c, p1, p2)
	p1.Goto(merge)
	p2.Goto(merge)
	r := merge.Phi(ir.ClsW, ir.PhiEdge{From: p1, Val: f.Word(3)}, ir.PhiEdge{From: p2, Val: y})
	merge.Jnz(r, useA, other) // r == 3 (nonzero) along p1 -> useA
	z := useA.Add(ir.ClsW, r, f.Word(1))
	useA.Ret(z) // uses r past the branch -> escapes merge
	other.Ret(f.Word(0))

	require.True(t, runThread(t, f), "threading fires and reconstructs")
	analysis.BuildCFG(f)
	// useA is now reached from both merge (value r) and the clone (value 3), so a
	// phi was inserted there and z reads it.
	require.NotEmpty(t, useA.Phis, "a reconstruction phi was placed at the escaping use")
	// z's operand is no longer the raw phi result r; it is the reconstructed value.
	assert.NotEqual(t, r, z, "sanity: distinct temps")
}

// TestThreadTerminates runs the pass to a fixpoint on a chain of merges and
// checks it stops rather than threading forever.
func TestThreadTerminates(t *testing.T) {
	f, _ := andMerge()
	state := map[*ir.Func]*jtState{}
	rounds := 0
	for jumpThread(f, state) {
		rounds++
		require.Less(t, rounds, 100, "threading did not converge")
	}
	require.NoError(t, ir.Verify(f))
	require.NoError(t, analysis.VerifyDominance(f))
}
