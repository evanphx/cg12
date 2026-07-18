package wasm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/wasm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wasmtime locates a wasmtime binary, or skips the test when none is present.
func wasmtime(t *testing.T) string {
	t.Helper()
	return testenv.Tool(t, "wasmtime")
}

// runFunc compiles m to WAT, then invokes exported function fn with args via
// wasmtime and returns its printed result (a single scalar).
func runFunc(t *testing.T, m *ir.Module, fn string, args ...string) string {
	t.Helper()
	wt := wasmtime(t)
	src, err := wasm.CompileModule(m)
	require.NoErrorf(t, err, "compile:\n%s", src)

	dir := t.TempDir()
	path := filepath.Join(dir, "m.wat")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))

	// -W tail-call=y enables return_call; harmless for programs that don't use it.
	argv := append([]string{"run", "-W", "tail-call=y", "--invoke", fn, path}, args...)
	cmd := exec.Command(wt, argv...)
	var out strings.Builder
	cmd.Stdout = &out // wasmtime prints the result to stdout, warnings to stderr
	require.NoErrorf(t, cmd.Run(), "wasmtime failed for %s\n%s", fn, src)
	return strings.TrimSpace(out.String())
}

// unary builds a module with one exported function taking a single parameter of
// class in, whose body is build(f, entry, param), returning class out.
func unary(name string, in, out ir.Cls, build func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref) *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc(name, out).Export()
	x := f.Param("x", in)
	e := f.Entry()
	e.Ret(build(f, e, x))
	return m
}

func TestE2EArithmetic(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	// ((a + b) * a) - (b / 2)
	s := e.Add(ir.ClsW, a, b)
	p := e.Mul(ir.ClsW, s, a)
	d := e.Div(ir.ClsW, b, f.Word(2))
	e.Ret(e.Sub(ir.ClsW, p, d))

	require.Equal(t, "19", runFunc(t, m, "f", "3", "4")) // (3+4)*3 - 4/2 = 21 - 2 = 19
}

func TestE2ESelect(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("sel", ir.ClsW).Export()
	c, a, b := f.Param("c", ir.ClsW), f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsW, c, a, b))

	require.Equal(t, "7", runFunc(t, m, "sel", "1", "7", "9"))
	require.Equal(t, "9", runFunc(t, m, "sel", "0", "7", "9"))
}

func TestE2ELongArith(t *testing.T) {
	m := unary("dbl", ir.ClsL, ir.ClsL, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Mul(ir.ClsL, x, f.Long(1000000000))
	})
	require.Equal(t, "9000000000", runFunc(t, m, "dbl", "9"))
}

func TestE2EBitwiseAndShifts(t *testing.T) {
	cases := []struct {
		name string
		op   func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref
		in   string
		want string
	}{
		{"and", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.And(ir.ClsW, x, f.Word(0x0f)) }, "255", "15"},
		{"or", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Or(ir.ClsW, x, f.Word(0x10)) }, "1", "17"},
		{"xor", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Xor(ir.ClsW, x, f.Word(0xff)) }, "0", "255"},
		{"shl", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Shl(ir.ClsW, x, f.Word(4)) }, "3", "48"},
		{"shr", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Shr(ir.ClsW, x, f.Word(1)) }, "-2", "2147483647"},
		{"sar", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Sar(ir.ClsW, x, f.Word(1)) }, "-2", "-1"},
		{"neg", func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref { return e.Neg(ir.ClsW, x) }, "7", "-7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := unary(c.name, ir.ClsW, ir.ClsW, c.op)
			require.Equal(t, c.want, runFunc(t, m, c.name, c.in))
		})
	}
}

func TestE2EFloat(t *testing.T) {
	m := unary("half", ir.ClsD, ir.ClsD, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Mul(ir.ClsD, x, f.Double(0.5))
	})
	require.Equal(t, "4.5", runFunc(t, m, "half", "9"))

	mn := unary("fneg", ir.ClsS, ir.ClsS, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Neg(ir.ClsS, x)
	})
	require.Equal(t, "-2.5", runFunc(t, mn, "fneg", "2.5"))
}

func TestE2ECompare(t *testing.T) {
	cases := []struct {
		name string
		pred ir.Cmp
		a, b string
		want string
	}{
		{"slt_true", ir.CmpSlt, "-3", "2", "1"},
		{"slt_false", ir.CmpSlt, "5", "2", "0"},
		{" eq", ir.CmpEq, "7", "7", "1"},
		{"uge", ir.CmpUge, "-1", "1", "1"}, // -1 unsigned is max
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := ir.NewModule()
			f := m.NewFunc("cmp", ir.ClsW).Export()
			a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
			e := f.Entry()
			e.Ret(e.Cmp(c.pred, ir.ClsW, a, b))
			require.Equal(t, c.want, runFunc(t, m, "cmp", c.a, c.b))
		})
	}
}

func TestE2EFloatCompare(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("flt", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsD), f.Param("b", ir.ClsD)
	e := f.Entry()
	e.Ret(e.Cmp(ir.CmpFlt, ir.ClsW, a, b))
	require.Equal(t, "1", runFunc(t, m, "flt", "1.5", "2.5"))
	require.Equal(t, "0", runFunc(t, m, "flt", "3.5", "2.5"))
}

func TestE2EConversions(t *testing.T) {
	// signed int -> double -> back to signed int, and a widening.
	sf := unary("sf", ir.ClsW, ir.ClsD, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Sltof(ir.ClsD, x)
	})
	require.Equal(t, "42", runFunc(t, sf, "sf", "42"))

	fs := unary("fs", ir.ClsD, ir.ClsW, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Stosi(ir.ClsW, x)
	})
	require.Equal(t, "3", runFunc(t, fs, "fs", "3.9"))

	// sign-extend a byte value: 0xFF as a byte is -1.
	sb := unary("sb", ir.ClsW, ir.ClsW, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Extsb(ir.ClsW, x)
	})
	require.Equal(t, "-1", runFunc(t, sb, "sb", "255"))

	// zero-extend a byte into a long.
	ub := unary("ub", ir.ClsW, ir.ClsL, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Extub(ir.ClsL, x)
	})
	require.Equal(t, "255", runFunc(t, ub, "ub", "-1"))
}

func TestE2EIf(t *testing.T) {
	// max(a, b) via a conditional branch to two return blocks.
	m := ir.NewModule()
	f := m.NewFunc("max", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	ra := f.NewBlock("ra")
	rb := f.NewBlock("rb")
	e.Jnz(e.Cmp(ir.CmpSgt, ir.ClsW, a, b), ra, rb)
	ra.Ret(a)
	rb.Ret(b)

	require.Equal(t, "7", runFunc(t, m, "max", "7", "3"))
	require.Equal(t, "9", runFunc(t, m, "max", "4", "9"))
}

func TestE2EDiamondMerge(t *testing.T) {
	// abs(x): if x<0 negate, else identity; both edges merge and return via phi.
	m := ir.NewModule()
	f := m.NewFunc("abs", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	neg := f.NewBlock("neg")
	pos := f.NewBlock("pos")
	end := f.NewBlock("end")
	e.Jnz(e.Cmp(ir.CmpSlt, ir.ClsW, x, f.Word(0)), neg, pos)
	n := neg.Neg(ir.ClsW, x)
	neg.Goto(end)
	pos.Goto(end)
	r := end.Phi(ir.ClsW, ir.PhiEdge{From: neg, Val: n}, ir.PhiEdge{From: pos, Val: x})
	end.Ret(r)

	require.Equal(t, "5", runFunc(t, m, "abs", "-5"))
	require.Equal(t, "8", runFunc(t, m, "abs", "8"))
}

func TestE2ELoop(t *testing.T) {
	// sumto(n) = 0+1+...+(n-1) using a loop header with two phis.
	m := ir.NewModule()
	f := m.NewFunc("sumto", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)
	s2 := body.Add(ir.ClsW, s, i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s)

	require.Equal(t, "45", runFunc(t, m, "sumto", "10"))
	require.Equal(t, "4950", runFunc(t, m, "sumto", "100"))
	require.Equal(t, "0", runFunc(t, m, "sumto", "0"))
}

func TestE2ENestedLoopWithBranch(t *testing.T) {
	// Count, over i in [0,n) and j in [0,n), the pairs with (i+j) even.
	// Exercises a loop nested in a loop plus an if inside the inner body.
	m := ir.NewModule()
	f := m.NewFunc("counteven", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	outer := f.NewBlock("outer")
	oBody := f.NewBlock("obody")
	inner := f.NewBlock("inner")
	iBody := f.NewBlock("ibody")
	bump := f.NewBlock("bump") // count++ then continue inner
	iCont := f.NewBlock("icont")
	oCont := f.NewBlock("ocont")
	exit := f.NewBlock("exit")

	start.Goto(outer)
	i := outer.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	cnt := outer.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	outer.Jnz(outer.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, oBody)

	oBody.Goto(inner)
	j := inner.Phi(ir.ClsW, ir.PhiEdge{From: oBody, Val: f.Word(0)})
	icnt := inner.Phi(ir.ClsW, ir.PhiEdge{From: oBody, Val: cnt})
	inner.Jnz(inner.Cmp(ir.CmpSge, ir.ClsW, j, n), oCont, iBody)

	// if (i+j) & 1 == 0 -> bump else icont
	sum := iBody.Add(ir.ClsW, i, j)
	par := iBody.And(ir.ClsW, sum, f.Word(1))
	iBody.Jnz(iBody.Cmp(ir.CmpEq, ir.ClsW, par, f.Word(0)), bump, iCont)
	c1 := bump.Add(ir.ClsW, icnt, f.Word(1))
	bump.Goto(iCont)
	icnt2 := iCont.Phi(ir.ClsW, ir.PhiEdge{From: bump, Val: c1}, ir.PhiEdge{From: iBody, Val: icnt})
	j2 := iCont.Add(ir.ClsW, j, f.Word(1))
	iCont.Goto(inner)
	inner.Phis[0].Add(iCont, j2)
	inner.Phis[1].Add(iCont, icnt2)

	// outer continue
	oCont.Goto(outer)
	i2 := oCont.Copy(ir.ClsW, i) // placeholder to have an instr; then i+1
	_ = i2
	outer.Phis[0].Add(oCont, oCont.Add(ir.ClsW, i, f.Word(1)))
	outer.Phis[1].Add(oCont, icnt)

	exit.Ret(cnt)

	// For n=4: pairs (i+j) even count = 8.
	require.Equal(t, "8", runFunc(t, m, "counteven", "4"))
	require.Equal(t, "13", runFunc(t, m, "counteven", "5"))
}

func TestE2EFloatCompareAll(t *testing.T) {
	cases := []struct {
		name string
		pred ir.Cmp
		a, b string
		want string
	}{
		{"fle", ir.CmpFle, "2.0", "2.0", "1"},
		{"fgt", ir.CmpFgt, "3.0", "2.0", "1"},
		{"fge", ir.CmpFge, "1.0", "2.0", "0"},
		{"fne", ir.CmpFne, "1.0", "2.0", "1"},
		{"feq", ir.CmpFeq, "2.0", "2.0", "1"},
		{"ford", ir.CmpFo, "1.0", "2.0", "1"},    // neither NaN -> ordered
		{"funord", ir.CmpFuo, "1.0", "2.0", "0"}, // neither NaN -> not unordered
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := ir.NewModule()
			f := m.NewFunc("fc", ir.ClsW).Export()
			a, b := f.Param("a", ir.ClsD), f.Param("b", ir.ClsD)
			e := f.Entry()
			e.Ret(e.Cmp(c.pred, ir.ClsW, a, b))
			require.Equal(t, c.want, runFunc(t, m, "fc", c.a, c.b))
		})
	}
}

func TestE2EMoreConversions(t *testing.T) {
	// single -> double -> single round trip (2.5 stays 2.5).
	pr := unary("pr", ir.ClsS, ir.ClsS, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Truncd(e.Exts(x))
	})
	require.Equal(t, "2.5", runFunc(t, pr, "pr", "2.5"))

	// reinterpret the bits of an f32 as i32 and back yields the original.
	cast := unary("cast", ir.ClsS, ir.ClsS, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		bits := e.Cast(ir.ClsW, x)
		return e.Cast(ir.ClsS, bits)
	})
	require.Equal(t, "1.5", runFunc(t, cast, "cast", "1.5"))

	// unsigned int -> float: a large word treated as unsigned.
	uf := unary("uf", ir.ClsW, ir.ClsD, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Ultof(ir.ClsD, x)
	})
	require.Equal(t, "4294967295", runFunc(t, uf, "uf", "-1"))

	// float -> unsigned int truncation.
	fu := unary("fu", ir.ClsD, ir.ClsL, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Stoui(ir.ClsL, x)
	})
	require.Equal(t, "100", runFunc(t, fu, "fu", "100.9"))

	// extsw: word -> long sign-extended.
	sw := unary("sw", ir.ClsW, ir.ClsL, func(f *ir.Func, e *ir.Block, x ir.Ref) ir.Ref {
		return e.Extsw(ir.ClsL, x)
	})
	require.Equal(t, "-5", runFunc(t, sw, "sw", "-5"))
}

func TestE2EDivRem(t *testing.T) {
	cases := []struct {
		name string
		op   func(f *ir.Func, e *ir.Block, a, b ir.Ref) ir.Ref
		a, b string
		want string
	}{
		{"div", func(f *ir.Func, e *ir.Block, a, b ir.Ref) ir.Ref { return e.Div(ir.ClsW, a, b) }, "-7", "2", "-3"},
		{"udiv", func(f *ir.Func, e *ir.Block, a, b ir.Ref) ir.Ref { return e.UDiv(ir.ClsW, a, b) }, "-2", "2", "2147483647"},
		{"rem", func(f *ir.Func, e *ir.Block, a, b ir.Ref) ir.Ref { return e.Rem(ir.ClsW, a, b) }, "-7", "3", "-1"},
		{"urem", func(f *ir.Func, e *ir.Block, a, b ir.Ref) ir.Ref { return e.URem(ir.ClsW, a, b) }, "7", "3", "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := ir.NewModule()
			f := m.NewFunc("dr", ir.ClsW).Export()
			a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
			e := f.Entry()
			e.Ret(c.op(f, e, a, b))
			require.Equal(t, c.want, runFunc(t, m, "dr", c.a, c.b))
		})
	}
}

func TestE2EPhiSwap(t *testing.T) {
	// Swap (a,b) exactly n times; the back edge assigns a<-b and b<-a in
	// parallel, exercising the parallel-copy phi semantics for a true swap.
	m := ir.NewModule()
	f := m.NewFunc("swap", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")
	start.Goto(loop)
	a := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(10)})
	b := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(20)})
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, b) // a <- b
	loop.Phis[1].Add(body, a) // b <- a  (parallel swap)
	loop.Phis[2].Add(body, i2)
	exit.Ret(a)

	require.Equal(t, "10", runFunc(t, m, "swap", "0"))
	require.Equal(t, "20", runFunc(t, m, "swap", "1"))
	require.Equal(t, "10", runFunc(t, m, "swap", "2"))
	require.Equal(t, "20", runFunc(t, m, "swap", "3"))
}

func TestE2EPointerIsWord(t *testing.T) {
	// A function over pointer-class values: ptradd(p, n) = p + n. On wasm the
	// pointer must be a 32-bit i32, so its param/result/arithmetic are all i32
	// and nothing wastes an i64 slot.
	m := ir.NewModule()
	f := m.NewFunc("ptradd", ir.ClsP).Export()
	p := f.Param("p", ir.ClsP)
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	off := e.Extuw(ir.ClsP, n) // widen the word offset into the pointer class
	e.Ret(e.Add(ir.ClsP, p, off))

	src, err := wasm.CompileModule(m)
	require.NoError(t, err)
	assert.Contains(t, src, "(param $t0 i32)") // pointer param is i32
	assert.Contains(t, src, "(result i32)")    // pointer result is i32
	assert.Contains(t, src, "i32.add")         // pointer arithmetic is i32
	assert.NotContains(t, src, "i64")          // no 64-bit anywhere

	require.Equal(t, "1024", runFunc(t, m, "ptradd", "1000", "24"))
}
