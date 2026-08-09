package opt_test

import (
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

func countRet(f *ir.Func) int {
	n := 0
	for _, b := range f.Blocks {
		if b.Jmp.Kind == ir.JmpRet {
			n++
		}
	}
	return n
}

// Two return blocks reached from distinct predecessors merge into one, and each
// path still returns its own value (through the merged block's phi).
func TestTailMergeReturns(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("pick", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	ba, bb, r1, r2 := f.NewBlock("A"), f.NewBlock("B"), f.NewBlock("r1"), f.NewBlock("r2")
	e.Jnz(a, ba, bb)
	ba.Goto(r1)
	bb.Goto(r2)
	r1.Ret(a)
	r2.Ret(b)

	require.True(t, opt.TailMerge(f), "the two return blocks should merge")
	opt.SimplifyCFG(f)
	require.Equal(t, 1, countRet(f), "one shared return block")

	mc, err := interp.New(m)
	require.NoError(t, err)
	got, _ := mc.Call("pick", interp.L(5), interp.L(9)) // a!=0 -> returns a
	require.Equal(t, int64(5), got.I64())
	got, _ = mc.Call("pick", interp.L(0), interp.L(9)) // a==0 -> returns b
	require.Equal(t, int64(9), got.I64())
}

// Void returns merge with no phi, and the function still runs.
func TestTailMergeVoidReturns(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFuncVoid("v").Export()
	a := f.Param("a", ir.ClsL)
	e := f.Entry()
	ba, bb, r1, r2 := f.NewBlock("A"), f.NewBlock("B"), f.NewBlock("r1"), f.NewBlock("r2")
	e.Jnz(a, ba, bb)
	ba.Goto(r1)
	bb.Goto(r2)
	r1.RetVoid()
	r2.RetVoid()

	require.True(t, opt.TailMerge(f))
	opt.SimplifyCFG(f)
	require.Equal(t, 1, countRet(f), "one shared void return")

	mc, err := interp.New(m)
	require.NoError(t, err)
	_, err = mc.Call("v", interp.L(1))
	require.NoError(t, err)
	_, err = mc.Call("v", interp.L(0))
	require.NoError(t, err)
}

// A return block a predecessor reaches together with another (if/else returns)
// is left alone: a phi cannot carry two values on one predecessor's edges.
func TestTailMergeSkipsConflict(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("iff", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	r1, r2 := f.NewBlock("r1"), f.NewBlock("r2")
	e.Jnz(a, r1, r2) // one predecessor reaches both returns
	r1.Ret(a)
	r2.Ret(b)

	require.False(t, opt.TailMerge(f), "conflicting returns must not merge")
	require.Equal(t, 2, countRet(f))
}

// findPhi returns the sole phi in the function's return block.
func retPhi(t *testing.T, f *ir.Func) *ir.Phi {
	t.Helper()
	for _, b := range f.Blocks {
		if b.Jmp.Kind == ir.JmpRet && len(b.Phis) == 1 {
			return b.Phis[0]
		}
	}
	t.Fatal("no return block with a phi")
	return nil
}

// aggRet builds a function that returns an aggregate by value in the
// representation where the parts have NOT been scalarised: Jmp.Arg holds the
// address of the value, and Retty -- documented as the return class only when
// RetAgg is nil -- keeps its zero value.
func aggRet(m *ir.Module, name string, retCls ir.Cls) (*ir.Func, ir.Ref, ir.Ref) {
	f := m.NewFuncVoid(name).Export()
	f.HasRet = true
	f.RetAgg = &ir.AggType{Name: "Big", Size: 32, Align: 8, Opaque: true}
	c := f.Param("c", ir.ClsL)
	x, y := f.Param("x", retCls), f.Param("y", retCls)
	e := f.Entry()
	ba, bb, r1, r2 := f.NewBlock("A"), f.NewBlock("B"), f.NewBlock("r1"), f.NewBlock("r2")
	e.Jnz(c, ba, bb)
	ba.Goto(r1)
	bb.Goto(r2)
	r1.Ret(x)
	r2.Ret(y)
	return f, x, y
}

// The merged phi takes its class from the values it merges, not from f.Retty.
//
// Reading Retty unconditionally was a miscompile, not a tidiness problem. For an
// aggregate returned by value the field is meaningless and reads as ClsW, so the
// phi was 32 bits wide over 64-bit addresses and the copies phi elimination
// inserts truncated them: `add x16, x29, #0x78` to compute a frame address,
// then `mov w21, w16` to move half of it. QuickJS returns JSValue by value
// throughout, which put this in 71 of its functions and segfaulted the
// interpreter on startup.
func TestTailMergePhiClassComesFromTheMergedValues(t *testing.T) {
	m := ir.NewModule()
	f, _, _ := aggRet(m, "aggret", ir.ClsP)
	require.Zero(t, f.Retty, "the premise: an aggregate return leaves Retty unset")

	require.True(t, opt.TailMerge(f), "two empty return blocks should merge")
	phi := retPhi(t, f)
	require.Equal(t, ir.ClsP, phi.Cls, "the phi merges addresses, so it is pointer-wide")
	require.Equal(t, ir.ClsP, f.Temps[phi.To.ID].Cls, "and so is the temp it defines")
	for i, a := range phi.Args {
		require.Equal(t, phi.Cls, f.ClassOf(a), "phi argument %d", i)
	}
}

// A group whose members return different classes has no single class to give the
// phi, so it is not merged. Sharing one epilogue is worth less than a phi that
// merges a value the collector must trace with one it must not, or a 32-bit
// value with a 64-bit one.
func TestTailMergeRefusesGroupsWithDisagreeingClasses(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mixed", ir.ClsL).Export()
	c := f.Param("c", ir.ClsL)
	x, y := f.Param("x", ir.ClsL), f.Param("y", ir.ClsW)
	e := f.Entry()
	ba, bb, r1, r2 := f.NewBlock("A"), f.NewBlock("B"), f.NewBlock("r1"), f.NewBlock("r2")
	e.Jnz(c, ba, bb)
	ba.Goto(r1)
	bb.Goto(r2)
	r1.Ret(x)
	r2.Ret(y)

	require.False(t, opt.TailMerge(f), "a 32-bit and a 64-bit return do not share a phi")
	require.Equal(t, 2, countRet(f), "both return blocks stay")
}
