package arm64_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func compile(t *testing.T, f *ir.Func) string {
	t.Helper()
	asm, err := arm64.Compile(f)
	require.NoError(t, err)
	return asm
}

func TestFloatArithMnemonics(t *testing.T) {
	f := ir.NewModule().NewFunc("ops", ir.ClsD)
	a := f.Param("a", ir.ClsD)
	b := f.Param("b", ir.ClsD)
	e := f.Entry()
	e.Add(ir.ClsD, a, b)
	e.Sub(ir.ClsD, a, b)
	e.Mul(ir.ClsD, a, b)
	e.Div(ir.ClsD, a, b)
	e.Ret(e.Neg(ir.ClsD, a))

	asm := compile(t, f)
	for _, mn := range []string{"fadd d", "fsub d", "fmul d", "fdiv d", "fneg d"} {
		assert.Containsf(t, asm, mn, "missing %q", mn)
	}
}

func TestFloatConversionMnemonics(t *testing.T) {
	f := ir.NewModule().NewFunc("conv", ir.ClsD)
	d := f.Param("d", ir.ClsD)
	s := f.Param("s", ir.ClsS)
	n := f.Param("n", ir.ClsL)
	w := f.Param("w", ir.ClsW)
	e := f.Entry()
	e.Stosi(ir.ClsL, d) // fcvtzs
	e.Stoui(ir.ClsL, d) // fcvtzu
	e.Sltof(ir.ClsD, n) // scvtf
	e.Ultof(ir.ClsD, n) // ucvtf
	e.Exts(s)           // fcvt d, s
	e.Truncd(d)         // fcvt s, d
	e.Cast(ir.ClsL, d)  // fmov x, d
	e.Extsb(ir.ClsW, w) // sxtb
	e.Extub(ir.ClsW, w) // uxtb
	e.Extsh(ir.ClsW, w) // sxth
	e.Extuh(ir.ClsW, w) // uxth
	e.Extsw(ir.ClsL, w) // sxtw
	e.Extuw(ir.ClsL, w) // mov (zero-extend)
	e.RetVoid()

	asm := compile(t, f)
	for _, mn := range []string{
		"fcvtzs x", "fcvtzu x", "scvtf d", "ucvtf d",
		"fcvt d", "fcvt s", "fmov x",
		"sxtb ", "uxtb ", "sxth ", "uxth ", "sxtw ",
	} {
		assert.Containsf(t, asm, mn, "missing %q", mn)
	}
}

func TestFloatCompareConditions(t *testing.T) {
	preds := []struct {
		p    ir.Cmp
		cond string
	}{
		{ir.CmpFeq, "eq"}, {ir.CmpFne, "ne"}, {ir.CmpFlt, "mi"}, {ir.CmpFle, "ls"},
		{ir.CmpFgt, "gt"}, {ir.CmpFge, "ge"}, {ir.CmpFo, "vc"}, {ir.CmpFuo, "vs"},
	}
	f := ir.NewModule().NewFunc("cmps", ir.ClsW)
	a := f.Param("a", ir.ClsD)
	b := f.Param("b", ir.ClsD)
	e := f.Entry()
	acc := f.Word(0)
	for _, pc := range preds {
		acc = e.Add(ir.ClsW, acc, e.Cmp(pc.p, ir.ClsW, a, b))
	}
	e.Ret(acc)

	asm := compile(t, f)
	assert.Contains(t, asm, "fcmp d")
	for _, pc := range preds {
		assert.Containsf(t, asm, "cset w9, "+pc.cond, "predicate %v", pc.p)
	}
}

func TestFloatLoadStoreAndConst(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("mem")
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	e.Store(f.Double(1.5), p) // str d, + fmov d,x for the constant
	e.Store(f.Single(2.5), p) // str s, + fmov s,w
	e.Load(ir.ClsD, p)        // ldr d
	e.Load(ir.ClsS, p)        // ldr s
	e.RetVoid()

	asm := compile(t, f)
	for _, mn := range []string{"str d", "str s", "ldr d", "ldr s", "fmov d", "fmov s"} {
		assert.Containsf(t, asm, mn, "missing %q", mn)
	}
}

func TestFloatSpill(t *testing.T) {
	// Keep many doubles live to force spilling of SIMD registers.
	const n = 40
	f := ir.NewModule().NewFunc("bigf", ir.ClsD).Export()
	x := f.Param("x", ir.ClsD)
	e := f.Entry()
	vals := make([]ir.Ref, n)
	for i := 0; i < n; i++ {
		vals[i] = e.Add(ir.ClsD, x, f.Double(float64(i)))
	}
	acc := f.Double(0)
	for i := 0; i < n; i++ {
		acc = e.Add(ir.ClsD, acc, vals[i])
	}
	e.Ret(acc)

	asm := compile(t, f)
	// Spilled doubles are stored to and reloaded from the stack.
	assert.True(t, strings.Contains(asm, "str d") && strings.Contains(asm, "ldr d"),
		"expected float spill/reload")
}
