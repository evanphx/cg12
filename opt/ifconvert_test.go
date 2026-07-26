package opt_test

import (
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// countOp totals occurrences of op across f.
func countOp(f *ir.Func, op ir.Op) int {
	n := 0
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if b.Instrs[i].Op == op {
				n++
			}
		}
	}
	return n
}

// hasJnz reports whether any block still ends in a conditional branch.
func hasJnz(f *ir.Func) bool {
	for _, b := range f.Blocks {
		if b.Jmp.Kind == ir.JmpJnz {
			return true
		}
	}
	return false
}

// A c?a:b diamond with empty arms (a plain phi merge) converts to a single
// select and no branch, and computes the same result either way.
func TestIfConvertDiamond(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mn", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	h := f.Entry()
	tb, fb, j := f.NewBlock("t"), f.NewBlock("f"), f.NewBlock("j")
	h.Jnz(h.Cmp(ir.CmpSlt, ir.ClsL, a, b), tb, fb)
	tb.Goto(j)
	fb.Goto(j)
	r := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: a}, ir.PhiEdge{From: fb, Val: b})
	j.Ret(r)

	require.True(t, opt.IfConvert(f), "the diamond should convert")
	opt.SimplifyCFG(f)
	require.Equal(t, 1, countOp(f, ir.OSel), "one select")
	require.False(t, hasJnz(f), "the branch should be gone")

	mc, err := interp.New(m)
	require.NoError(t, err)
	for _, tc := range []struct{ a, b, want int64 }{{3, 9, 3}, {9, 3, 3}, {-5, 2, -5}} {
		got, err := mc.Call("mn", interp.L(tc.a), interp.L(tc.b))
		require.NoError(t, err)
		require.Equal(t, tc.want, got.I64(), "mn(%d,%d)", tc.a, tc.b)
	}
}

// Arms that compute values (k=2) still convert, and the select polarity is
// right: the true arm's value is chosen when the condition holds.
func TestIfConvertDiamondWithArms(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("pick", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	h := f.Entry()
	tb, fb, j := f.NewBlock("t"), f.NewBlock("f"), f.NewBlock("j")
	h.Jnz(h.Cmp(ir.CmpSgt, ir.ClsL, a, b), tb, fb)
	sum := tb.Add(ir.ClsL, a, b)
	tb.Goto(j)
	dif := fb.Sub(ir.ClsL, a, b)
	fb.Goto(j)
	r := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: sum}, ir.PhiEdge{From: fb, Val: dif})
	j.Ret(r)

	require.True(t, opt.IfConvert(f))
	opt.SimplifyCFG(f)
	require.Equal(t, 1, countOp(f, ir.OSel))
	require.False(t, hasJnz(f))

	mc, err := interp.New(m)
	require.NoError(t, err)
	// a>b -> a+b, else a-b.
	got, _ := mc.Call("pick", interp.L(9), interp.L(4))
	require.Equal(t, int64(13), got.I64())
	got, _ = mc.Call("pick", interp.L(4), interp.L(9))
	require.Equal(t, int64(-5), got.I64())
}

// A triangle -- an `if` with no else, one arm going straight to the join --
// converts too, selecting between the arm's value and the fall-through value.
func TestIfConvertTriangle(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("relu", ir.ClsL).Export()
	a := f.Param("a", ir.ClsL)
	h := f.Entry()
	tb, j := f.NewBlock("t"), f.NewBlock("j")
	// if (a > 0) t = a*2; result = phi(t, a)
	h.Jnz(h.Cmp(ir.CmpSgt, ir.ClsL, a, f.Long(0)), tb, j)
	dbl := tb.Mul(ir.ClsL, a, f.Long(2))
	tb.Goto(j)
	r := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: dbl}, ir.PhiEdge{From: h, Val: a})
	j.Ret(r)

	require.True(t, opt.IfConvert(f))
	opt.SimplifyCFG(f)
	require.Equal(t, 1, countOp(f, ir.OSel))
	require.False(t, hasJnz(f))

	mc, err := interp.New(m)
	require.NoError(t, err)
	got, _ := mc.Call("relu", interp.L(5))
	require.Equal(t, int64(10), got.I64()) // 5>0 -> 5*2
	got, _ = mc.Call("relu", interp.L(-5))
	require.Equal(t, int64(-5), got.I64()) // not >0 -> a
}

// A pure phi-diamond with two phis (p=2, k=0) converts to two selects.
func TestIfConvertMultiPhi(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("swap2", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	c := f.Param("c", ir.ClsL)
	h := f.Entry()
	tb, fb, j := f.NewBlock("t"), f.NewBlock("f"), f.NewBlock("j")
	h.Jnz(c, tb, fb)
	tb.Goto(j)
	fb.Goto(j)
	x := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: a}, ir.PhiEdge{From: fb, Val: b})
	y := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: b}, ir.PhiEdge{From: fb, Val: a})
	j.Ret(j.Sub(ir.ClsL, x, y))

	require.True(t, opt.IfConvert(f))
	opt.SimplifyCFG(f)
	require.Equal(t, 2, countOp(f, ir.OSel), "two selects for two phis")

	mc, err := interp.New(m)
	require.NoError(t, err)
	got, _ := mc.Call("swap2", interp.L(10), interp.L(3), interp.L(1)) // c!=0 -> x=a,y=b -> 7
	require.Equal(t, int64(7), got.I64())
	got, _ = mc.Call("swap2", interp.L(10), interp.L(3), interp.L(0)) // c==0 -> x=b,y=a -> -7
	require.Equal(t, int64(-7), got.I64())
}

// An arm with a load must NOT convert: speculating a load could fault.
func TestIfConvertRejectsLoad(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("guarded", ir.ClsL).Export()
	c, p := f.Param("c", ir.ClsL), f.Param("p", ir.ClsL)
	h := f.Entry()
	tb, fb, j := f.NewBlock("t"), f.NewBlock("f"), f.NewBlock("j")
	h.Jnz(c, tb, fb)
	ld := tb.Load(ir.ClsL, p) // must stay guarded
	tb.Goto(j)
	fb.Goto(j)
	r := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: ld}, ir.PhiEdge{From: fb, Val: f.Long(0)})
	j.Ret(r)

	require.False(t, opt.IfConvert(f), "a load arm must not convert")
	require.Equal(t, 0, countOp(f, ir.OSel))
	require.True(t, hasJnz(f), "the guard branch must remain")
}

// Arms exceeding the speculated-instruction budget are left as branches.
func TestIfConvertRejectsOverBudget(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("big", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	c := f.Param("c", ir.ClsL)
	h := f.Entry()
	tb, fb, j := f.NewBlock("t"), f.NewBlock("f"), f.NewBlock("j")
	h.Jnz(c, tb, fb)
	// Four instructions in the true arm -> k=4 > 3.
	v := tb.Add(ir.ClsL, a, b)
	v = tb.Add(ir.ClsL, v, a)
	v = tb.Add(ir.ClsL, v, b)
	v = tb.Add(ir.ClsL, v, a)
	tb.Goto(j)
	fb.Goto(j)
	r := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: v}, ir.PhiEdge{From: fb, Val: b})
	j.Ret(r)

	require.False(t, opt.IfConvert(f), "over-budget arms must not convert")
	require.Equal(t, 0, countOp(f, ir.OSel))
}

// More phis than the select budget is rejected.
func TestIfConvertRejectsTooManyPhis(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("three", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	c := f.Param("c", ir.ClsL)
	h := f.Entry()
	tb, fb, j := f.NewBlock("t"), f.NewBlock("f"), f.NewBlock("j")
	h.Jnz(c, tb, fb)
	tb.Goto(j)
	fb.Goto(j)
	x := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: a}, ir.PhiEdge{From: fb, Val: b})
	y := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: b}, ir.PhiEdge{From: fb, Val: a})
	z := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: a}, ir.PhiEdge{From: fb, Val: a})
	j.Ret(j.Add(ir.ClsL, j.Add(ir.ClsL, x, y), z))

	require.False(t, opt.IfConvert(f), "three phis exceed the select budget")
	require.Equal(t, 0, countOp(f, ir.OSel))
}

// A join reached by a third, unrelated predecessor keeps that predecessor's phi
// entry; only the two diamond edges are replaced by the select.
func TestIfConvertPreservesOtherPreds(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("merge3", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	sel := f.Param("sel", ir.ClsL)
	pre := f.Entry()
	h, side, j := f.NewBlock("h"), f.NewBlock("side"), f.NewBlock("j")
	tb, fb := f.NewBlock("t"), f.NewBlock("f")
	// pre: if sel goto side else h ; side jumps to j with value 100.
	pre.Jnz(sel, side, h)
	side.Goto(j)
	// h: the diamond on a<b.
	h.Jnz(h.Cmp(ir.CmpSlt, ir.ClsL, a, b), tb, fb)
	tb.Goto(j)
	fb.Goto(j)
	r := j.Phi(ir.ClsL,
		ir.PhiEdge{From: side, Val: f.Long(100)},
		ir.PhiEdge{From: tb, Val: a},
		ir.PhiEdge{From: fb, Val: b},
	)
	j.Ret(r)

	require.True(t, opt.IfConvert(f))
	opt.SimplifyCFG(f)
	// The inner diamond converts; that turns h into a straight speculatable run,
	// so the outer diamond on sel converts too -- two selects, both preserving the
	// third (side) predecessor's contribution, which the results below confirm.
	require.GreaterOrEqual(t, countOp(f, ir.OSel), 1, "at least the inner diamond converts")

	mc, err := interp.New(m)
	require.NoError(t, err)
	got, _ := mc.Call("merge3", interp.L(3), interp.L(9), interp.L(1)) // sel!=0 -> 100
	require.Equal(t, int64(100), got.I64())
	got, _ = mc.Call("merge3", interp.L(3), interp.L(9), interp.L(0)) // sel==0 -> min(3,9)=3
	require.Equal(t, int64(3), got.I64())
	got, _ = mc.Call("merge3", interp.L(9), interp.L(3), interp.L(0)) // sel==0 -> min(9,3)=3
	require.Equal(t, int64(3), got.I64())
}

// The env switch disables the pass, for A/B measurement.
func TestIfConvertEnvDisable(t *testing.T) {
	t.Setenv("CG12_NO_IFCONVERT", "1")
	m := ir.NewModule()
	f := m.NewFunc("mn", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	h := f.Entry()
	tb, fb, j := f.NewBlock("t"), f.NewBlock("f"), f.NewBlock("j")
	h.Jnz(h.Cmp(ir.CmpSlt, ir.ClsL, a, b), tb, fb)
	tb.Goto(j)
	fb.Goto(j)
	r := j.Phi(ir.ClsL, ir.PhiEdge{From: tb, Val: a}, ir.PhiEdge{From: fb, Val: b})
	j.Ret(r)

	require.False(t, opt.IfConvert(f), "disabled by env")
	require.Equal(t, 0, countOp(f, ir.OSel))
}
