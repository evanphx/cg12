package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// A self-tail-recursive countdown runs in constant stack: without real tail-call
// elimination, a million deep frames would overflow the stack under qemu.
func TestObjTailCallDeep(t *testing.T) {
	m := ir.NewModule()
	count := m.NewFunc("count", ir.ClsW)
	n := count.Param("n", ir.ClsW)
	ce := count.Entry()
	base := count.NewBlock("base")
	rec := count.NewBlock("rec")
	ce.Jnz(ce.Cmp(ir.CmpEq, ir.ClsW, n, count.Word(0)), base, rec)
	base.Ret(count.Word(0))
	rec.TailCall(ir.ClsW, count.Sym("count", 0), rec.Sub(ir.ClsW, n, count.Word(1)))

	rt := m.NewFunc("runtest", ir.ClsW).Export()
	e := rt.Entry()
	e.Ret(e.Call(ir.ClsW, rt.Sym("count", 0), rt.Word(1000000)))
	require.Equal(t, 0, runObj(t, m))
}

// Mutual tail recursion (even/odd) also runs in constant stack and returns the
// right parity.
func TestObjTailCallMutual(t *testing.T) {
	m := ir.NewModule()
	build := func(name, other string, base int64) {
		f := m.NewFunc(name, ir.ClsW)
		n := f.Param("n", ir.ClsW)
		e := f.Entry()
		bb := f.NewBlock("base")
		rec := f.NewBlock("rec")
		e.Jnz(e.Cmp(ir.CmpEq, ir.ClsW, n, f.Word(0)), bb, rec)
		bb.Ret(f.Word(base))
		rec.TailCall(ir.ClsW, f.Sym(other, 0), rec.Sub(ir.ClsW, n, f.Word(1)))
	}
	build("iseven", "isodd", 1)
	build("isodd", "iseven", 0)

	rt := m.NewFunc("runtest", ir.ClsW).Export()
	e := rt.Entry()
	// iseven(500001) == 0; return it (0 on success).
	e.Ret(e.Call(ir.ClsW, rt.Sym("iseven", 0), rt.Word(500001)))
	require.Equal(t, 0, runObj(t, m))
}

// An indirect tail call (callee in a register) also reuses the frame.
func TestObjTailCallIndirect(t *testing.T) {
	m := ir.NewModule()
	target := m.NewFunc("tgt", ir.ClsW)
	x := target.Param("x", ir.ClsW)
	te := target.Entry()
	te.Ret(te.Add(ir.ClsW, x, target.Word(1)))

	m.Data = append(m.Data, &ir.Data{Name: "tptr", Align: 8, Items: []ir.DataItem{{Sym: "tgt"}}})

	f := m.NewFunc("via", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	fe := f.Entry()
	fp := fe.Load(ir.ClsL, f.Sym("tptr", 0))
	fe.TailCall(ir.ClsW, fp, fe.Mul(ir.ClsW, a, f.Word(10)))

	rt := m.NewFunc("runtest", ir.ClsW).Export()
	e := rt.Entry()
	e.Ret(e.Sub(ir.ClsW, e.Call(ir.ClsW, rt.Sym("via", 0), rt.Word(4)), rt.Word(41))) // 4*10+1 == 41
	require.Equal(t, 0, runObj(t, m))
}

// A tail call needing stack arguments is rejected (v1 limitation), rather than
// silently miscompiled.
func TestTailCallStackArgsError(t *testing.T) {
	m := ir.NewModule()
	sink := m.NewFunc("sink", ir.ClsW)
	for i := 0; i < 8; i++ {
		sink.Param(string(rune('a'+i)), ir.ClsW)
	}
	sink.Entry().Ret(sink.Word(0))

	f := m.NewFunc("caller", ir.ClsW).Export()
	fe := f.Entry()
	var args []ir.Ref
	for i := 0; i < 8; i++ {
		args = append(args, f.Word(int64(i)))
	}
	fe.TailCall(ir.ClsW, f.Sym("sink", 0), args...)

	_, err := amd64.CompileObject(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stack arguments")
}
