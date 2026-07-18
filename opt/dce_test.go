package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDCERemovesDeadInstr(t *testing.T) {
	f := ir.NewModule().NewFunc("d", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Add(ir.ClsW, a, b) // dead: result never used
	e.Ret(b)

	assert.True(t, DCE(f))
	assert.Empty(t, e.Instrs)
}

func TestDCEChainCollapses(t *testing.T) {
	// A dead value feeding another dead value: both go.
	f := ir.NewModule().NewFunc("d", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	e := f.Entry()
	t1 := e.Add(ir.ClsW, a, a)
	e.Mul(ir.ClsW, t1, a) // dead, uses t1
	e.Ret(a)

	assert.True(t, DCE(f))
	assert.Empty(t, e.Instrs, "both instructions removed via fixpoint")
}

func TestDCEKeepsSideEffects(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("d")
	a := f.Param("a", ir.ClsL)
	e := f.Entry()
	e.Store(f.Word(1), a)             // side effect: kept
	e.Call(ir.ClsW, f.Sym("g", 0), a) // result unused but call kept
	e.RetVoid()

	DCE(f)
	assert.Len(t, e.Instrs, 2, "store and call preserved")
}

func TestDCEKeepsSafepoint(t *testing.T) {
	f := ir.NewModule().NewFunc("d", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	e := f.Entry()
	e.Safepoint() // no result, but pins a stack map: must be kept
	e.Ret(a)

	DCE(f)
	require.Len(t, e.Instrs, 1)
	assert.Equal(t, ir.OSafepoint, e.Instrs[0].Op, "safepoint survives DCE")
}

func TestDCERemovesDeadPhi(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("d")
	c := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	join := f.NewBlock("join")
	start.Jnz(c, a, b)
	a.Goto(join)
	b.Goto(join)
	join.Phi(ir.ClsW, ir.PhiEdge{From: a, Val: f.Word(1)}, ir.PhiEdge{From: b, Val: f.Word(2)}) // unused
	join.RetVoid()

	assert.True(t, DCE(f))
	assert.Empty(t, join.Phis)
}

func TestIsDeadClassification(t *testing.T) {
	uses := map[uint32]int{}
	// No side effect and no result temporary: dead weight.
	assert.True(t, isDead(&ir.Instr{Op: ir.OArg}, uses))
	// Side-effecting op is never dead.
	assert.False(t, isDead(&ir.Instr{Op: ir.OStorew}, uses))
	assert.False(t, isDead(&ir.Instr{Op: ir.OCall}, uses))
}

// An intrinsic's effect registry entry decides whether DCE may remove it. The
// two stack intrinsics sit on opposite sides of that line: stacksave has a
// result but no side effect, so an unused one is dead; stackrestore is a side
// effect, so it is kept even though it yields nothing.
func TestDCEIntrinsicEffects(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("d")
	e := f.Entry()
	e.StackSave()                              // result unused: removable
	e.IntrinsicVoid("stackrestore", f.Word(0)) // side effect: kept
	e.RetVoid()

	assert.True(t, DCE(f))
	require.Len(t, e.Instrs, 1)
	assert.Equal(t, "stackrestore", e.Instrs[0].Intrin.Name, "stackrestore survives DCE")
}

func TestDCENoChange(t *testing.T) {
	f := ir.NewModule().NewFunc("d", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b))
	assert.False(t, DCE(f))
}
