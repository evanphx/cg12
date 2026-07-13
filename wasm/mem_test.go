package wasm_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

func TestE2EAllocRoundTrip(t *testing.T) {
	// Allocate a buffer, store the argument at an offset, read it back.
	m := ir.NewModule()
	f := m.NewFunc("rt", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	buf := e.Alloc(8, 16)
	p := e.Add(ir.ClsP, buf, f.ConstInt(ir.ClsP, 8))
	e.Store(x, p)
	e.Ret(e.Load(ir.ClsW, p))

	require.Equal(t, "1234", runFunc(t, m, "rt", "1234"))
}

func TestE2ESubwordLoadStore(t *testing.T) {
	// Store a word, read back its low byte sign-extended and zero-extended.
	cases := []struct {
		name string
		load func(f *ir.Func, e *ir.Block, addr ir.Ref) ir.Ref
		in   string
		want string
	}{
		{"sb", func(f *ir.Func, e *ir.Block, addr ir.Ref) ir.Ref { return e.LoadSub(ir.ClsW, ir.SubB, addr) }, "255", "-1"},
		{"ub", func(f *ir.Func, e *ir.Block, addr ir.Ref) ir.Ref { return e.LoadSub(ir.ClsW, ir.SubUB, addr) }, "255", "255"},
		{"sh", func(f *ir.Func, e *ir.Block, addr ir.Ref) ir.Ref { return e.LoadSub(ir.ClsW, ir.SubH, addr) }, "65535", "-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := ir.NewModule()
			f := m.NewFunc("sw", ir.ClsW).Export()
			x := f.Param("x", ir.ClsW)
			e := f.Entry()
			buf := e.Alloc(8, 16)
			e.Store(x, buf)
			e.Ret(c.load(f, e, buf))
			require.Equal(t, c.want, runFunc(t, m, "sw", c.in))
		})
	}
}

func TestE2EArrayLoop(t *testing.T) {
	// Fill buf[i] = i*i for i in [0,n), then sum the buffer back.
	m := ir.NewModule()
	f := m.NewFunc("sqsum", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	buf := start.Alloc(16, 256)

	fill := f.NewBlock("fill")
	fbody := f.NewBlock("fbody")
	sum := f.NewBlock("sum")
	sbody := f.NewBlock("sbody")
	exit := f.NewBlock("exit")

	start.Goto(fill)
	i := fill.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	fill.Jnz(fill.Cmp(ir.CmpSge, ir.ClsW, i, n), sum, fbody)
	// buf[i] = i*i
	off := fbody.Mul(ir.ClsP, fbody.Extsw(ir.ClsP, i), f.ConstInt(ir.ClsP, 4))
	addr := fbody.Add(ir.ClsP, buf, off)
	fbody.Store(fbody.Mul(ir.ClsW, i, i), addr)
	i2 := fbody.Add(ir.ClsW, i, f.Word(1))
	fbody.Goto(fill)
	fill.Phis[0].Add(fbody, i2)

	// sum buf[0..n)
	j := sum.Phi(ir.ClsW, ir.PhiEdge{From: fill, Val: f.Word(0)})
	acc := sum.Phi(ir.ClsW, ir.PhiEdge{From: fill, Val: f.Word(0)})
	sum.Jnz(sum.Cmp(ir.CmpSge, ir.ClsW, j, n), exit, sbody)
	soff := sbody.Mul(ir.ClsP, sbody.Extsw(ir.ClsP, j), f.ConstInt(ir.ClsP, 4))
	v := sbody.Load(ir.ClsW, sbody.Add(ir.ClsP, buf, soff))
	acc2 := sbody.Add(ir.ClsW, acc, v)
	j2 := sbody.Add(ir.ClsW, j, f.Word(1))
	sbody.Goto(sum)
	sum.Phis[0].Add(sbody, j2)
	sum.Phis[1].Add(sbody, acc2)
	exit.Ret(acc)

	// sum of i*i for i in [0,5) = 0+1+4+9+16 = 30.
	require.Equal(t, "30", runFunc(t, m, "sqsum", "5"))
}

func TestE2ERecursion(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("fact", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	rec := f.NewBlock("rec")
	base := f.NewBlock("base")
	e.Jnz(e.Cmp(ir.CmpSle, ir.ClsW, n, f.Word(1)), base, rec)
	base.Ret(f.Word(1))
	sub := rec.Call(ir.ClsW, f.Sym("fact", 0), rec.Sub(ir.ClsW, n, f.Word(1)))
	rec.Ret(rec.Mul(ir.ClsW, n, sub))

	require.Equal(t, "120", runFunc(t, m, "fact", "5"))
	require.Equal(t, "1", runFunc(t, m, "fact", "0"))
}

func TestE2EGlobalData(t *testing.T) {
	// A global word array, indexed by the argument.
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name:  "arr",
		Align: 4,
		Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{10, 20, 30, 40, 50}}},
	})
	f := m.NewFunc("get", ir.ClsW).Export()
	i := f.Param("i", ir.ClsW)
	e := f.Entry()
	off := e.Mul(ir.ClsP, e.Extsw(ir.ClsP, i), f.ConstInt(ir.ClsP, 4))
	e.Ret(e.Load(ir.ClsW, e.Add(ir.ClsP, f.Sym("arr", 0), off)))

	require.Equal(t, "30", runFunc(t, m, "get", "2"))
	require.Equal(t, "50", runFunc(t, m, "get", "4"))
}

func TestE2EDataSymbolReference(t *testing.T) {
	// A global pointer that holds the address of another global; dereference it
	// twice. Exercises pointer-sized (4-byte) symbol references in data.
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "val", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{777}}}},
		&ir.Data{Name: "ptr", Align: 4, Items: []ir.DataItem{{Sym: "val"}}},
	)
	f := m.NewFunc("deref", ir.ClsW).Export()
	e := f.Entry()
	p := e.Load(ir.ClsP, f.Sym("ptr", 0)) // load the pointer
	e.Ret(e.Load(ir.ClsW, p))             // load through it
	require.Equal(t, "777", runFunc(t, m, "deref"))
}

func TestE2EIndirectCall(t *testing.T) {
	// apply(sel, x, y) picks add or sub by sel and calls it through the table.
	m := ir.NewModule()
	add := m.NewFunc("add", ir.ClsW).Export()
	{
		a, b := add.Param("a", ir.ClsW), add.Param("b", ir.ClsW)
		e := add.Entry()
		e.Ret(e.Add(ir.ClsW, a, b))
	}
	sub := m.NewFunc("sub", ir.ClsW).Export()
	{
		a, b := sub.Param("a", ir.ClsW), sub.Param("b", ir.ClsW)
		e := sub.Entry()
		e.Ret(e.Sub(ir.ClsW, a, b))
	}
	ap := m.NewFunc("apply", ir.ClsW).Export()
	sel, x, y := ap.Param("sel", ir.ClsW), ap.Param("x", ir.ClsW), ap.Param("y", ir.ClsW)
	e := ap.Entry()
	ra, rb, call := ap.NewBlock("ra"), ap.NewBlock("rb"), ap.NewBlock("call")
	e.Jnz(e.Cmp(ir.CmpEq, ir.ClsW, sel, ap.Word(0)), ra, rb)
	ra.Goto(call)
	rb.Goto(call)
	fp := call.Phi(ir.ClsP,
		ir.PhiEdge{From: ra, Val: ap.Sym("add", 0)},
		ir.PhiEdge{From: rb, Val: ap.Sym("sub", 0)})
	call.Ret(call.Call(ir.ClsW, fp, x, y))

	require.Equal(t, "10", runFunc(t, m, "apply", "0", "7", "3"))
	require.Equal(t, "4", runFunc(t, m, "apply", "1", "7", "3"))
}
