package opt

import (
	"math"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvalBinop(t *testing.T) {
	cases := []struct {
		op     ir.Op
		cls    ir.Cls
		a, b   int64
		want   int64
		wantOK bool
	}{
		{ir.OAdd, ir.ClsW, 2, 3, 5, true},
		{ir.OAdd, ir.ClsW, 0x7fffffff, 1, math.MinInt32, true}, // word wrap
		{ir.OSub, ir.ClsL, 10, 4, 6, true},
		{ir.OMul, ir.ClsW, 100000, 100000, truncCls(ir.ClsW, 10000000000), true},
		{ir.OAnd, ir.ClsL, 0xff, 0x0f, 0x0f, true},
		{ir.OOr, ir.ClsL, 0xf0, 0x0f, 0xff, true},
		{ir.OXor, ir.ClsL, 0xff, 0x0f, 0xf0, true},
		{ir.OShl, ir.ClsW, 1, 31, math.MinInt32, true},
		{ir.OShr, ir.ClsW, -1, 4, 0x0fffffff, true}, // logical
		{ir.OSar, ir.ClsW, -16, 2, -4, true},        // arithmetic
		{ir.ODiv, ir.ClsL, 20, 7, 2, true},
		{ir.ODiv, ir.ClsL, -20, 7, -2, true},
		{ir.ODiv, ir.ClsL, math.MinInt64, -1, math.MinInt64, true}, // no panic
		{ir.ORem, ir.ClsL, 20, 7, 6, true},
		{ir.ORem, ir.ClsL, math.MinInt64, -1, 0, true},
		{ir.OUDiv, ir.ClsW, -1, 2, 0x7fffffff, true}, // 0xffffffff/2
		{ir.OURem, ir.ClsW, -1, 10, int64(0xffffffff % 10), true},
		{ir.ODiv, ir.ClsL, 5, 0, 0, false}, // divide by zero: not folded
		{ir.ORem, ir.ClsW, 5, 0, 0, false},
		{ir.OUDiv, ir.ClsL, 5, 0, 0, false},
		{ir.OURem, ir.ClsL, 5, 0, 0, false},
	}
	for _, c := range cases {
		got, ok := evalBinop(c.op, c.cls, c.a, c.b)
		assert.Equalf(t, c.wantOK, ok, "%v ok", c.op)
		if c.wantOK {
			assert.Equalf(t, c.want, got, "%v %d,%d", c.op, c.a, c.b)
		}
	}
}

func TestEvalCmp(t *testing.T) {
	assert.Equal(t, int64(1), evalCmp(ir.CmpEq, ir.ClsW, 5, 5))
	assert.Equal(t, int64(0), evalCmp(ir.CmpNe, ir.ClsW, 5, 5))
	assert.Equal(t, int64(1), evalCmp(ir.CmpSlt, ir.ClsW, -1, 1))
	assert.Equal(t, int64(0), evalCmp(ir.CmpUlt, ir.ClsW, -1, 1)) // -1 is huge unsigned
	assert.Equal(t, int64(1), evalCmp(ir.CmpSge, ir.ClsL, 3, 3))
	assert.Equal(t, int64(1), evalCmp(ir.CmpUgt, ir.ClsW, -1, 1))
}

func TestCmpReflexive(t *testing.T) {
	assert.Equal(t, int64(1), cmpReflexive(ir.CmpEq))
	assert.Equal(t, int64(1), cmpReflexive(ir.CmpSle))
	assert.Equal(t, int64(0), cmpReflexive(ir.CmpNe))
	assert.Equal(t, int64(0), cmpReflexive(ir.CmpSlt))
}

func TestFoldConstantPropagation(t *testing.T) {
	// ret ((2+3)*4) -> ret 20
	f := ir.NewModule().NewFunc("k", ir.ClsW)
	e := f.Entry()
	s := e.Add(ir.ClsW, f.Word(2), f.Word(3))
	p := e.Mul(ir.ClsW, s, f.Word(4))
	e.Ret(p)

	Optimize(f)
	assert.Empty(t, e.Instrs, "all arithmetic folded away")
	v, ok := constInt(f, e.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(20), v)
}

func TestFoldIdentities(t *testing.T) {
	check := func(build func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref, wantX bool, wantConst int64) {
		f := ir.NewModule().NewFunc("id", ir.ClsW)
		x := f.Param("x", ir.ClsW)
		e := f.Entry()
		e.Ret(build(f, e, x))
		Fold(f)
		if wantX {
			assert.Equal(t, x, e.Jmp.Arg)
		} else {
			v, ok := constInt(f, e.Jmp.Arg)
			require.True(t, ok)
			assert.Equal(t, wantConst, v)
		}
	}
	check(func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Add(ir.ClsW, x, f.Word(0)) }, true, 0)  // x+0 = x
	check(func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Mul(ir.ClsW, x, f.Word(1)) }, true, 0)  // x*1 = x
	check(func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Mul(ir.ClsW, x, f.Word(0)) }, false, 0) // x*0 = 0
	check(func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Sub(ir.ClsW, x, x) }, false, 0)         // x-x = 0
	check(func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.And(ir.ClsW, x, f.Word(0)) }, false, 0) // x&0 = 0
	check(func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Shl(ir.ClsW, x, f.Word(0)) }, true, 0)  // x<<0 = x
}

func TestFoldPreservesWidthChangingIdentity(t *testing.T) {
	f := ir.NewModule().NewFunc("widen", ir.ClsL)
	value := f.Param("value", ir.ClsW)
	entry := f.Entry()
	widened := entry.Sub(ir.ClsL, value, f.Long(0))
	entry.Ret(widened)

	assert.False(t, Fold(f))
	assert.Len(t, entry.Instrs, 1)
	assert.Equal(t, ir.ClsL, f.ClassOf(entry.Jmp.Arg))
}

func TestFoldMoreIdentities(t *testing.T) {
	// Each builder returns the value to return; want is either x or a constant.
	cases := []struct {
		name  string
		build func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref
		wantX bool
		konst int64
	}{
		{"0+x", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Add(ir.ClsW, f.Word(0), x) }, true, 0},
		{"1*x", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Mul(ir.ClsW, f.Word(1), x) }, true, 0},
		{"0*x", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Mul(ir.ClsW, f.Word(0), x) }, false, 0},
		{"x/1", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Div(ir.ClsW, x, f.Word(1)) }, true, 0},
		{"x udiv 1", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.UDiv(ir.ClsW, x, f.Word(1)) }, true, 0},
		{"x|0", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Or(ir.ClsW, x, f.Word(0)) }, true, 0},
		{"x^0", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Xor(ir.ClsW, x, f.Word(0)) }, true, 0},
		{"x&-1", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.And(ir.ClsW, x, f.Word(-1)) }, true, 0},
		{"-1&x", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.And(ir.ClsW, f.Word(-1), x) }, true, 0},
		{"x&x", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.And(ir.ClsW, x, x) }, true, 0},
		{"x|x", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Or(ir.ClsW, x, x) }, true, 0},
	}
	for _, c := range cases {
		f := ir.NewModule().NewFunc("id", ir.ClsW)
		x := f.Param("x", ir.ClsW)
		e := f.Entry()
		e.Ret(c.build(f, e, x))
		Fold(f)
		if c.wantX {
			assert.Equalf(t, x, e.Jmp.Arg, c.name)
		} else {
			v, ok := constInt(f, e.Jmp.Arg)
			require.Truef(t, ok, c.name)
			assert.Equalf(t, c.konst, v, c.name)
		}
	}
}

func TestFoldSelect(t *testing.T) {
	// A constant-true condition picks the first arm.
	f := ir.NewModule().NewFunc("t", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsW, f.Word(1), x, f.Word(99)))
	Fold(f)
	assert.Equal(t, x, e.Jmp.Arg)

	// A constant-false condition picks the second arm.
	f2 := ir.NewModule().NewFunc("t2", ir.ClsW)
	y := f2.Param("y", ir.ClsW)
	e2 := f2.Entry()
	e2.Ret(e2.Select(ir.ClsW, f2.Word(0), f2.Word(99), y))
	Fold(f2)
	assert.Equal(t, y, e2.Jmp.Arg)

	// Identical arms collapse regardless of the condition.
	f3 := ir.NewModule().NewFunc("t3", ir.ClsW)
	c, z := f3.Param("c", ir.ClsW), f3.Param("z", ir.ClsW)
	e3 := f3.Entry()
	e3.Ret(e3.Select(ir.ClsW, c, z, z))
	Fold(f3)
	assert.Equal(t, z, e3.Jmp.Arg)
}

func TestEvalCmpAllPredicates(t *testing.T) {
	assert.Equal(t, int64(1), evalCmp(ir.CmpSle, ir.ClsW, 3, 3))
	assert.Equal(t, int64(1), evalCmp(ir.CmpSgt, ir.ClsW, 4, 3))
	assert.Equal(t, int64(0), evalCmp(ir.CmpSgt, ir.ClsW, 3, 4))
	assert.Equal(t, int64(1), evalCmp(ir.CmpUle, ir.ClsW, 3, 3))
	assert.Equal(t, int64(1), evalCmp(ir.CmpUge, ir.ClsW, 4, 3))
}

func TestOptimizeModule(t *testing.T) {
	m := ir.NewModule()
	for _, name := range []string{"a", "b"} {
		f := m.NewFunc(name, ir.ClsW)
		e := f.Entry()
		e.Ret(e.Add(ir.ClsW, f.Word(2), f.Word(3)))
	}
	OptimizeModule(m)
	for _, f := range m.Funcs {
		assert.Empty(t, f.Blocks[0].Instrs, "constant add folded in %s", f.Name)
		v, ok := constInt(f, f.Blocks[0].Jmp.Arg)
		require.True(t, ok)
		assert.Equal(t, int64(5), v)
	}
}

func TestFoldReflexiveCompare(t *testing.T) {
	f := ir.NewModule().NewFunc("c", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Cmp(ir.CmpEq, ir.ClsW, x, x))
	Fold(f)
	v, ok := constInt(f, e.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(1), v)
}

func TestFoldRedundantBooleanReTestRemoved(t *testing.T) {
	// (a == b) is already 0/1, so ((a==b) != 0) is just (a==b).
	f := ir.NewModule().NewFunc("c", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	inner := e.Cmp(ir.CmpEq, ir.ClsW, a, b)
	e.Ret(e.Cmp(ir.CmpNe, ir.ClsW, inner, f.Word(0)))

	assert.True(t, Fold(f))
	assert.Equal(t, 1, countOp(f, ir.OCmp), "the redundant != 0 re-test is removed")
	assert.Equal(t, inner, e.Jmp.Arg, "the return value is the original comparison")
}

func TestFoldAndOneIsBoolean(t *testing.T) {
	// (x & 1) is 0/1, so ((x & 1) != 0) folds back to (x & 1).
	f := ir.NewModule().NewFunc("c", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	bit := e.And(ir.ClsW, x, f.Word(1))
	e.Ret(e.Cmp(ir.CmpNe, ir.ClsW, bit, f.Word(0)))

	assert.True(t, Fold(f))
	assert.Equal(t, 0, countOp(f, ir.OCmp), "the != 0 on a one-bit value is removed")
	assert.Equal(t, bit, e.Jmp.Arg)
}

func TestFoldNonBooleanCompareKept(t *testing.T) {
	// (a + b) is not a boolean, so ((a+b) != 0) must be preserved.
	f := ir.NewModule().NewFunc("c", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	sum := e.Add(ir.ClsW, a, b)
	e.Ret(e.Cmp(ir.CmpNe, ir.ClsW, sum, f.Word(0)))

	Fold(f)
	assert.Equal(t, 1, countOp(f, ir.OCmp), "a compare of a non-boolean against 0 stays")
}

func TestFoldBranchThroughNeZero(t *testing.T) {
	// jnz (x != 0), T, F  ->  jnz x, T, F  (any x).
	f := ir.NewModule().NewFuncVoid("b")
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	tb := f.NewBlock("t")
	fb := f.NewBlock("f")
	e.Jnz(e.Cmp(ir.CmpNe, ir.ClsW, x, f.Word(0)), tb, fb)
	tb.RetVoid()
	fb.RetVoid()

	assert.True(t, Fold(f))
	assert.Equal(t, x, e.Jmp.Arg, "the branch tests x directly")
	assert.Equal(t, tb, e.Jmp.To, "!= 0 keeps the arms")
	// The compare is now unused; DCE (a later pass) removes it, not Fold itself.
}

func TestFoldBranchThroughEqZeroSwapsArms(t *testing.T) {
	// jnz (x == 0), T, F  ->  jnz x, F, T.
	f := ir.NewModule().NewFuncVoid("b")
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	tb := f.NewBlock("t")
	fb := f.NewBlock("f")
	e.Jnz(e.Cmp(ir.CmpEq, ir.ClsW, x, f.Word(0)), tb, fb)
	tb.RetVoid()
	fb.RetVoid()

	assert.True(t, Fold(f))
	assert.Equal(t, x, e.Jmp.Arg)
	assert.Equal(t, fb, e.Jmp.To, "== 0 swaps the arms")
	assert.Equal(t, tb, e.Jmp.To2)
}

func TestFoldDivByZeroKept(t *testing.T) {
	f := ir.NewModule().NewFunc("d", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Div(ir.ClsW, f.Word(10), f.Word(0)))
	changed := Fold(f)
	assert.False(t, changed, "division by zero must not be folded")
	require.Len(t, e.Instrs, 1)
	assert.Equal(t, ir.ODiv, e.Instrs[0].Op)
}

func TestFoldNegAndSkipsResultlessInstr(t *testing.T) {
	// neg of a constant folds; the store (no result) is skipped but its operand
	// is rewritten to the folded constant.
	f := ir.NewModule().NewFuncVoid("s")
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	n := e.Neg(ir.ClsW, f.Word(5))
	e.Store(n, p)
	e.RetVoid()

	assert.True(t, Fold(f))
	require.Len(t, e.Instrs, 1)
	assert.True(t, e.Instrs[0].Op.IsStore())
	v, ok := constInt(f, e.Instrs[0].Args[0])
	require.True(t, ok)
	assert.Equal(t, int64(-5), v)
}

func TestFoldNoChangeReturnsFalse(t *testing.T) {
	f := ir.NewModule().NewFunc("n", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b)) // both non-constant
	assert.False(t, Fold(f))
}
