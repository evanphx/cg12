package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// aggParam adds an aggregate-by-value parameter of type agg to f (a pointer temp,
// as the parser would for ":type %p") and returns its ref.
func aggParam(f *ir.Func, name string, agg *ir.AggType) ir.Ref {
	r := f.NewTemp(name, ir.ClsL)
	t := f.Temp(r)
	t.Agg = agg
	f.Params = append(f.Params, t)
	return r
}

// {int,int} == one INTEGER eightbyte, passed in a single GP register.
func TestObjAggIntPairArg(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	sum2 := m.NewFunc("sum2", ir.ClsW)
	p := aggParam(sum2, "p", pair)
	se := sum2.Entry()
	lo := se.Load(ir.ClsW, p)
	hi := se.Load(ir.ClsW, se.Add(ir.ClsL, p, sum2.Long(4)))
	se.Ret(se.Add(ir.ClsW, lo, hi))

	f, e := entry(m)
	ptr := e.Alloc(8, 8)
	e.StoreSub(ir.SubW, f.Word(7), ptr)
	e.StoreSub(ir.SubW, f.Word(35), e.Add(ir.ClsL, ptr, f.Long(4)))
	r := e.Call(ir.ClsW, f.Sym("sum2", 0), ptr)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = []*ir.AggType{pair}
	e.Ret(e.Sub(ir.ClsW, r, f.Word(42)))
	require.Equal(t, 0, runObj(t, m))
}

// {int × 4} == two INTEGER eightbytes, passed in two GP registers.
func TestObjAggFourIntArg(t *testing.T) {
	m := ir.NewModule()
	quad := &ir.AggType{Name: "quad", Fields: []ir.Field{{Sub: ir.SubW, Count: 4}}}
	m.AddType(quad)
	sum4 := m.NewFunc("sum4", ir.ClsW)
	p := aggParam(sum4, "p", quad)
	se := sum4.Entry()
	acc := se.Load(ir.ClsW, p)
	for i := 1; i < 4; i++ {
		acc = se.Add(ir.ClsW, acc, se.Load(ir.ClsW, se.Add(ir.ClsL, p, sum4.Long(int64(4*i)))))
	}
	se.Ret(acc)

	f, e := entry(m)
	ptr := e.Alloc(16, 16)
	for i := 0; i < 4; i++ {
		e.StoreSub(ir.SubW, f.Word(int64(i+1)), e.Add(ir.ClsL, ptr, f.Long(int64(4*i))))
	}
	r := e.Call(ir.ClsW, f.Sym("sum4", 0), ptr)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = []*ir.AggType{quad}
	e.Ret(e.Sub(ir.ClsW, r, f.Word(10))) // 1+2+3+4
	require.Equal(t, 0, runObj(t, m))
}

// {double,double} == two SSE eightbytes, passed in xmm0/xmm1.
func TestObjAggDoublePairArg(t *testing.T) {
	m := ir.NewModule()
	vec := &ir.AggType{Name: "vec2d", Fields: []ir.Field{{Sub: ir.SubD}, {Sub: ir.SubD}}}
	m.AddType(vec)
	sumd := m.NewFunc("sumd", ir.ClsW)
	p := aggParam(sumd, "p", vec)
	se := sumd.Entry()
	x := se.Load(ir.ClsD, p)
	y := se.Load(ir.ClsD, se.Add(ir.ClsL, p, sumd.Long(8)))
	se.Ret(se.Stosi(ir.ClsW, se.Add(ir.ClsD, x, y)))

	f, e := entry(m)
	ptr := e.Alloc(16, 16)
	e.Store(f.Double(1.5), ptr)
	e.Store(f.Double(2.5), e.Add(ir.ClsL, ptr, f.Long(8)))
	r := e.Call(ir.ClsW, f.Sym("sumd", 0), ptr)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = []*ir.AggType{vec}
	e.Ret(e.Sub(ir.ClsW, r, f.Word(4))) // 1.5+2.5 == 4.0
	require.Equal(t, 0, runObj(t, m))
}

// {int,double} == INTEGER eightbyte (rdi) + SSE eightbyte (xmm0): a mixed class.
func TestObjAggMixedArg(t *testing.T) {
	m := ir.NewModule()
	mixed := &ir.AggType{Name: "mixed", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubD}}}
	m.AddType(mixed)
	fn := m.NewFunc("mix", ir.ClsW)
	p := aggParam(fn, "p", mixed)
	fe := fn.Entry()
	i := fe.Load(ir.ClsW, p)
	d := fe.Load(ir.ClsD, fe.Add(ir.ClsL, p, fn.Long(8)))
	fe.Ret(fe.Add(ir.ClsW, i, fe.Stosi(ir.ClsW, d)))

	f, e := entry(m)
	ptr := e.Alloc(16, 16)
	e.StoreSub(ir.SubW, f.Word(10), ptr)
	e.Store(f.Double(32.0), e.Add(ir.ClsL, ptr, f.Long(8)))
	r := e.Call(ir.ClsW, f.Sym("mix", 0), ptr)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = []*ir.AggType{mixed}
	e.Ret(e.Sub(ir.ClsW, r, f.Word(42))) // 10 + 32
	require.Equal(t, 0, runObj(t, m))
}

// A register-class aggregate returned by value (eightbytes come back in rax/rdx).
func TestObjAggReturn(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	mk := m.NewFuncVoid("mk")
	mk.HasRet = true
	mk.RetAgg = pair
	a := mk.Param("a", ir.ClsW)
	b := mk.Param("b", ir.ClsW)
	me := mk.Entry()
	buf := me.Alloc(8, 8)
	me.StoreSub(ir.SubW, a, buf)
	me.StoreSub(ir.SubW, b, me.Add(ir.ClsL, buf, mk.Long(4)))
	me.Ret(buf)

	f, e := entry(m)
	r := e.Call(ir.ClsL, f.Sym("mk", 0), f.Word(7), f.Word(35))
	call := &e.Instrs[len(e.Instrs)-1]
	call.RetAgg = pair
	lo := e.Load(ir.ClsW, r)
	hi := e.Load(ir.ClsW, e.Add(ir.ClsL, r, f.Long(4)))
	e.Ret(e.Sub(ir.ClsW, e.Add(ir.ClsW, lo, hi), f.Word(42)))
	require.Equal(t, 0, runObj(t, m))
}

// A > 16-byte aggregate is MEMORY class: passed by value on the stack, returned
// via a hidden sret buffer pointer.
func TestObjAggMemoryArgReturn(t *testing.T) {
	m := ir.NewModule()
	big := &ir.AggType{Name: "big", Fields: []ir.Field{{Sub: ir.SubW, Count: 6}}} // 24 bytes

	// sumbig(b) sums the six ints of a MEMORY-class argument.
	sumbig := m.NewFunc("sumbig", ir.ClsW)
	p := aggParam(sumbig, "p", big)
	se := sumbig.Entry()
	acc := se.Load(ir.ClsW, p)
	for i := 1; i < 6; i++ {
		acc = se.Add(ir.ClsW, acc, se.Load(ir.ClsW, se.Add(ir.ClsL, p, sumbig.Long(int64(4*i)))))
	}
	se.Ret(acc)

	// mkbig(n) returns a big struct {n, n+1, ..., n+5} via sret.
	mkbig := m.NewFuncVoid("mkbig")
	mkbig.HasRet = true
	mkbig.RetAgg = big
	n := mkbig.Param("n", ir.ClsW)
	me := mkbig.Entry()
	buf := me.Alloc(16, 24)
	for i := 0; i < 6; i++ {
		me.StoreSub(ir.SubW, me.Add(ir.ClsW, n, mkbig.Word(int64(i))), me.Add(ir.ClsL, buf, mkbig.Long(int64(4*i))))
	}
	me.Ret(buf)

	f, e := entry(m)
	// s = mkbig(10) -> {10,11,12,13,14,15}; then sumbig(s) == 75.
	s := e.Call(ir.ClsL, f.Sym("mkbig", 0), f.Word(10))
	mkcall := &e.Instrs[len(e.Instrs)-1]
	mkcall.RetAgg = big
	r := e.Call(ir.ClsW, f.Sym("sumbig", 0), s)
	sumcall := &e.Instrs[len(e.Instrs)-1]
	sumcall.AggArgs = []*ir.AggType{big}
	e.Ret(e.Sub(ir.ClsW, r, f.Word(75)))
	require.Equal(t, 0, runObj(t, m))
}
