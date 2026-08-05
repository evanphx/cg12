package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// Execution tests for AMD64_PARITY_PLAN B1: Go ABIInternal functions and the
// calls that reach them, built and run natively.
//
// The shape every test here uses is the shape goc emits and the one the flip
// changed: a platform-ABI function calling a CallConvGoInternal one. Before B1
// both sides were emitted as System V and were self-consistent; now both sides
// are genuinely ABIInternal, and the register file underneath them is different
// (no callee-saved registers, scratch moved to R12/R13, arguments starting at
// RAX). goabi_lower_test.go names the registers; these tests are what prove the
// resulting machine code runs.
//
// runtest is the exported entry point the harness links and executes; a zero exit
// status means the computed value matched.

// goInternalFunc builds a CallConvGoInternal function of n long parameters
// returning their sum plus bias, with enough simultaneously live values that the
// allocator has to use several registers -- under ABIInternal it starts with RBX,
// which is precisely the register a System V caller believes is preserved.
func goInternalFunc(m *ir.Module, name string, n int, bias int64) *ir.Func {
	f := m.NewFunc(name, ir.ClsL)
	f.CallConv = ir.CallConvGoInternal
	var ps []ir.Ref
	for i := 0; i < n; i++ {
		ps = append(ps, f.Param("p", ir.ClsL))
	}
	e := f.Entry()
	acc := f.Long(bias)
	// Build every product first, so all of them are live at once and the body
	// genuinely occupies the ABIInternal allocation order rather than reusing one
	// register.
	var terms []ir.Ref
	for i, p := range ps {
		terms = append(terms, e.Mul(ir.ClsL, p, f.Long(int64(i+1))))
	}
	for _, term := range terms {
		acc = e.Add(ir.ClsL, acc, term)
	}
	e.Ret(acc)
	return f
}

// TestGoInternalCallFromPlatformFunctionRuns is the base case: a System V
// function calls an ABIInternal one and gets the right answer back. Arguments go
// out in RAX/RBX/RCX and the result comes back in RAX, none of which is where the
// same call went before the flip.
func TestGoInternalCallFromPlatformFunctionRuns(t *testing.T) {
	m := ir.NewModule()
	goInternalFunc(m, "closure", 3, 5) // 5 + 1*p0 + 2*p1 + 3*p2

	f, e := entry(m)
	r := e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, f.Sym("closure", 0),
		f.Long(10), f.Long(20), f.Long(30)) // 5 + 10 + 40 + 90 = 145
	e.Ret(e.Extsw(ir.ClsW, e.Sub(ir.ClsL, r, f.Long(145))))
	require.Equal(t, 0, runObj(t, m))
}

// TestGoInternalCallDoesNotDestroyTheCallersLiveValues is the mixed-frame hazard,
// and the one this whole change could plausibly have got wrong. An ABIInternal
// callee preserves nothing; a System V caller's allocator prefers callee-saved
// registers -- RBX first among them -- for values live across a call. If the
// lowered call does not declare that it destroys them, the caller reloads
// whatever the closure happened to leave behind.
//
// The five values are produced by calls so they are real register-resident
// values (a constant would be rematerialised at each use and never occupy a
// register at all), and every one of them is live across the closure call.
func TestGoInternalCallDoesNotDestroyTheCallersLiveValues(t *testing.T) {
	m := ir.NewModule()
	src := m.NewFunc("src", ir.ClsL)
	x := src.Param("x", ir.ClsL)
	se := src.Entry()
	se.Ret(se.Add(ir.ClsL, se.Mul(ir.ClsL, x, src.Long(2)), src.Long(1))) // 2x+1

	goInternalFunc(m, "closure", 1, 2) // 2 + 1*p

	f, e := entry(m)
	var live []ir.Ref
	want := int64(0)
	for i := 1; i <= 5; i++ {
		live = append(live, e.Call(ir.ClsL, f.Sym("src", 0), f.Long(int64(i))))
		want += 2*int64(i) + 1 // 3 + 5 + 7 + 9 + 11 = 35
	}
	r := e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, f.Sym("closure", 0), f.Long(6)) // 8
	want += 8

	acc := r
	for _, v := range live {
		acc = e.Add(ir.ClsL, acc, v)
	}
	e.Ret(e.Extsw(ir.ClsW, e.Sub(ir.ClsL, acc, f.Long(want))))
	require.Equal(t, 0, runObj(t, m))
}

// TestGoInternalCallDoesNotClobberItsFirstArgument is the RAX case. System V puts
// the number of vector registers used into AL before every call, for variadic
// callees; RAX is ABIInternal's argument register 0, so emitting that write
// before a closure call would destroy the first argument between placing it and
// making the call. The failure is silent -- the callee simply receives the float
// count instead of the value -- so it needs execution to catch.
func TestGoInternalCallDoesNotClobberItsFirstArgument(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("closure", ir.ClsL)
	f.CallConv = ir.CallConvGoInternal
	p := f.Param("p", ir.ClsL)
	fe := f.Entry()
	fe.Ret(p) // the identity, so a clobbered RAX is exactly what comes back

	r, e := entry(m)
	call := e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, r.Sym("closure", 0), r.Long(1234))
	e.Ret(e.Extsw(ir.ClsW, e.Sub(ir.ClsL, call, r.Long(1234))))
	require.Equal(t, 0, runObj(t, m))
}

// TestGoInternalClosureContextSurvivesDivision covers the stabilization. Go's
// ABIInternal delivers the closure environment in RDX, which on amd64 is also the
// high half of div/rem: the divide overwrites it. The backend copies the pointer
// into an allocatable register at entry, before anything else runs, so a body
// that divides and then reads its environment still sees the environment.
//
// Without that copy this returns the remainder-high garbage the divide left in
// RDX rather than 40, so the test fails by value rather than by crashing.
func TestGoInternalClosureContextSurvivesDivision(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("closure", ir.ClsL)
	f.CallConv = ir.CallConvGoInternal
	f.HasClosureContext = true
	p := f.Param("p", ir.ClsL)
	context := f.NewTemp("closure", ir.ClsL)
	ct := f.Temp(context)
	ct.Fixed, ct.Reg, ct.ClosureContext = true, 2, true // RDX, per Go's ABIInternal
	fe := f.Entry()
	// The divide clobbers RDX, and the environment is only read afterwards.
	quotient := fe.Div(ir.ClsL, p, f.Long(7))
	fe.Ret(fe.Add(ir.ClsL, quotient, context))

	r, e := entry(m)
	environment := e.Copy(ir.ClsL, r.Long(40))
	et := r.Temp(environment)
	et.Fixed, et.Reg = true, 2 // the caller places the environment in RDX
	value := e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, r.Sym("closure", 0), r.Long(70))
	call := &e.Instrs[len(e.Instrs)-1]
	call.ClosureCall, call.ClosureContext = true, environment
	e.Ret(e.Extsw(ir.ClsW, e.Sub(ir.ClsL, value, r.Long(50)))) // 70/7 + 40
	require.Equal(t, 0, runObj(t, m))
}

// TestGoInternalCallWithStackArguments exercises the half of the argument rule
// registers cannot reach: past the ninth integer argument ABIInternal packs the
// rest onto the stack, and the caller has to reserve an outgoing area the callee
// agrees with. The call is made from another ABIInternal function because from a
// System V one it would need argument registers 8 and 9 -- R10 and R11, that
// caller's emitter scratch pair -- which lowering refuses by name.
func TestGoInternalCallWithStackArguments(t *testing.T) {
	m := ir.NewModule()
	goInternalFunc(m, "wide", 11, 0) // sum of i*p(i-1), i = 1..11

	middle := m.NewFunc("middle", ir.ClsL)
	middle.CallConv = ir.CallConvGoInternal
	me := middle.Entry()
	args := make([]ir.Ref, 11)
	want := int64(0)
	for i := range args {
		args[i] = middle.Long(int64(i + 1))
		want += int64(i+1) * int64(i+1) // 1+4+9+...+121 = 506
	}
	me.Ret(me.Call(ir.ClsL, middle.Sym("wide", 0), args...))

	f, e := entry(m)
	r := e.Call(ir.ClsL, f.Sym("middle", 0))
	e.Ret(e.Extsw(ir.ClsW, e.Sub(ir.ClsL, r, f.Long(want))))
	require.Equal(t, 0, runObj(t, m))
}

// TestGoInternalAggregateArgumentAndResult covers the aggregate rule, where the
// two conventions genuinely differ rather than coincide: Go gives every flattened
// field its own register (two 32-bit fields take RAX and RBX), where System V
// packs both into one eightbyte and one register.
func TestGoInternalAggregateArgumentAndResult(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)

	// swap returns the pair with its halves exchanged and each scaled.
	swap := m.NewFuncVoid("swap")
	swap.CallConv = ir.CallConvGoInternal
	swap.HasRet = true
	swap.RetAgg = pair
	p := aggParam(swap, "p", pair)
	se := swap.Entry()
	lo := se.Load(ir.ClsW, p)
	hi := se.Load(ir.ClsW, se.Add(ir.ClsL, p, swap.Long(4)))
	buf := se.Alloc(8, 8)
	se.StoreSub(ir.SubW, se.Mul(ir.ClsW, hi, swap.Word(2)), buf)
	se.StoreSub(ir.SubW, lo, se.Add(ir.ClsL, buf, swap.Long(4)))
	se.Ret(buf)

	f, e := entry(m)
	in := e.Alloc(8, 8)
	e.StoreSub(ir.SubW, f.Word(7), in)
	e.StoreSub(ir.SubW, f.Word(16), e.Add(ir.ClsL, in, f.Long(4)))
	out := e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, f.Sym("swap", 0), in)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = []*ir.AggType{pair}
	call.RetAgg = pair
	a := e.Load(ir.ClsW, out)                            // 32
	b := e.Load(ir.ClsW, e.Add(ir.ClsL, out, f.Long(4))) // 7
	e.Ret(e.Sub(ir.ClsW, e.Add(ir.ClsW, a, b), f.Word(39)))
	require.Equal(t, 0, runObj(t, m))
}

// TestPlatformCallOutOfAGoInternalFunctionRuns is the other direction, and the
// one arm64 gets wrong latently: a closure making an ordinary unmarked call to an
// ordinary function. The call must be lowered System V even though the body
// around it is ABIInternal, and the two conventions share no argument register,
// so inheriting the enclosing function's convention would misplace every one.
func TestPlatformCallOutOfAGoInternalFunctionRuns(t *testing.T) {
	m := ir.NewModule()
	plain := m.NewFunc("plain", ir.ClsL)
	a := plain.Param("a", ir.ClsL)
	b := plain.Param("b", ir.ClsL)
	c := plain.Param("c", ir.ClsL)
	pe := plain.Entry()
	// Weighted, so a permuted argument order gives a different answer.
	pe.Ret(pe.Add(ir.ClsL, pe.Add(ir.ClsL, a, pe.Mul(ir.ClsL, b, plain.Long(10))),
		pe.Mul(ir.ClsL, c, plain.Long(100))))

	closure := m.NewFunc("closure", ir.ClsL)
	closure.CallConv = ir.CallConvGoInternal
	ce := closure.Entry()
	ce.Ret(ce.Call(ir.ClsL, closure.Sym("plain", 0), closure.Long(1), closure.Long(2), closure.Long(3)))

	f, e := entry(m)
	r := e.CallWithConvention(ir.ClsL, ir.CallConvGoInternal, f.Sym("closure", 0))
	e.Ret(e.Extsw(ir.ClsW, e.Sub(ir.ClsL, r, f.Long(321))))
	require.Equal(t, 0, runObj(t, m))
}
