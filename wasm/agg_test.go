package wasm_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/wasm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lastInstr returns the most recently appended instruction of a block, so a test
// can annotate a call with its aggregate argument/return types (which the
// builder does not set).
func lastInstr(b *ir.Block) *ir.Instr { return &b.Instrs[len(b.Instrs)-1] }

func TestE2EAggregateReturnAndArg(t *testing.T) {
	// pair { w, w }. mkpair returns one by value (sret); sumpair takes one by
	// value (a pointer) and adds the fields; driver composes them.
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m := ir.NewModule()

	mk := m.NewFunc("mkpair", ir.ClsW)
	mk.HasRet, mk.RetAgg = true, pair
	a, b := mk.Param("a", ir.ClsW), mk.Param("b", ir.ClsW)
	e := mk.Entry()
	buf := e.Alloc(8, 8)
	e.Store(a, buf)
	e.Store(b, e.Add(ir.ClsP, buf, mk.ConstInt(ir.ClsP, 4)))
	e.Ret(buf)

	sp := m.NewFunc("sumpair", ir.ClsW)
	p := sp.Param("p", ir.ClsP)
	sp.Temp(p).Agg = pair
	se := sp.Entry()
	x := se.Load(ir.ClsW, p)
	y := se.Load(ir.ClsW, se.Add(ir.ClsP, p, sp.ConstInt(ir.ClsP, 4)))
	se.Ret(se.Add(ir.ClsW, x, y))

	dr := m.NewFunc("driver", ir.ClsW).Export()
	da, db := dr.Param("a", ir.ClsW), dr.Param("b", ir.ClsW)
	de := dr.Entry()
	res := de.Call(ir.ClsP, dr.Sym("mkpair", 0), da, db)
	lastInstr(de).RetAgg = pair
	out := de.Call(ir.ClsW, dr.Sym("sumpair", 0), res)
	lastInstr(de).SetAggArgs([]*ir.AggType{pair})
	de.Ret(out)

	require.Equal(t, "42", runFunc(t, m, "driver", "20", "22"))
	require.Equal(t, "0", runFunc(t, m, "driver", "-5", "5"))
}

func TestIndirectAggReturnUnsupported(t *testing.T) {
	// Returning an aggregate through an indirect (computed) callee is rejected.
	agg := &ir.AggType{Name: "box", Fields: []ir.Field{{Sub: ir.SubL}, {Sub: ir.SubL}}}
	m := ir.NewModule()
	f := m.NewFunc("g", ir.ClsW).Export()
	fp := f.Param("fp", ir.ClsP)
	e := f.Entry()
	e.Call(ir.ClsP, fp) // indirect callee
	lastInstr(e).RetAgg = agg
	e.Ret(f.Word(0))

	_, err := wasm.CompileModule(m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "indirect aggregate-returning")
}

func TestAggregateSignatureShape(t *testing.T) {
	// An aggregate-returning function takes a hidden sret pointer and yields
	// nothing; an aggregate parameter is a plain i32 pointer.
	agg := &ir.AggType{Name: "box", Fields: []ir.Field{{Sub: ir.SubL}, {Sub: ir.SubL}}}
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsW)
	f.HasRet, f.RetAgg = true, agg
	p := f.Param("p", ir.ClsP)
	f.Temp(p).Agg = agg
	f.Entry().Ret(p)

	s, err := wasm.CompileModule(m)
	require.NoError(t, err)
	assert.Contains(t, s, "(param $__ret i32)")
	assert.Contains(t, s, "(param $t0 i32)") // aggregate param is a pointer
	assert.NotContains(t, s, "(result")      // sret: no result
	assert.Contains(t, s, "memory.copy")
}

func TestE2EVariadicInt(t *testing.T) {
	m := ir.NewModule()
	vs := m.NewFunc("vsum", ir.ClsW)
	vs.Variadic = true
	n := vs.Param("n", ir.ClsW)
	start := vs.Entry()
	ap := start.Alloc(8, 8)
	start.VaStart(ap)
	loop, body, exit := vs.NewBlock("loop"), vs.NewBlock("body"), vs.NewBlock("exit")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: vs.Word(0)})
	acc := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: vs.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)
	v := body.VaArg(ir.ClsW, ap)
	body.Goto(loop)
	loop.Phis[0].Add(body, body.Add(ir.ClsW, i, vs.Word(1)))
	loop.Phis[1].Add(body, body.Add(ir.ClsW, acc, v))
	exit.Ret(acc)

	dr := m.NewFunc("driver", ir.ClsW).Export()
	de := dr.Entry()
	de.Ret(de.Call(ir.ClsW, dr.Sym("vsum", 0), dr.Word(3), dr.Word(10), dr.Word(20), dr.Word(30)))

	require.Equal(t, "60", runFunc(t, m, "driver"))
}

func TestE2EVariadicDouble(t *testing.T) {
	// Double varargs exercise 8-byte alignment in the buffer and f64.load.
	m := ir.NewModule()
	vs := m.NewFunc("dsum", ir.ClsD)
	vs.Variadic = true
	n := vs.Param("n", ir.ClsW)
	start := vs.Entry()
	ap := start.Alloc(8, 8)
	start.VaStart(ap)
	loop, body, exit := vs.NewBlock("loop"), vs.NewBlock("body"), vs.NewBlock("exit")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: vs.Word(0)})
	acc := loop.Phi(ir.ClsD, ir.PhiEdge{From: start, Val: vs.Double(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)
	v := body.VaArg(ir.ClsD, ap)
	body.Goto(loop)
	loop.Phis[0].Add(body, body.Add(ir.ClsW, i, vs.Word(1)))
	loop.Phis[1].Add(body, body.Add(ir.ClsD, acc, v))
	exit.Ret(acc)

	dr := m.NewFunc("driver", ir.ClsD).Export()
	de := dr.Entry()
	de.Ret(de.Call(ir.ClsD, dr.Sym("dsum", 0), dr.Word(3), dr.Double(1.5), dr.Double(2.25), dr.Double(0.25)))

	require.Equal(t, "4", runFunc(t, m, "driver"))
}
