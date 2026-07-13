package wasm

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWasmType(t *testing.T) {
	assert.Equal(t, "i32", wasmType(ir.ClsW))
	assert.Equal(t, "i64", wasmType(ir.ClsL))
	assert.Equal(t, "f32", wasmType(ir.ClsS))
	assert.Equal(t, "f64", wasmType(ir.ClsD))
	assert.Equal(t, "i32", wasmType(ir.Cls(99))) // default
}

func TestBinOp(t *testing.T) {
	cases := map[ir.Op]struct{ w, d string }{
		ir.OAdd:  {"i32.add", "f64.add"},
		ir.OSub:  {"i32.sub", "f64.sub"},
		ir.OMul:  {"i32.mul", "f64.mul"},
		ir.ODiv:  {"i32.div_s", "f64.div"},
		ir.OUDiv: {"i32.div_u", ""},
		ir.ORem:  {"i32.rem_s", ""},
		ir.OURem: {"i32.rem_u", ""},
		ir.OAnd:  {"i32.and", ""},
		ir.OOr:   {"i32.or", ""},
		ir.OXor:  {"i32.xor", ""},
		ir.OShl:  {"i32.shl", ""},
		ir.OShr:  {"i32.shr_u", ""},
		ir.OSar:  {"i32.shr_s", ""},
	}
	for op, want := range cases {
		assert.Equal(t, want.w, binOp(op, ir.ClsW), "%s word", op)
		if want.d != "" {
			assert.Equal(t, want.d, binOp(op, ir.ClsD), "%s double", op)
		}
	}
	assert.Equal(t, "unreachable", binOp(ir.ONop, ir.ClsW))
}

func TestCmpOp(t *testing.T) {
	cases := map[ir.Cmp]string{
		ir.CmpEq:  "i32.eq",
		ir.CmpNe:  "i32.ne",
		ir.CmpSlt: "i32.lt_s",
		ir.CmpSle: "i32.le_s",
		ir.CmpSgt: "i32.gt_s",
		ir.CmpSge: "i32.ge_s",
		ir.CmpUlt: "i32.lt_u",
		ir.CmpUle: "i32.le_u",
		ir.CmpUgt: "i32.gt_u",
		ir.CmpUge: "i32.ge_u",
	}
	for p, want := range cases {
		assert.Equal(t, want, cmpOp(p, "i32"), p.String())
	}
	// Float predicates use the unsuffixed forms.
	assert.Equal(t, "f64.eq", cmpOp(ir.CmpFeq, "f64"))
	assert.Equal(t, "f64.ne", cmpOp(ir.CmpFne, "f64"))
	assert.Equal(t, "f64.lt", cmpOp(ir.CmpFlt, "f64"))
	assert.Equal(t, "f64.le", cmpOp(ir.CmpFle, "f64"))
	assert.Equal(t, "f64.gt", cmpOp(ir.CmpFgt, "f64"))
	assert.Equal(t, "f64.ge", cmpOp(ir.CmpFge, "f64"))
	assert.Equal(t, "unreachable", cmpOp(ir.CmpFo, "f64")) // handled separately by compare
}

func TestConvOps(t *testing.T) {
	cases := []struct {
		op       ir.Op
		src, dst ir.Cls
		want     []string
	}{
		{ir.OExtsb, ir.ClsW, ir.ClsW, []string{"i32.extend8_s"}},
		{ir.OExtsb, ir.ClsW, ir.ClsL, []string{"i32.extend8_s", "i64.extend_i32_s"}},
		{ir.OExtsh, ir.ClsW, ir.ClsW, []string{"i32.extend16_s"}},
		{ir.OExtsh, ir.ClsW, ir.ClsL, []string{"i32.extend16_s", "i64.extend_i32_s"}},
		{ir.OExtub, ir.ClsW, ir.ClsW, []string{"i32.const 255", "i32.and"}},
		{ir.OExtub, ir.ClsW, ir.ClsL, []string{"i32.const 255", "i32.and", "i64.extend_i32_u"}},
		{ir.OExtuh, ir.ClsW, ir.ClsW, []string{"i32.const 65535", "i32.and"}},
		{ir.OExtuh, ir.ClsW, ir.ClsL, []string{"i32.const 65535", "i32.and", "i64.extend_i32_u"}},
		{ir.OExtsw, ir.ClsW, ir.ClsL, []string{"i64.extend_i32_s"}},
		{ir.OExtsw, ir.ClsW, ir.ClsW, nil},
		{ir.OExtuw, ir.ClsW, ir.ClsL, []string{"i64.extend_i32_u"}},
		{ir.OExtuw, ir.ClsW, ir.ClsW, nil},
		{ir.OExts, ir.ClsS, ir.ClsD, []string{"f64.promote_f32"}},
		{ir.OTruncd, ir.ClsD, ir.ClsS, []string{"f32.demote_f64"}},
		{ir.OStosi, ir.ClsD, ir.ClsW, []string{"i32.trunc_f64_s"}},
		{ir.OStoui, ir.ClsS, ir.ClsL, []string{"i64.trunc_f32_u"}},
		{ir.OSltof, ir.ClsW, ir.ClsD, []string{"f64.convert_i32_s"}},
		{ir.OUltof, ir.ClsL, ir.ClsS, []string{"f32.convert_i64_u"}},
		{ir.OCast, ir.ClsS, ir.ClsW, []string{"i32.reinterpret_f32"}},
		{ir.OCast, ir.ClsL, ir.ClsD, []string{"f64.reinterpret_i64"}},
		{ir.ONop, ir.ClsW, ir.ClsW, nil}, // not a conversion
	}
	for _, c := range cases {
		assert.Equal(t, c.want, convOps(c.op, c.src, c.dst), "%s %v->%v", c.op, c.src, c.dst)
	}
}

func TestFormatFloat(t *testing.T) {
	assert.Equal(t, "0.5", formatFloat(ir.Const{Kind: ir.ConstFloat, Cls: ir.ClsD, Flt: 0.5}))
	assert.Equal(t, "0.25", formatFloat(ir.Const{Kind: ir.ConstFloat, Cls: ir.ClsS, Flt: 0.25}))
}

func TestSanitize(t *testing.T) {
	assert.Equal(t, "foo.bar_baz", sanitize("foo.bar_baz"))
	assert.Equal(t, "a_b_c", sanitize("a-b c"))
	assert.Equal(t, "x$1", sanitize("x$1"))
}

// compileErr compiles a single-function module and returns any error.
func compileErr(t *testing.T, build func(f *ir.Func)) error {
	t.Helper()
	m := ir.NewModule()
	f := m.NewFunc("g", ir.ClsW).Export()
	build(f)
	_, err := CompileModule(m)
	return err
}

func TestUnsupportedOpErrors(t *testing.T) {
	// A memory-to-memory blit is not yet supported by this backend.
	err := compileErr(t, func(f *ir.Func) {
		e := f.Entry()
		p := e.Alloc(8, 16)
		q := e.Alloc(8, 16)
		e.Instrs = append(e.Instrs, ir.Instr{Op: ir.OBlit, Args: []ir.Ref{p, q}, Aux: 16})
		e.Ret(f.Word(0))
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet supported")
}

func TestUnknownSymbolErrors(t *testing.T) {
	err := compileErr(t, func(f *ir.Func) {
		e := f.Entry()
		e.Ret(f.Sym("global", 0)) // no such data symbol
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown symbol")
}

func TestIrreducibleErrors(t *testing.T) {
	// entry branches into a two-headed loop (A<->B), which is irreducible.
	m := ir.NewModule()
	f := m.NewFunc("irr", ir.ClsW).Export()
	p := f.Param("p", ir.ClsW)
	entry := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	xa := f.NewBlock("xa")
	xb := f.NewBlock("xb")
	entry.Jnz(p, a, b)
	a.Jnz(p, b, xa)
	b.Jnz(p, a, xb)
	xa.Ret(f.Word(1))
	xb.Ret(f.Word(2))

	_, err := CompileModule(m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "irreducible")
}

func TestVoidAndHltEmit(t *testing.T) {
	// A void function whose body traps compiles to `unreachable`.
	m := ir.NewModule()
	f := m.NewFuncVoid("boom")
	f.Linkage.Export = true
	f.Entry().Hlt()
	s, err := CompileModule(m)
	require.NoError(t, err)
	assert.Contains(t, s, "unreachable")
	assert.NotContains(t, s, "(result") // void: no result type
}

func TestLoadOps(t *testing.T) {
	cases := []struct {
		op  ir.Op
		res ir.Cls
		out string
	}{
		{ir.OLoadsb, ir.ClsW, "i32.load8_s"},
		{ir.OLoadub, ir.ClsW, "i32.load8_u"},
		{ir.OLoadsb, ir.ClsL, "i64.load8_s"},
		{ir.OLoadsh, ir.ClsW, "i32.load16_s"},
		{ir.OLoaduh, ir.ClsL, "i64.load16_u"},
		{ir.OLoadsw, ir.ClsL, "i64.load32_s"},
		{ir.OLoadsw, ir.ClsW, "i32.load"},
		{ir.OLoaduw, ir.ClsL, "i64.load32_u"},
		{ir.OLoaduw, ir.ClsW, "i32.load"},
		{ir.OLoadl, ir.ClsL, "i64.load"},
		{ir.OLoads, ir.ClsS, "f32.load"},
		{ir.OLoadd, ir.ClsD, "f64.load"},
	}
	for _, c := range cases {
		assert.Equal(t, c.out, loadOp(c.op, c.res), "%s -> %v", c.op, c.res)
	}
	assert.Equal(t, "unreachable", loadOp(ir.ONop, ir.ClsW))
}

func TestStoreOps(t *testing.T) {
	cases := []struct {
		op  ir.Op
		val ir.Cls
		out string
	}{
		{ir.OStoreb, ir.ClsW, "i32.store8"},
		{ir.OStoreb, ir.ClsL, "i64.store8"},
		{ir.OStoreh, ir.ClsW, "i32.store16"},
		{ir.OStorew, ir.ClsW, "i32.store"},
		{ir.OStorew, ir.ClsL, "i64.store32"},
		{ir.OStorel, ir.ClsL, "i64.store"},
		{ir.OStores, ir.ClsS, "f32.store"},
		{ir.OStored, ir.ClsD, "f64.store"},
	}
	for _, c := range cases {
		assert.Equal(t, c.out, storeOp(c.op, c.val), "%s <- %v", c.op, c.val)
	}
	assert.Equal(t, "unreachable", storeOp(ir.ONop, ir.ClsW))
}

func TestDataEncoding(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "a", Align: 4, Items: []ir.DataItem{
			{Sub: ir.SubW, Ints: []int64{1}},          // 4 bytes
			{Sub: ir.SubB, Ints: []int64{0xff, 0x00}}, // 2 bytes
			{Zero: 2},   // 2 bytes
			{Str: "hi"}, // 2 bytes
		}},
		&ir.Data{Name: "b", Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubD, Flts: []float64{1.0}}, // 8 bytes
			{Sym: "a"},                           // 4 bytes (pointer-width on wasm)
		}},
	)
	ctx := layoutModule(m)

	// a at the null guard (16), b aligned to 8 after a's 10 bytes -> 16+10=26 -> 32.
	assert.Equal(t, int64(16), ctx.syms["a"])
	assert.Equal(t, int64(32), ctx.syms["b"])

	assert.Equal(t, int64(10), dataSize(m.Data[0]))
	assert.Equal(t, int64(12), dataSize(m.Data[1]))

	ab, err := encodeData(m.Data[0], ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 0, 0, 0, 0xff, 0x00, 0, 0, 'h', 'i'}, ab)

	bb, err := encodeData(m.Data[1], ctx)
	require.NoError(t, err)
	// 1.0 as little-endian f64, then the 4-byte address of "a" (16).
	assert.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0xf0, 0x3f, 16, 0, 0, 0}, bb)
}

func TestEncodeDataUnknownSymbol(t *testing.T) {
	d := &ir.Data{Name: "d", Items: []ir.DataItem{{Sym: "missing"}}}
	_, err := encodeData(d, &modCtx{syms: map[string]int64{}})
	require.Error(t, err)
}

func TestCompileStandalone(t *testing.T) {
	// Compile of a single memory-free function works without a module wrapper.
	m := ir.NewModule()
	f := m.NewFunc("id", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	f.Entry().Ret(x)
	s, err := Compile(f)
	require.NoError(t, err)
	assert.Contains(t, s, "(func $id")
	assert.Contains(t, s, "return")
}
