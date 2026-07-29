package arm64

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegNames(t *testing.T) {
	assert.Equal(t, "x0", X0.xName())
	assert.Equal(t, "w0", X0.wName())
	assert.Equal(t, "x28", X28.xName())
	assert.Equal(t, "sp", SP.xName())
	assert.Equal(t, "wsp", SP.wName())
	assert.Equal(t, "xzr", ZR.xName())
	assert.Equal(t, "wzr", ZR.wName())
	assert.Equal(t, "d0", V0.xName())
	assert.Equal(t, "s0", V0.wName())

	assert.Equal(t, "x5", X5.Name(8))
	assert.Equal(t, "w5", X5.Name(4))

	assert.True(t, V0.IsFloat())
	assert.False(t, X0.IsFloat())

	assert.Equal(t, "<badreg>", Reg(999).xName())
	assert.Equal(t, "<badreg>", Reg(999).wName())
}

func TestRegisterPools(t *testing.T) {
	scratch := map[Reg]bool{scratch0: true, scratch1: true, scratch2: true}
	reserved := map[Reg]bool{X18: true, X29: true, X30: true, SP: true, ZR: true}
	seen := map[Reg]bool{}
	for _, r := range intAllocOrder {
		assert.Falsef(t, scratch[r], "scratch %s must not be allocatable", r.xName())
		assert.Falsef(t, reserved[r], "reserved %s must not be allocatable", r.xName())
		assert.Falsef(t, seen[r], "duplicate %s in alloc order", r.xName())
		seen[r] = true
	}
	// A platform-C-ABI function may also use X26 and X28 (which the Go runtime
	// reserves for the closure context and the g register); the base order, used by
	// Go-convention functions, must not.
	assert.NotContains(t, intAllocOrder, X26, "base order reserves X26 for Go")
	assert.NotContains(t, intAllocOrder, X28, "base order reserves X28 (g) for Go")
	assert.Contains(t, intAllocOrderPlatform, X26, "platform ABI reclaims X26")
	assert.Contains(t, intAllocOrderPlatform, X28, "platform ABI reclaims X28")
	for _, r := range intAllocOrderPlatform {
		assert.False(t, scratch[r] || reserved[r], "platform order excludes scratch/reserved %s", r.xName())
	}

	assert.True(t, isCallerSaved(X0))
	assert.True(t, isCallerSaved(X9))
	assert.False(t, isCallerSaved(X19))
	assert.True(t, calleeSaved[X19])
	assert.False(t, calleeSaved[X9])
}

// The signed and unsigned orderings are different conditions on AArch64 -- lt/le
// against lo/ls -- and picking from the wrong column compares the wrong way
// round on exactly the inputs that straddle the sign bit.
func TestCondCode(t *testing.T) {
	want := map[ir.Cmp]a64.Cond{
		ir.CmpEq: a64.EQ, ir.CmpNe: a64.NE,
		ir.CmpSlt: a64.LT, ir.CmpSle: a64.LE, ir.CmpSgt: a64.GT, ir.CmpSge: a64.GE,
		ir.CmpUlt: a64.CC, ir.CmpUle: a64.LS, ir.CmpUgt: a64.HI, ir.CmpUge: a64.CS,
	}
	for pred, cond := range want {
		got, ok := intCondOf(pred)
		assert.Truef(t, ok, "%v", pred)
		assert.Equalf(t, cond, got, "%v", pred)
	}
	_, ok := intCondOf(ir.CmpFeq) // float predicates are a separate table
	assert.False(t, ok)
}

// The access width is not just the class size: a sub-word load zero-extends into
// a w register whatever it was asked for, while a sign-extending one widens to
// its result class.
func TestLoadStoreSize(t *testing.T) {
	loads := []struct {
		op   ir.Op
		cls  ir.Cls
		size int
	}{
		{ir.OLoadub, ir.ClsW, 4},
		{ir.OLoaduh, ir.ClsW, 4},
		{ir.OLoaduw, ir.ClsL, 4}, // zero-extends: still a w destination
		{ir.OLoadsb, ir.ClsL, 8}, // sign-extends into an x
		{ir.OLoadsh, ir.ClsW, 4},
		{ir.OLoadsw, ir.ClsL, 8},
		{ir.OLoadl, ir.ClsL, 8},
		{ir.OLoadq, ir.ClsD, 16},
	}
	for _, c := range loads {
		assert.Equal(t, c.size, loadSize(c.op, c.cls), c.op.String())
	}
	stores := []struct {
		op   ir.Op
		size int
	}{
		{ir.OStoreb, 4}, {ir.OStoreh, 4}, {ir.OStorew, 4},
		{ir.OStorel, 8}, {ir.OStores, 4}, {ir.OStored, 8}, {ir.OStoreq, 16},
	}
	for _, c := range stores {
		assert.Equal(t, c.size, storeSize(c.op), c.op.String())
	}
}

func TestRoundUpAndSanitize(t *testing.T) {
	assert.Equal(t, 8, roundUp(5, 4))
	assert.Equal(t, 8, roundUp(8, 8))
	assert.Equal(t, 0, roundUp(0, 16))
	assert.Equal(t, 3, roundUp(3, 0)) // non-positive alignment is a no-op

	assert.Equal(t, "a_b", sanitize("a.b"))
	assert.Equal(t, "foo_1", sanitize("foo_1"))
	assert.Equal(t, "anon", sanitize(""))
	assert.Equal(t, "x__y", sanitize("x$.y"))
}

func TestLocHelpers(t *testing.T) {
	r0 := loc{reg: X0, size: 8}
	r1 := loc{reg: X1, size: 8}
	m3 := loc{mem: true, slot: 3, size: 8}
	m4 := loc{mem: true, slot: 4, size: 8}
	im := loc{imm: true, val: 5, size: 8}

	assert.True(t, sameLoc(r0, loc{reg: X0, size: 8}))
	assert.False(t, sameLoc(r0, r1))
	assert.True(t, sameLoc(m3, loc{mem: true, slot: 3}))
	assert.False(t, sameLoc(m3, m4))
	assert.False(t, sameLoc(im, im))

	assert.True(t, srcReadsDst(r0, r0))
	assert.False(t, srcReadsDst(r0, r1))
	assert.True(t, srcReadsDst(m3, m3))
	assert.False(t, srcReadsDst(m3, m4))
	assert.False(t, srcReadsDst(im, r0))
	assert.False(t, srcReadsDst(r0, m3))
}

func TestAllocShape(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("a")
	// No constant size argument: falls back to the alignment.
	al, sz := allocShape(f, &ir.Instr{Op: ir.OAlloc16})
	assert.Equal(t, 16, al)
	assert.Equal(t, 16, sz)

	e := f.Entry()
	e.Alloc(8, 20) // size 20 rounds up to 24 at align 8
	al, sz = allocShape(f, &e.Instrs[len(e.Instrs)-1])
	assert.Equal(t, 8, al)
	assert.Equal(t, 24, sz)
}

func TestClassifyAgg(t *testing.T) {
	agg := func(fs ...ir.Field) *ir.AggType { return &ir.AggType{Fields: fs} }
	fw := ir.Field{Sub: ir.SubW}
	fl := ir.Field{Sub: ir.SubL}
	fb := ir.Field{Sub: ir.SubB}
	fs := ir.Field{Sub: ir.SubS}
	fd := ir.Field{Sub: ir.SubD}

	c := classifyAgg(agg(fw, fw)) // 8 bytes -> one x register
	assert.Equal(t, aggGP, c.kind)
	assert.Equal(t, 1, c.nregs)

	c = classifyAgg(agg(fl, fb, fw)) // 16 bytes -> two x registers
	assert.Equal(t, aggGP, c.kind)
	assert.Equal(t, 2, c.nregs)

	c = classifyAgg(agg(fs, fb, fs)) // 12 bytes, has a byte -> not HFA, GP
	assert.Equal(t, aggGP, c.kind)
	assert.Equal(t, 2, c.nregs)

	c = classifyAgg(agg(fs, fs, fs)) // HFA of 3 singles
	assert.Equal(t, aggHFA, c.kind)
	assert.Equal(t, 3, c.nregs)
	assert.Equal(t, ir.ClsS, c.elem)

	c = classifyAgg(agg(fd, fd)) // HFA of 2 doubles
	assert.Equal(t, aggHFA, c.kind)
	assert.Equal(t, ir.ClsD, c.elem)

	c = classifyAgg(&ir.AggType{Fields: []ir.Field{{Sub: ir.SubB, Count: 17}}}) // 17 bytes
	assert.Equal(t, aggMemory, c.kind)

	c = classifyAgg(agg(fs, fs, fs, fs, fs)) // 5 floats: too many for an HFA, 20 bytes
	assert.Equal(t, aggMemory, c.kind)
}

func TestLowerAcceptsFloat(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsD)
	x := f.Param("x", ir.ClsD)
	f.Entry().Ret(x)
	require.NoError(t, lower(f, moduleConventions(f), TLSLocalExec))
}

func TestLowerManyParamsUseStack(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	for i := 0; i < 10; i++ {
		f.Param("p", ir.ClsW)
	}
	f.Entry().RetVoid()
	require.NoError(t, lower(f, moduleConventions(f), TLSLocalExec))
	stacked := 0
	for _, in := range f.Start.Instrs {
		if in.Op == ir.OPar && len(in.Args) == 0 {
			stacked++
		}
	}
	assert.Equal(t, 2, stacked, "the 9th and 10th integer params are stacked")
}

func TestLowerManyFloatParamsUseStack(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	for i := 0; i < 10; i++ {
		f.Param("d", ir.ClsD)
	}
	f.Entry().RetVoid()
	require.NoError(t, lower(f, moduleConventions(f), TLSLocalExec))
	stacked := 0
	for _, in := range f.Start.Instrs {
		if in.Op == ir.OPar && len(in.Args) == 0 {
			stacked++
		}
	}
	assert.Equal(t, 2, stacked)
}

func TestLowerFloatParamsUseSeparateBank(t *testing.T) {
	// Nine mixed params are fine as long as neither bank exceeds 8.
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	for i := 0; i < 6; i++ {
		f.Param("i", ir.ClsW)
	}
	for i := 0; i < 6; i++ {
		f.Param("d", ir.ClsD)
	}
	f.Entry().RetVoid()
	require.NoError(t, lower(f, moduleConventions(f), TLSLocalExec), "6 int + 6 float args fit in x0..x5 and v0..v5")
}

func TestLowerManyCallArgsUseStack(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	e := f.Entry()
	args := make([]ir.Ref, 10)
	for i := range args {
		args[i] = f.Word(int64(i))
	}
	e.CallVoid(f.Sym("g", 0), args...)
	e.RetVoid()
	require.NoError(t, lower(f, moduleConventions(f), TLSLocalExec))
	// The call records a 16-aligned outgoing stack area for the two extra args.
	var call *ir.Instr
	for k := range f.Start.Instrs {
		if f.Start.Instrs[k].Op == ir.OCall {
			call = &f.Start.Instrs[k]
		}
	}
	require.NotNil(t, call)
	assert.Equal(t, int64(16), call.Aux, "two stacked args round up to 16 bytes")
}

func TestCompileFloatFunction(t *testing.T) {
	// A float add now compiles; the parameter comes in s0 and the result too.
	m := ir.NewModule()
	f := m.NewFunc("fadd", ir.ClsS).Export()
	a := f.Param("a", ir.ClsS)
	b := f.Param("b", ir.ClsS)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsS, a, b))

	assert.Contains(t, disasmModule(t, m), "fadd s")
}

func TestLifetimeMarkersAreNoOps(t *testing.T) {
	// A function with a stack slot used by a store and a load, compiled with and
	// without lifetime markers bracketing the slot, must produce byte-identical code:
	// the markers emit nothing and (via analysis/live.go skipping them) do not perturb
	// register allocation.
	build := func(withMarkers bool) string {
		m := ir.NewModule()
		f := m.NewFuncVoid("life").Export()
		p := f.Param("p", ir.ClsL)
		e := f.Entry()
		a := e.Alloc(8, 8)
		if withMarkers {
			e.LifeStart(a)
		}
		e.Store(p, a)           // *a = p
		v := e.Load(ir.ClsL, a) // v = *a
		e.Store(v, p)           // *p = v
		if withMarkers {
			e.LifeEnd(a)
		}
		e.RetVoid()
		return disasmModule(t, m)
	}
	assert.Equal(t, build(false), build(true),
		"lifetime markers must emit no machine code and not change allocation")
}

func TestSelectMulAddFuses(t *testing.T) {
	// A single-use integer multiply feeding an add fuses to one madd; feeding a
	// subtract's subtrahend fuses to msub (c - a*b). The separate mul disappears.
	build := func(name string, body func(e *ir.Block, a, b, c ir.Ref) ir.Ref) string {
		m := ir.NewModule()
		f := m.NewFunc(name, ir.ClsL).Export()
		a, b, c := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL), f.Param("c", ir.ClsL)
		e := f.Entry()
		e.Ret(body(e, a, b, c))
		return disasmModule(t, m)
	}
	add := build("f", func(e *ir.Block, a, b, c ir.Ref) ir.Ref {
		return e.Add(ir.ClsL, e.Mul(ir.ClsL, a, b), c)
	})
	assert.Contains(t, add, "madd x")
	assert.NotContains(t, add, "mul x")

	sub := build("g", func(e *ir.Block, a, b, c ir.Ref) ir.Ref {
		return e.Sub(ir.ClsL, c, e.Mul(ir.ClsL, a, b))
	})
	assert.Contains(t, sub, "msub x")
	assert.NotContains(t, sub, "mul x")
}

func TestSingleBitTestBranchFuses(t *testing.T) {
	// `if (x & (1<<k))` driving a branch becomes a single tbnz on the register,
	// with no tst and no materialized boolean.
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	e := f.Entry()
	hi, lo := f.NewBlock("hi"), f.NewBlock("lo")
	e.Jnz(e.And(ir.ClsL, x, f.Long(1<<12)), hi, lo)
	hi.Ret(f.Long(1))
	lo.Ret(f.Long(0))

	asm := disasmModule(t, m)
	assert.Regexp(t, `tbnz\s+\w+, #12,`, asm, "single-bit test becomes tbnz on bit 12")
	assert.NotContains(t, asm, "tst", "no flags-setting tst is needed")
}

func TestTbzRangeGating(t *testing.T) {
	// tbzInRange fuses a single-bit test only when the pass-1 distance to its target
	// is within tbz's reach. In pass 1 (no offsets recorded) it never fuses, so the
	// measurement pass emits no tbz.
	f := ir.NewModule().NewFunc("f", ir.ClsL)
	b, target := f.Entry(), f.NewBlock("t")
	em := newMC(f)

	assert.False(t, em.tbzInRange(b, target), "pass 1 (no offsets) never fuses a tbz")

	em.refTermPC = map[*ir.Block]int{b: 40000}
	em.refBlockPC = map[*ir.Block]int{target: 20000}
	assert.True(t, em.tbzInRange(b, target), "20000-byte distance is within reach")

	em.refBlockPC[target] = 5000 // 35000 bytes away, past tbzRangeLimit
	assert.False(t, em.tbzInRange(b, target), "35000-byte distance is out of reach")
}

func TestStoreZeroUsesZeroRegister(t *testing.T) {
	// Storing the constant 0 uses the zero register (str xzr) instead of first
	// materializing 0 into a scratch with a mov.
	m := ir.NewModule()
	f := m.NewFunc("clr", ir.ClsL)
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	e.Store(f.Long(0), p) // *p = 0
	e.Ret(f.Long(0))

	asm := disasmModule(t, m)
	assert.Regexp(t, `str\s+xzr, \[`, asm, "store of 0 uses xzr")
	assert.NotContains(t, asm, "mov x9, #0", "no zero is materialized for the store")
}

func TestSharedOffsetFoldsIntoEachAccess(t *testing.T) {
	// A constant field offset shared by a read AND a write of the same address (a
	// read-modify-write: `s->b += 8`) folds into both the load and the store, so the
	// shared address `add base, #8` is dropped and each access carries [base, #8].
	m := ir.NewModule()
	f := m.NewFunc("rmw", ir.ClsL)
	base := f.Param("s", ir.ClsL)
	e := f.Entry()
	addr := e.Add(ir.ClsL, base, f.Long(8)) // a = s + 8, feeding both the load and store
	v := e.Load(ir.ClsL, addr)
	e.Store(e.Add(ir.ClsL, v, f.Long(8)), addr) // *(a) = *(a) + 8
	e.Ret(v)

	foldSharedOffset(f)

	var loads, stores, sharedAddGone int
	for i := range e.Instrs {
		in := &e.Instrs[i]
		switch in.Op {
		case ir.OLoadl:
			assert.Equal(t, int64(8), in.Aux, "load carries the folded offset")
			assert.Equal(t, base, in.Args[0], "load reads base directly")
			loads++
		case ir.OStorel:
			assert.Equal(t, int64(8), in.Aux, "store carries the folded offset")
			assert.Equal(t, base, in.Args[1], "store writes through base directly")
			stores++
		case ir.ONop:
			sharedAddGone++ // the shared address add was removed
		}
	}
	assert.Equal(t, 1, loads)
	assert.Equal(t, 1, stores)
	assert.Equal(t, 1, sharedAddGone, "exactly the shared address add is dropped (the v+8 add stays)")
}

func TestBitfieldExtractFuses(t *testing.T) {
	// (x >> lsb) & (2^width - 1) folds into a single ubfx, dropping the shift-and-mask.
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	e := f.Entry()
	e.Ret(e.And(ir.ClsL, e.Shr(ir.ClsL, x, f.Long(12)), f.Long(0xf)))
	asm := disasmModule(t, m)
	assert.Contains(t, asm, "ubfx x0, x0, #12, #4", "shift-and-mask becomes a bitfield extract")
	assert.NotContains(t, asm, "lsr", "the standalone shift is gone")
	assert.NotContains(t, asm, "and x", "the standalone mask is gone")
}

func TestCompareConstantFolds(t *testing.T) {
	// A comparison with the constant first (CONST == x) folds into a compare-immediate
	// by swapping the operands, rather than materializing the constant into a register.
	// The ordered predicate flips when swapped; equality is unchanged.
	eq := func() string {
		m := ir.NewModule()
		f := m.NewFunc("f", ir.ClsW).Export()
		x := f.Param("x", ir.ClsL)
		e := f.Entry()
		e.Ret(e.Cmp(ir.CmpEq, ir.ClsL, f.Long(28), x)) // 28 == x
		return disasmModule(t, m)
	}()
	assert.Contains(t, eq, "cmp x", "compares the register directly")
	assert.Contains(t, eq, "#28", "against the immediate 28")
	assert.NotRegexp(t, `mov\s+\w+, #28[\s\S]*cmp`, eq, "no materialize-then-compare")

	lt := func() string {
		m := ir.NewModule()
		f := m.NewFunc("g", ir.ClsW).Export()
		x := f.Param("x", ir.ClsL)
		e := f.Entry()
		// 5 < x  becomes  x > 5, so the result is set on gt.
		e.Ret(e.Cmp(ir.CmpSlt, ir.ClsL, f.Long(5), x))
		return disasmModule(t, m)
	}()
	assert.Contains(t, lt, "#5", "compares against 5 directly")
	assert.Contains(t, lt, "cset w0, gt", "5 < x becomes x > 5")
}

func TestCopyConstMaterializesDirect(t *testing.T) {
	// A copy of a constant (as phi destruction produces on an edge) materializes the
	// constant straight into the destination register, not into a scratch and then a
	// move. The anti-pattern is `mov x16, #imm` immediately followed by `mov dst, x16`.
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	e := f.Entry()
	then, done := f.NewBlock("then"), f.NewBlock("done")
	e.Jnz(x, then, done)
	then.Goto(done)
	r := done.Phi(ir.ClsL, ir.PhiEdge{From: e, Val: f.Long(0)}, ir.PhiEdge{From: then, Val: f.Long(42)})
	done.Ret(r)

	asm := disasmModule(t, m)
	assert.Contains(t, asm, "#0x2a", "the constant 42 is materialized")
	assert.NotRegexp(t, `mov\s+x1[67], #[^\n]*\n\s*mov\s+\w+, x1[67]\b`, asm,
		"no materialize-into-scratch-then-move")
}

func TestSelectConstOffsetAddressing(t *testing.T) {
	// A load or store at a constant offset uses the [base, #imm] addressing mode
	// rather than materializing the offset into a register and indexing.
	load := func() string {
		m := ir.NewModule()
		f := m.NewFunc("f", ir.ClsL).Export()
		p := f.Param("p", ir.ClsL)
		e := f.Entry()
		e.Ret(e.Load(ir.ClsL, e.Add(ir.ClsL, p, f.Long(40))))
		return disasmModule(t, m)
	}()
	assert.Contains(t, load, "#40]", "immediate offset, not a register index")

	store := func() string {
		m := ir.NewModule()
		f := m.NewFuncVoid("g").Export()
		p := f.Param("p", ir.ClsL)
		v := f.Param("v", ir.ClsL)
		e := f.Entry()
		e.Store(v, e.Add(ir.ClsL, p, f.Long(24)))
		e.RetVoid()
		return disasmModule(t, m)
	}()
	assert.Contains(t, store, "#24]", "immediate offset, not a register index")
}

func TestGoABIZerosPointerLocalsBeforeCalls(t *testing.T) {
	module := ir.NewModule()
	f := module.NewFunc("pointer_local", ir.ClsW)
	f.CallConv = ir.CallConvGoInternal
	f.ManagedFrame = true
	entry := f.Entry()
	value := entry.Alloc(8, 8)
	pointerSlot := entry.Alloc(8, 8)
	entry.Store(value, pointerSlot)
	entry.Call(ir.ClsL, f.Sym("observe", 0), value)
	entry.Ret(f.Word(0))

	assert.Contains(t, disasmModule(t, module), "str xzr, [x29")
}

func TestManagedFrameKeepsAAPCS64ParameterAssignment(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("managed_aapcs", ir.ClsL)
	function.ManagedFrame = true
	var ninth ir.Ref
	for index := 0; index < 9; index++ {
		parameter := function.Param(fmt.Sprintf("p%d", index), ir.ClsL)
		if index == 8 {
			ninth = parameter
		}
	}
	function.Entry().Ret(ninth)

	prepareGoABI(function)
	ir.LowerPointers(function, ptrCls)
	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))

	stackParameterFound := false
	for index := range function.Start.Instrs {
		instruction := &function.Start.Instrs[index]
		if instruction.Op == ir.OPar && instruction.To == ninth && len(instruction.Args) == 0 {
			stackParameterFound = true
			assert.Equal(t, int64(0), instruction.Aux)
		}
	}
	assert.True(t, stackParameterFound, "the ninth AAPCS64 parameter should arrive at stack offset zero")
}

func TestAAPCS64FunctionCanCallGoInternalContract(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("managed_caller")
	function.ManagedFrame = true
	arguments := make([]ir.Ref, 9)
	for index := range arguments {
		arguments[index] = function.Long(int64(index + 1))
	}
	entry := function.Entry()
	entry.CallVoidWithConvention(ir.CallConvGoInternal, function.Sym("runtime_contract", 0), arguments...)
	entry.RetVoid()

	prepareGoABI(function)
	ir.LowerPointers(function, ptrCls)
	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))

	var loweredCall *ir.Instr
	goInternalNinthRegister := false
	for index := range function.Start.Instrs {
		instruction := &function.Start.Instrs[index]
		if instruction.Op == ir.OArg && instruction.To.Kind == ir.RefTemp {
			temporary := function.Temp(instruction.To)
			if temporary.Fixed && temporary.Reg == int(X8) {
				goInternalNinthRegister = true
			}
		}
		if instruction.Op == ir.OCall {
			loweredCall = instruction
		}
	}
	require.NotNil(t, loweredCall)
	assert.True(t, loweredCall.CallConvSet)
	assert.Equal(t, ir.CallConvGoInternal, loweredCall.CallConv)
	assert.True(t, goInternalNinthRegister, "GoInternal should pass the ninth integer in x8")
}

func TestAAPCS64GroupedSliceValuesUseIndirectAggregateResult(t *testing.T) {
	module := ir.NewModule()
	slice := &ir.AggType{Name: "slice", Fields: []ir.Field{
		{Sub: ir.SubL, Pointer: true},
		{Sub: ir.SubL},
		{Sub: ir.SubL},
	}}

	callee := module.NewFunc("slice_identity", ir.ClsP)
	callee.RetAgg = slice
	callee.RetValues = true
	parts := callee.ParamGroup("value", slice, ir.ClsP, ir.ClsL, ir.ClsL)
	callee.Entry().RetAggregate(parts...)

	caller := module.NewFunc("slice_length", ir.ClsL)
	arguments := []ir.Ref{caller.Sym("buffer", 0), caller.Long(7), caller.Long(9)}
	results := caller.Entry().CallAggregate(slice, []ir.Cls{ir.ClsP, ir.ClsL, ir.ClsL}, caller.Sym("slice_identity", 0), arguments...)
	call := &caller.Entry().Instrs[0]
	call.ArgGroups = []ir.ValueGroup{{Index: 0, Count: 3, Type: slice}}
	caller.Entry().Ret(results[1])
	assert.Equal(t, 48, aapcsCallStackBytes(caller, call), "outgoing area must include the x8 morestack spill home")

	assembly := disasmModule(t, module)
	assert.Contains(t, assembly, "x8", "a 24-byte AAPCS64 result must use the indirect-result register")
}

func TestAAPCS64IndirectResultBufferIsStackCopyRoot(t *testing.T) {
	resultType := &ir.AggType{Name: "large_result", Fields: []ir.Field{
		{Sub: ir.SubL, Pointer: true},
		{Sub: ir.SubL},
		{Sub: ir.SubL},
	}}
	function := ir.NewModule().NewFunc("return_large_result", ir.ClsP)
	function.ManagedFrame = true
	function.RetAgg = resultType
	function.RetValues = true
	function.Entry().RetAggregate(
		function.Long(0),
		function.Long(1),
		function.Long(1),
	)

	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))

	var resultBuffer *ir.Temp
	for _, temporary := range function.Temps {
		if temporary.Name == "retbuf" {
			resultBuffer = temporary
			break
		}
	}
	require.NotNil(t, resultBuffer)
	assert.True(t, resultBuffer.GCRef)
}

func TestAAPCS64AggregateParameterPointersAreStackCopyRoots(t *testing.T) {
	interfaceType := &ir.AggType{Name: "interface", Fields: []ir.Field{
		{Sub: ir.SubL, Pointer: true},
		{Sub: ir.SubL, Pointer: true},
	}}
	function := ir.NewModule().NewFuncVoid("consume_interface")
	function.ManagedFrame = true
	parameter := function.Param("value", ir.ClsL)
	function.Temp(parameter).Agg = interfaceType
	function.Entry().CallVoid(function.Sym("observe", 0))
	function.Entry().RetVoid()

	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))
	assert.True(t, function.Temp(parameter).GCRef)

	pointerRegisters := make(map[int]bool)
	for _, temporary := range function.Temps {
		if temporary.Fixed && temporary.GCRef {
			pointerRegisters[temporary.Reg] = true
		}
	}
	assert.True(t, pointerRegisters[int(X0)])
	assert.True(t, pointerRegisters[int(X1)])

	foundInterfaceHome := false
	for allocationID, pointerWords := range function.StackPointerWords {
		if pointerWords[0] && pointerWords[8] {
			foundInterfaceHome = true
			assert.True(t, function.Temps[allocationID].GCRef)
			break
		}
	}
	assert.True(t, foundInterfaceHome)
}

func TestManagedAAPCS64HomesGroupedSliceRegistersForStackGrowth(t *testing.T) {
	slice := &ir.AggType{Name: "slice", Fields: []ir.Field{
		{Sub: ir.SubL, Pointer: true},
		{Sub: ir.SubL},
		{Sub: ir.SubL},
	}}
	function := ir.NewModule().NewFuncVoid("consume_slice")
	function.ManagedFrame = true
	function.ParamGroup("value", slice, ir.ClsP, ir.ClsL, ir.ClsL)

	frame := goArgumentFrameFor(function)
	require.Equal(t, 24, frame.size)
	require.Equal(t, []int{0}, frame.pointerWords)
	require.Len(t, frame.spills, 3)
	require.Equal(t, X0, frame.spills[0].reg)
	require.Equal(t, 8, frame.spills[0].offset)
	require.True(t, frame.spills[0].pointer)
	require.Equal(t, X1, frame.spills[1].reg)
	require.Equal(t, X2, frame.spills[2].reg)
}

func TestManagedAAPCS64DoesNotSplitGroupedInterfaceAcrossRegisterBoundary(t *testing.T) {
	interfaceType := &ir.AggType{Name: "interface", Fields: []ir.Field{
		{Sub: ir.SubL, Pointer: true},
		{Sub: ir.SubL, Pointer: true},
	}}
	function := ir.NewModule().NewFuncVoid("consume_interface_after_seven_words")
	function.ManagedFrame = true
	for index := 0; index < 7; index++ {
		function.Param(fmt.Sprintf("word%d", index), ir.ClsL)
	}
	function.ParamGroup("value", interfaceType, ir.ClsP, ir.ClsP)
	function.Entry().RetVoid()

	frame := goArgumentFrameFor(function)
	require.Equal(t, []int{0, 1}, frame.pointerWords)
	for _, spill := range frame.spills {
		require.NotEqual(t, X7, spill.reg, "the complete interface must move to the stack")
	}

	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))
	interfaceParameters := function.Params[7:]
	require.Len(t, interfaceParameters, 2)
	for index, parameter := range interfaceParameters {
		found := false
		for _, instruction := range function.Start.Instrs {
			if instruction.Op == ir.OPar && instruction.To == parameter.Ref() {
				require.Equal(t, int64(index*8), instruction.Aux)
				found = true
				break
			}
		}
		require.True(t, found, "missing stack parameter %d", index)
	}
}

// The argument frame carries two pointer maps because its words become valid at
// different times. A stack-passed pointer argument is written by the caller
// before the call, so it is a root for the whole call; a register argument's home
// slot is written only by the stack-growth prologue on the path that calls
// morestack, so outside that window it holds whatever the caller's stack held.
// Only the entry map may name it.
func TestManagedAAPCS64SeparatesIncomingArgumentsFromRegisterHomes(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("consume_nine_pointers")
	function.ManagedFrame = true
	function.ParamRef("registerPointer")
	for index := 0; index < 7; index++ {
		function.Param(fmt.Sprintf("word%d", index), ir.ClsL)
	}
	function.ParamRef("stackPointer")
	function.Entry().RetVoid()

	frame := goArgumentFrameFor(function)
	assert.Equal(t, []int{0}, frame.incomingPointerWords, "only the stack-passed argument is caller-written")
	assert.Equal(t, []int{0, 1}, frame.pointerWords, "the entry map adds the first register's home slot")
	require.NotEmpty(t, frame.spills)
	assert.Equal(t, X0, frame.spills[0].reg)
	assert.True(t, frame.spills[0].pointer)
	assert.Equal(t, 16, frame.spills[0].offset, "the home slots follow the stack-passed argument")
}

// A grouped aggregate that lands on the stack is caller-written like any other
// stack argument, so all of its pointer words belong to both maps.
func TestManagedAAPCS64KeepsStackedAggregateWordsInBothArgumentMaps(t *testing.T) {
	interfaceType := &ir.AggType{Name: "interface", Fields: []ir.Field{
		{Sub: ir.SubL, Pointer: true},
		{Sub: ir.SubL, Pointer: true},
	}}
	function := ir.NewModule().NewFuncVoid("consume_stacked_interface")
	function.ManagedFrame = true
	for index := 0; index < 7; index++ {
		function.Param(fmt.Sprintf("word%d", index), ir.ClsL)
	}
	function.ParamGroup("value", interfaceType, ir.ClsP, ir.ClsP)
	function.Entry().RetVoid()

	frame := goArgumentFrameFor(function)
	assert.Equal(t, []int{0, 1}, frame.incomingPointerWords)
	assert.Equal(t, []int{0, 1}, frame.pointerWords)
}

// A function with no stack-passed pointer argument has an empty body argument
// map, so no safepoint reports a root in the argument frame at all.
func TestManagedAAPCS64LeavesRegisterOnlyArgumentsOutOfTheBodyMap(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("consume_two_pointers")
	function.ManagedFrame = true
	function.ParamRef("left")
	function.ParamRef("right")
	function.Entry().RetVoid()

	frame := goArgumentFrameFor(function)
	assert.Equal(t, []int{0, 1}, frame.pointerWords)
	assert.Empty(t, frame.incomingPointerWords)
}

func TestManagedAAPCS64DoesNotHomeEmptyAggregate(t *testing.T) {
	empty := &ir.AggType{Name: "empty"}
	function := ir.NewModule().NewFuncVoid("consume_empty")
	function.ManagedFrame = true
	aggParam(function, "value", empty)

	frame := goArgumentFrameFor(function)
	require.Empty(t, frame.spills)
	require.Zero(t, frame.size)
}

func TestManagedAAPCS64CallerReservesRegisterHomes(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("call_two_pointers", ir.ClsL)
	function.ManagedFrame = true
	left := function.ParamRef("left")
	right := function.ParamRef("right")
	result := function.Entry().Call(ir.ClsL, function.Sym("hash", 0), left, right)
	function.Entry().Ret(result)

	assembly := disasmModule(t, module)
	callOffset := strings.Index(assembly, "bl hash")
	require.NotEqual(t, -1, callOffset)
	assert.Contains(t, assembly[:callOffset], "sub sp, sp, #48")
	assert.NotContains(t, assembly[callOffset:], "add sp, sp, #32")
}

func TestGoABIRematerializesFixedStackAddressesAfterCalls(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("stack_address", ir.ClsW)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	entry := function.Entry()
	value := entry.Alloc(8, 8)
	entry.Store(function.Long(42), value)
	entry.CallVoid(function.Sym("observe", 0))
	entry.Ret(entry.Load(ir.ClsW, value))

	assembly := disasmModule(t, module)
	callOffset := strings.Index(assembly, "bl observe")
	require.NotEqual(t, -1, callOffset)
	assert.Contains(t, assembly[callOffset:], "add x17, x29", "the post-call load must derive the local address from the current frame")
}

func TestGoABIRematerializesCopiedFixedStackAddressesAfterCalls(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("copied_stack_address", ir.ClsW)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	entry := function.Entry()
	value := entry.Alloc(8, 8)
	copiedAddress := entry.Copy(ir.ClsP, value)
	entry.Store(function.Long(42), copiedAddress)
	entry.CallVoid(function.Sym("observe", 0))
	entry.Ret(entry.Load(ir.ClsW, copiedAddress))

	assembly := disasmModule(t, module)
	callOffset := strings.Index(assembly, "bl observe")
	require.NotEqual(t, -1, callOffset)
	assert.Contains(t, assembly[callOffset:], "add x17, x29", "the copied post-call address must be derived from the current frame")
}

func TestInferFrameAddressOffsetsHandlesLongReverseDependencyChain(t *testing.T) {
	const chainLength = 20_000

	function := ir.NewModule().NewFuncVoid("reverse_frame_addresses")
	addresses := make([]ir.Ref, chainLength+1)
	for index := range addresses {
		addresses[index] = function.NewTemp(fmt.Sprintf("address%d", index), ir.ClsP)
	}
	one := function.Long(1)
	additionCount := 0
	for index := 0; index < chainLength; index++ {
		instruction := ir.Instr{
			Op:   ir.OCopy,
			Cls:  ir.ClsP,
			To:   addresses[index],
			Args: []ir.Ref{addresses[index+1]},
		}
		if index%2 == 0 {
			instruction.Op = ir.OAdd
			instruction.Args = append(instruction.Args, one)
			additionCount++
		}
		block := function.NewBlock(fmt.Sprintf("dependency%d", index))
		block.Instrs = append(block.Instrs, instruction)
	}

	const rootOffset = 96
	machine := &mc{
		f:        function,
		allocTmp: map[uint32]int{addresses[chainLength].ID: rootOffset},
	}
	machine.inferFrameAddressOffsets()

	require.Len(t, machine.allocTmp, chainLength+1)
	assert.Equal(t, rootOffset+additionCount, machine.allocTmp[addresses[0].ID])
}

func TestInferFrameAddressOffsetsDoesNotCollapsePhiCopies(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("selected_frame_address")
	leftAddress := function.NewTemp("left", ir.ClsP)
	rightAddress := function.NewTemp("right", ir.ClsP)
	selectedAddress := function.NewTemp("selected", ir.ClsP)

	leftBlock := function.NewBlock("left")
	leftBlock.Instrs = append(leftBlock.Instrs, ir.Instr{
		Op:   ir.OCopy,
		Cls:  ir.ClsP,
		To:   selectedAddress,
		Args: []ir.Ref{leftAddress},
	})
	rightBlock := function.NewBlock("right")
	rightBlock.Instrs = append(rightBlock.Instrs, ir.Instr{
		Op:   ir.OCopy,
		Cls:  ir.ClsP,
		To:   selectedAddress,
		Args: []ir.Ref{rightAddress},
	})

	machine := &mc{
		f: function,
		allocTmp: map[uint32]int{
			leftAddress.ID:  40,
			rightAddress.ID: 80,
		},
	}
	machine.inferFrameAddressOffsets()

	_, inferred := machine.allocTmp[selectedAddress.ID]
	assert.False(t, inferred, "a runtime-selected frame address must not be replaced by one incoming offset")
}

func TestGoABINoSplitOmitsStackGrowthCheck(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("nosplit")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	function.NoSplit = true
	function.Entry().RetVoid()

	assembly := disasmModule(t, module)
	assert.NotContains(t, assembly, "runtime_morestack_noctxt")
}

func TestGoABISystemStackUsesSystemStackGuard(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("system_stack")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	function.SystemStack = true
	function.Entry().RetVoid()

	assembly := disasmModule(t, module)
	assert.Contains(t, assembly, "ldr x16, [x28, #24]")
	assert.Contains(t, assembly, "runtime_morestackc")
	assert.NotContains(t, assembly, "runtime_morestack_noctxt")
}

func TestManagedFrameMetadataStartsAfterFrameAllocation(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("metadata_start")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	function.ManagedFrame = true
	entry := function.Entry()
	slot := entry.Alloc(8, 1024)
	entry.Store(function.ConstInt(ir.ClsL, 1), slot)
	entry.RetVoid()

	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))
	allocation, err := regAlloc(function)
	require.NoError(t, err)
	machine, err := emitMachine(function, allocation, moduleConventions(function), nil, TLSLocalExec)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, machine.m.frameStart, 44)
	assert.Equal(t, 0, machine.m.frameStart%4)
}

func TestGoABICallerFrameIntrinsicsUseSavedFrame(t *testing.T) {
	pcModule := ir.NewModule()
	callerPC := pcModule.NewFunc("caller_pc", ir.ClsL)
	callerPC.CallConv = ir.CallConvGoInternal
	callerPC.ManagedFrame = true
	callerPC.NoSplit = true
	callerPC.Entry().Ret(callerPC.Entry().CallerPC())

	pcAssembly := disasmModule(t, pcModule)
	assert.Contains(t, pcAssembly, "ldr x0, [x29, #8]")

	spModule := ir.NewModule()
	callerSP := spModule.NewFunc("caller_sp", ir.ClsL)
	callerSP.CallConv = ir.CallConvGoInternal
	callerSP.ManagedFrame = true
	callerSP.NoSplit = true
	callerSP.Entry().Ret(callerSP.Entry().CallerSP())

	spAssembly := disasmModule(t, spModule)
	assert.Contains(t, spAssembly, "add x0, x29, #16")
}

func TestManagedAAPCS64OutgoingArgumentsDoNotOverlapFrameRecord(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("wide_call")
	function.ManagedFrame = true
	function.NoSplit = true

	arguments := make([]ir.Ref, 9)
	for index := range arguments {
		arguments[index] = function.Long(int64(index + 1))
	}
	function.Entry().CallVoid(function.Sym("callee", 0), arguments...)
	function.Entry().RetVoid()

	assembly := disasmModule(t, module)
	assert.Contains(t, assembly, "stp x29, x30, [x17]", "frame record must sit above the outgoing area")
	assert.Contains(t, assembly, "str x", "the ninth argument must be stored in the outgoing area")
	assert.Contains(t, assembly, "[sp]", "AAPCS64 stack arguments begin at the stable SP")
	assert.NotContains(t, assembly, "stp x29, x30, [sp]", "the frame record must not share [sp] with the ninth argument")
}

func TestGoABISpillsPointerLiveAcrossCall(t *testing.T) {
	module := ir.NewModule()
	f := module.NewFunc("pointer_across_call", ir.ClsP)
	f.CallConv = ir.CallConvGoInternal
	f.ManagedFrame = true
	pointer := f.ParamRef("pointer")
	entry := f.Entry()
	entry.CallVoid(f.Sym("observe", 0))
	entry.Ret(pointer)

	assembly := disasmModule(t, module)
	assert.Equal(t, ir.NoReg, f.Temps[pointer.ID].Reg)
	assert.Contains(t, assembly, "str xzr, [x29")
}

func TestGoABISpillsScalarLiveAcrossCall(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("scalar_across_call", ir.ClsW)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	function.ManagedFrame = true
	value := function.Param("value", ir.ClsW)

	entry := function.Entry()
	entry.CallVoid(function.Sym("observe", 0), function.Word(1), function.Word(2), function.Word(3), function.Word(4))
	entry.Ret(value)

	disasmModule(t, module)
	temporary := function.Temp(value)
	assert.Equal(t, ir.NoReg, temporary.Reg)
	assert.GreaterOrEqual(t, temporary.Slot, 0)
}

func TestSafepointRootsIncludeManagedPointerLiveAcrossCall(t *testing.T) {
	// A GC root is identified purely by value: a GCRef pointer live across a
	// safepoint is reported, regardless of calling convention. A copying-stack
	// frontend relies on this by marking every pointer that must survive stack
	// growth (see TestRuntimeStackGrowthRelocatesInteriorPointers for the
	// end-to-end guarantee); the register allocator itself is ABI-agnostic.
	function := ir.NewModule().NewFunc("pointer_across_call", ir.ClsP)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	ref := function.Param("ref", ir.ClsP)
	n := function.Param("n", ir.ClsW)
	function.Temp(ref).GCRef = true
	entry := function.Entry()
	result := entry.Call(ir.ClsW, function.Sym("observe", 0), n)
	entry.Ret(ref)

	cfg := analysis.BuildCFG(function)
	liveness := cfg.Liveness()
	roots := computeSafepointRoots(function, cfg, liveness)

	call := &entry.Instrs[0]
	require.Equal(t, result, call.To)
	require.Contains(t, roots, call)
	require.Contains(t, roots[call], int(ref.ID))
}

func TestSafepointRootsExcludeDeadControlFlowIntervals(t *testing.T) {
	function := ir.NewModule().NewFunc("branch_root", ir.ClsP)
	pointer := function.ParamRef("pointer")
	condition := function.Param("condition", ir.ClsW)
	entry := function.Entry()
	usePointer := function.NewBlock("use_pointer")
	safepointBlock := function.NewBlock("safepoint")

	// With this successor order, reverse postorder places safepoint between the
	// pointer's entry definition and its use in usePointer. Its conservative
	// linear interval therefore crosses the safepoint even though CFG liveness
	// correctly says it is dead on that branch.
	entry.Jnz(condition, usePointer, safepointBlock)
	usePointer.Ret(pointer)
	safepointBlock.Safepoint()
	safepointBlock.Ret(function.ConstInt(ir.ClsP, 0))

	cfg := analysis.BuildCFG(function)
	liveness := cfg.Liveness()
	numbering := numberInstrs(cfg)
	intervals := buildIntervals(function, cfg, liveness, numbering)
	safepoint := &safepointBlock.Instrs[0]
	safepointPosition := -1
	for position, instruction := range numbering.posInstr {
		if instruction == safepoint {
			safepointPosition = position
			break
		}
	}
	require.NotEqual(t, -1, safepointPosition)

	var pointerInterval *interval
	for _, candidate := range intervals {
		if candidate.temp == int(pointer.ID) {
			pointerInterval = candidate
			break
		}
	}
	require.NotNil(t, pointerInterval)
	require.Less(t, pointerInterval.start, safepointPosition)
	require.Greater(t, pointerInterval.end, safepointPosition)

	roots := computeSafepointRoots(function, cfg, liveness)
	assert.Empty(t, roots[safepoint])
}

func TestGoStackMapsDropDeadPointerBearingLocal(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("dead_pointer_local")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	input := function.ParamRef("input")
	entry := function.Entry()
	local := entry.Alloc(8, 8)
	function.MarkGCRef(local)
	function.StackPointerWords = map[uint32]map[int]bool{
		local.ID: map[int]bool{0: true},
	}
	entry.Store(input, local)
	entry.CallVoid(function.Sym("first", 0))
	entry.Store(function.ConstInt(ir.ClsP, 0), local)
	entry.CallVoid(function.Sym("second", 0))
	entry.RetVoid()

	prepareGoABI(function)
	ir.LowerPointers(function, ptrCls)
	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))
	allocation, err := regAlloc(function)
	require.NoError(t, err)
	machine, err := emitMachine(function, allocation, moduleConventions(function), nil, TLSLocalExec)
	require.NoError(t, err)

	points := machine.m.goStackMapPoints()
	require.Len(t, points, 2)
	require.Len(t, machine.m.safepoints, 2)
	assert.Equal(t, int(machine.m.safepoints[0].startPC), points[0].PC)
	assert.Equal(t, int(machine.m.safepoints[1].startPC), points[1].PC)
	localOffset := machine.m.stackAllocTmp[local.ID]
	localWord := (localOffset - 16) / 8
	assert.Contains(t, points[0].PointerWords, localWord)
	assert.NotContains(t, points[1].PointerWords, localWord)
}

// The emitted map, not just the safepoint root set, has to drop the dead local.
// The conservative function-wide map used to be unioned into every safepoint,
// which put the word back and kept whatever the slot last held reachable for the
// life of the frame -- the over-retention bug of RUNTIME_PLAN.md 5.3.
func TestGoEmittedStackMapDropsDeadPointerBearingLocal(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("dead_pointer_local_emitted")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	input := function.ParamRef("input")
	entry := function.Entry()
	local := entry.Alloc(8, 8)
	function.MarkGCRef(local)
	function.StackPointerWords = map[uint32]map[int]bool{
		local.ID: {0: true},
	}
	entry.Store(input, local)
	// The local is read after the first call and never again, so it is live at
	// the first safepoint and dead at the second.
	entry.CallVoid(function.Sym("first", 0))
	entry.Store(entry.Load(ir.ClsP, local), function.Sym("sink", 0))
	entry.CallVoid(function.Sym("second", 0))
	entry.RetVoid()

	machine := compileGoFunctionForStackMaps(t, function)
	information, err := goFunctionInfoFor(function, "dead_pointer_local_emitted", machine)
	require.NoError(t, err)

	localWord := (machine.m.stackAllocTmp[local.ID] - 16) / 8
	assert.Contains(t, information.LocalPointerWords, localWord)

	pointerMaps, indexPoints := gometa.FunctionStackMaps(information)
	require.Len(t, indexPoints, 2)
	assert.Contains(t, pointerMaps[indexPoints[0].Index], localWord)
	assert.NotContains(t, pointerMaps[indexPoints[1].Index], localWord)
}

// A local reached only through a derived interior address stays scanned. cg12
// approximates a local's liveness by the liveness of the temporary holding its
// address, so a derived address that outlives that temporary would otherwise
// leave the local's pointer words unscanned while code can still read them.
func TestGoStackMapsRetainLocalReachedThroughDerivedAddress(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("derived_address_local")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	input := function.ParamRef("input")
	entry := function.Entry()
	local := entry.Alloc(8, 16)
	function.MarkGCRef(local)
	function.StackPointerWords = map[uint32]map[int]bool{
		local.ID: {8: true},
	}
	// After this addition nothing uses the allocation's own temporary again, so
	// only the derived address keeps the local reachable.
	field := entry.Add(ir.ClsP, local, function.Long(8))
	entry.Store(input, field)
	entry.CallVoid(function.Sym("between", 0))
	entry.Store(entry.Load(ir.ClsP, field), function.Sym("sink", 0))
	entry.RetVoid()

	machine := compileGoFunctionForStackMaps(t, function)
	points := machine.m.goStackMapPoints()
	require.Len(t, points, 1)

	fieldWord := (machine.m.stackAllocTmp[local.ID] + 8 - 16) / 8
	assert.Contains(t, points[0].PointerWords, fieldWord)
}

// goc builds an interface argument as a stack descriptor and then nil-checks it
// against a zeroed one, so a destructed phi merges two allocation addresses. A
// single-valued derivation map could not express that merge and fell back to
// making both allocations conservative for the life of the frame -- which is
// what kept runtime.SetFinalizer's argument, and the object it names, reachable
// forever (RUNTIME_PLAN.md 5.3). The merged temporary now tracks both, so each
// allocation is a root exactly where an address into it is live.
func TestGoStackMapsDropMergedInterfaceDescriptorsAfterTheirCall(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("merged_interface_descriptor")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	object := function.ParamRef("object")
	entry := function.Entry()
	descriptor := entry.Alloc(8, 16)
	zeroed := entry.Alloc(8, 16)
	function.MarkGCRef(descriptor)
	function.MarkGCRef(zeroed)
	function.StackPointerWords = map[uint32]map[int]bool{
		descriptor.ID: {8: true},
		zeroed.ID:     {8: true},
	}
	entry.Store(object, entry.Add(ir.ClsP, descriptor, function.Long(8)))
	entry.Store(function.ConstInt(ir.ClsP, 0), entry.Add(ir.ClsP, zeroed, function.Long(8)))

	useZero := function.NewBlock("callinterfacezero")
	useValue := function.NewBlock("callinterfacevalue")
	done := function.NewBlock("callinterfaceend")
	isNil := entry.Cmp(ir.CmpEq, ir.ClsW, descriptor, function.ConstInt(ir.ClsP, 0))
	entry.Jnz(isNil, useZero, useValue)
	useZero.Goto(done)
	useValue.Goto(done)
	selected := done.Phi(ir.ClsP,
		ir.PhiEdge{From: useZero, Val: zeroed},
		ir.PhiEdge{From: useValue, Val: descriptor},
	)

	// The first call still has the merged address live, so both descriptors are
	// roots there. The second consumes their words the way an ABIInternal
	// interface argument does, after which nothing addresses either allocation.
	done.CallVoid(function.Sym("between", 0))
	typeWord := done.Load(ir.ClsP, selected)
	dataWord := done.Load(ir.ClsP, done.Add(ir.ClsP, selected, function.Long(8)))
	done.CallVoid(function.Sym("consume", 0), typeWord, dataWord)
	done.CallVoid(function.Sym("collect", 0))
	done.RetVoid()

	machine := compileGoFunctionForStackMaps(t, function)
	points := machine.m.goStackMapPoints()
	require.Len(t, points, 3)

	descriptorData := (machine.m.stackAllocTmp[descriptor.ID] + 8 - 16) / 8
	zeroedData := (machine.m.stackAllocTmp[zeroed.ID] + 8 - 16) / 8
	assert.Contains(t, points[0].PointerWords, descriptorData)
	assert.Contains(t, points[0].PointerWords, zeroedData)
	assert.NotContains(t, points[1].PointerWords, descriptorData)
	assert.NotContains(t, points[1].PointerWords, zeroedData)
	assert.NotContains(t, points[2].PointerWords, descriptorData)
	assert.NotContains(t, points[2].PointerWords, zeroedData)
}

// The escape boundary is what keeps the three capabilities named in
// RUNTIME_PLAN.md 5.3 correct, so the cases that must stay conservative are
// asserted directly rather than only through the programs that notice them.
func TestFrameEscapingAllocationsKeepPublishedAddressesConservative(t *testing.T) {
	tests := []struct {
		name     string
		publish  func(function *ir.Func, entry *ir.Block, local ir.Ref)
		escaping bool
	}{
		{
			name: "passed to a call",
			publish: func(function *ir.Func, entry *ir.Block, local ir.Ref) {
				entry.CallVoid(function.Sym("observe", 0), local)
			},
			escaping: true,
		},
		{
			name: "stored into memory",
			publish: func(function *ir.Func, entry *ir.Block, local ir.Ref) {
				entry.Store(local, function.Sym("sink", 0))
			},
			escaping: true,
		},
		{
			name: "returned",
			publish: func(function *ir.Func, entry *ir.Block, local ir.Ref) {
				entry.Ret(local)
			},
			escaping: true,
		},
		{
			name: "added to a runtime value",
			publish: func(function *ir.Func, entry *ir.Block, local ir.Ref) {
				offset := entry.Load(ir.ClsL, function.Sym("offset", 0))
				entry.Store(entry.Add(ir.ClsP, local, offset), function.Sym("sink", 0))
			},
			escaping: true,
		},
		{
			// A closure environment is carried outside Instr.Args, so the operand
			// walk has to read it explicitly or the address leaves unnoticed.
			name: "handed to a callee as a closure environment",
			publish: func(function *ir.Func, entry *ir.Block, local ir.Ref) {
				entry.CallVoid(function.Sym("invoke", 0))
				call := &entry.Instrs[len(entry.Instrs)-1]
				call.ClosureCall = true
				call.ClosureContext = local
			},
			escaping: true,
		},
		{
			name: "compared against nil",
			publish: func(function *ir.Func, entry *ir.Block, local ir.Ref) {
				isNil := entry.Cmp(ir.CmpEq, ir.ClsW, local, function.ConstInt(ir.ClsP, 0))
				entry.Store(isNil, function.Sym("sink", 0))
			},
			escaping: false,
		},
		{
			name: "selected between",
			publish: func(function *ir.Func, entry *ir.Block, local ir.Ref) {
				condition := entry.Load(ir.ClsW, function.Sym("condition", 0))
				chosen := entry.Select(ir.ClsP, condition, local, function.ConstInt(ir.ClsP, 0))
				entry.Store(entry.Load(ir.ClsP, chosen), function.Sym("sink", 0))
			},
			escaping: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			function := ir.NewModule().NewFunc("published_local", ir.ClsP)
			function.CallConv = ir.CallConvGoInternal
			function.ManagedFrame = true
			entry := function.Entry()
			local := entry.Alloc(8, 8)
			function.StackPointerWords = map[uint32]map[int]bool{
				local.ID: {0: true},
			}
			test.publish(function, entry, local)
			if entry.Jmp.Kind == ir.JmpNone {
				entry.Ret(function.ConstInt(ir.ClsP, 0))
			}

			allocationsOf := pointerAllocationSources(function)
			require.Contains(t, allocationsOf, int(local.ID))
			escaping := frameEscapingAllocations(function, allocationsOf)
			assert.Equal(t, test.escaping, escaping[int(local.ID)])
		})
	}
}

// A temporary that may address either of two allocations has to name both, or
// the one it does not name goes unscanned on the path that reaches it.
func TestPointerAllocationSourcesTrackEveryMergedAllocation(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("merged_allocations")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	entry := function.Entry()
	first := entry.Alloc(8, 8)
	second := entry.Alloc(8, 8)
	function.StackPointerWords = map[uint32]map[int]bool{
		first.ID:  {0: true},
		second.ID: {0: true},
	}

	useFirst := function.NewBlock("first")
	useSecond := function.NewBlock("second")
	done := function.NewBlock("done")
	condition := entry.Load(ir.ClsW, function.Sym("condition", 0))
	entry.Jnz(condition, useFirst, useSecond)
	useFirst.Goto(done)
	useSecond.Goto(done)
	selected := done.Phi(ir.ClsP,
		ir.PhiEdge{From: useFirst, Val: first},
		ir.PhiEdge{From: useSecond, Val: second},
	)
	// A derived address keeps the merge alive past the phi's own temporary.
	field := done.Add(ir.ClsP, selected, function.Long(0))
	done.Store(done.Load(ir.ClsP, field), function.Sym("sink", 0))
	done.RetVoid()

	allocationsOf := pointerAllocationSources(function)
	assert.Equal(t, []int{int(first.ID), int(second.ID)}, allocationsOf[int(selected.ID)])
	assert.Equal(t, []int{int(first.ID), int(second.ID)}, allocationsOf[int(field.ID)])
	assert.Empty(t, frameEscapingAllocations(function, allocationsOf))
}

func compileGoFunctionForStackMaps(t *testing.T, function *ir.Func) *machineCode {
	t.Helper()

	prepareGoABI(function)
	ir.LowerPointers(function, ptrCls)
	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))
	allocation, err := regAlloc(function)
	require.NoError(t, err)
	machine, err := emitMachine(function, allocation, moduleConventions(function), nil, TLSLocalExec)
	require.NoError(t, err)
	return machine
}

func TestGoABIGroupedSliceValuesUseRegistersOrWholeStack(t *testing.T) {
	sliceType := &ir.AggType{
		Name:  "slice",
		Align: 8,
		Fields: []ir.Field{
			{Sub: ir.SubL, Pointer: true},
			{Sub: ir.SubL},
			{Sub: ir.SubL},
		},
	}

	calleeModule := ir.NewModule()
	callee := calleeModule.NewFunc("consume_slice", ir.ClsL)
	callee.CallConv = ir.CallConvGoInternal
	callee.ManagedFrame = true
	callee.NoSplit = true
	for index := 0; index < 14; index++ {
		callee.Param(fmt.Sprintf("value%d", index), ir.ClsL)
	}
	parts := callee.ParamGroup("values", sliceType, ir.ClsP, ir.ClsL, ir.ClsL)
	callee.Entry().Ret(parts[1])
	calleeAssembly := disasmModule(t, calleeModule)
	assert.Contains(t, calleeAssembly, "ldr x0, [x29, #32]")

	callerModule := ir.NewModule()
	caller := callerModule.NewFunc("call_slice_len", ir.ClsL)
	caller.CallConv = ir.CallConvGoInternal
	caller.ManagedFrame = true
	caller.NoSplit = true
	entry := caller.Entry()
	arguments := make([]ir.Ref, 0, 17)
	for index := 0; index < 14; index++ {
		arguments = append(arguments, caller.Long(int64(index)))
	}
	arguments = append(arguments,
		caller.ConstInt(ir.ClsP, 0),
		caller.Long(7),
		caller.Long(9),
	)
	result := entry.Call(ir.ClsL, caller.Sym("consume_slice", 0), arguments...)
	call := &entry.Instrs[len(entry.Instrs)-1]
	call.ArgGroups = []ir.ValueGroup{{Index: 14, Count: 3, Type: sliceType}}
	entry.Ret(result)

	callerAssembly := disasmModule(t, callerModule)
	assert.Contains(t, callerAssembly, "[sp, #8]")
	assert.Contains(t, callerAssembly, "[sp, #16]")
	assert.Contains(t, callerAssembly, "[sp, #24]")

	argumentFrame := goArgumentFrameFor(callee)
	assert.Contains(t, argumentFrame.pointerWords, 0, "stack-passed slice data must remain visible to the garbage collector")
}

func TestAAPCSStackMemoryAggregateParameterLoadsSpilledPointer(t *testing.T) {
	big := &ir.AggType{
		Name:  "big",
		Align: 8,
		Fields: []ir.Field{
			{Sub: ir.SubL, Pointer: true},
			{Sub: ir.SubL, Pointer: true},
			{Sub: ir.SubL},
		},
	}
	module := ir.NewModule()
	module.AddType(big)
	function := module.NewFunc("stack_memory_aggregate_pointer", ir.ClsL)
	for index := 0; index < 8; index++ {
		function.Param(fmt.Sprintf("value%d", index), ir.ClsL)
	}
	aggregate := function.Param("aggregate", ir.ClsP)
	aggregateTemp := function.Temp(aggregate)
	aggregateTemp.Agg = big
	aggregateTemp.Fixed = true
	aggregateTemp.Reg = int(X0)
	entry := function.Entry()
	entry.Ret(entry.Load(ir.ClsL, aggregate))

	assembly := disasmModule(t, module)
	assert.Contains(t, assembly, "ldr x0, [x29")
	assert.NotContains(t, assembly, "add x0, x29")
}

func TestGoABIGroupedSliceResultUsesThreeValueRegisters(t *testing.T) {
	sliceType := &ir.AggType{
		Name:  "slice_result",
		Align: 8,
		Fields: []ir.Field{
			{Sub: ir.SubL, Pointer: true},
			{Sub: ir.SubL},
			{Sub: ir.SubL},
		},
	}

	module := ir.NewModule()
	function := module.NewFunc("return_slice", ir.ClsP)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	function.NoSplit = true
	function.RetAgg = sliceType
	function.RetValues = true
	entry := function.Entry()
	entry.RetAggregate(function.ConstInt(ir.ClsP, 0), function.Long(3), function.Long(5))

	assembly := disasmModule(t, module)
	assert.Contains(t, assembly, "x0")
	assert.Contains(t, assembly, "x1")
	assert.Contains(t, assembly, "x2")
}

func TestGoABIStackGrowthPreservesClosureContext(t *testing.T) {
	function := ir.NewModule().NewFunc("closure_value", ir.ClsL)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	function.HasClosureContext = true
	context := function.NewTemp("closure", ir.ClsP)
	temporary := function.Temp(context)
	temporary.Fixed = true
	temporary.Reg = 26
	temporary.ClosureContext = true
	function.Entry().Ret(function.Entry().Load(ir.ClsL, context))

	argumentFrame := goArgumentFrameFor(function)
	foundClosureSpill := false
	for _, spill := range argumentFrame.spills {
		if spill.reg == X26 {
			foundClosureSpill = true
			assert.True(t, spill.pointer)
		}
	}
	assert.True(t, foundClosureSpill, "closure context must survive morestack")
}

func TestGoABIClosureContextLiveAcrossCallGetsStableSpill(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("closure_across_call", ir.ClsL)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	function.HasClosureContext = true
	context := function.NewTemp("closure", ir.ClsP)
	contextTemporary := function.Temp(context)
	contextTemporary.GCRef = true
	contextTemporary.Fixed = true
	contextTemporary.Reg = int(X26)
	contextTemporary.ClosureContext = true
	entry := function.Entry()
	entry.CallVoid(function.Sym("observe", 0))
	entry.Ret(entry.Load(ir.ClsL, context))

	disasmModule(t, module)
	var saved *ir.Temp
	for _, temporary := range function.Temps {
		if temporary.Name == "closure.saved" {
			saved = temporary
			break
		}
	}
	require.NotNil(t, saved)
	assert.Equal(t, ir.NoReg, saved.Reg)
	assert.GreaterOrEqual(t, saved.Slot, 0)
}

func TestGoABICallSiteClosureRegisterIsNotAnIncomingContext(t *testing.T) {
	function := ir.NewModule().NewFunc("call_function_value", ir.ClsL)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	context := function.NewTemp("call_context", ir.ClsP)
	temporary := function.Temp(context)
	temporary.Fixed = true
	temporary.Reg = 26
	function.Entry().Ret(function.Long(1))

	argumentFrame := goArgumentFrameFor(function)
	for _, spill := range argumentFrame.spills {
		assert.NotEqual(t, X26, spill.reg)
	}
}

func TestGoABIClosureCallReservesContextSpillWord(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("closure_caller")
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	callee := function.ParamRef("callee")
	function.Entry().CallVoid(callee)
	call := &function.Entry().Instrs[len(function.Entry().Instrs)-1]
	call.ClosureCall = true
	function.Entry().RetVoid()

	prepareGoABI(function)
	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))
	var loweredCall *ir.Instr
	for index := range function.Entry().Instrs {
		instruction := &function.Entry().Instrs[index]
		if instruction.Op == ir.OCall {
			loweredCall = instruction
		}
	}
	require.NotNil(t, loweredCall)
	assert.Equal(t, int64(16), loweredCall.Aux)
}

func TestGoABIReportsPointerResultSlotLiveAcrossCall(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("multi_result", ir.ClsP)
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = true
	function.Param("size", ir.ClsL)
	result := function.ParamRef("result1")
	entry := function.Entry()
	entry.CallVoid(function.Sym("observe", 0))
	entry.Store(function.Long(8), result)
	entry.Ret(function.ConstInt(ir.ClsP, 0))

	prepareGoABI(function)
	ir.LowerPointers(function, ptrCls)
	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))
	allocation, err := regAlloc(function)
	require.NoError(t, err)
	frame := computeFrame(function, allocation, moduleConventions(function))
	words := goPointerWordIndexes(function, frame.allocOff, frame.spillBase)
	assert.Contains(t, words, 0)
}

func TestGoPointerFrameOffsetsIgnoreUnassignedSpillSlots(t *testing.T) {
	f := ir.NewModule().NewFunc("pointer_slots", ir.ClsW)
	f.CallConv = ir.CallConvGoInternal
	f.ManagedFrame = true
	unassigned := f.NewTemp("unassigned", ir.ClsP)
	f.Temps[unassigned.ID].GCRef = true
	spilled := f.NewTemp("spilled", ir.ClsP)
	f.Temps[spilled.ID].GCRef = true
	f.Temps[spilled.ID].Slot = 8

	assert.Equal(t, []int{40}, goPointerFrameOffsets(f, nil, 32))
}

func TestCompileLargeFrame(t *testing.T) {
	// A frame larger than the stp pre-index reach (504 bytes) must adjust sp
	// separately in the prologue and epilogue. Add/sub immediate only carries 12
	// bits, and the register form cannot target sp -- register 31 is the zero
	// register there -- so a frame past 4095 is subtracted in two goes: a shifted
	// high half and a low half.
	compileAlloc := func(size int) string {
		m := ir.NewModule()
		f := m.NewFunc("bf", ir.ClsW).Export()
		x := f.Param("x", ir.ClsW)
		e := f.Entry()
		p := e.Alloc(4, size)
		e.Store(x, p)
		e.Ret(e.Load(ir.ClsW, p))
		return disasmModule(t, m)
	}

	small := compileAlloc(2048) // frame 2064: one immediate reaches it
	assert.Contains(t, small, "sub sp, sp, #2064")
	assert.Contains(t, small, "add sp, sp, #2064")
	assert.NotContains(t, small, "stp x29, x30, [sp, #-") // not the pre-index form

	// frame 9024 = 2<<12 + 832, which no single immediate reaches.
	huge := compileAlloc(9000)
	assert.Contains(t, huge, "sub sp, sp, #2, lsl #12")
	assert.Contains(t, huge, "sub sp, sp, #832")
	assert.Contains(t, huge, "add sp, sp, #2, lsl #12")
	assert.Contains(t, huge, "add sp, sp, #832")
}

func TestFramelessLeaf(t *testing.T) {
	// A leaf function whose only frame would be the x29/x30 save needs no frame at
	// all: nothing clobbers x30 (the return address stays live to `ret`) and x29 is
	// never allocated. So the prologue and epilogue vanish. A function that calls,
	// by contrast, must save x30 and keeps its frame.
	leaf := func() string {
		m := ir.NewModule()
		f := m.NewFunc("leaf", ir.ClsL).Export()
		a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
		e := f.Entry()
		e.Ret(e.Add(ir.ClsL, e.Mul(ir.ClsL, a, b), a))
		return disasmModule(t, m)
	}()
	assert.NotContains(t, leaf, "stp x29, x30", "leaf function needs no frame")
	assert.NotContains(t, leaf, "ldp x29, x30", "leaf function needs no epilogue")
	assert.Contains(t, leaf, "ret", "still returns")

	caller := func() string {
		m := ir.NewModule()
		g := m.NewFunc("g", ir.ClsL)
		g.Param("x", ir.ClsL)
		g.Entry().Ret(g.Long(0))
		f := m.NewFunc("caller", ir.ClsL).Export()
		p := f.Param("p", ir.ClsL)
		e := f.Entry()
		e.Ret(e.Call(ir.ClsL, f.Sym("g", 0), p))
		return disasmModule(t, m)
	}()
	assert.Contains(t, caller, "stp x29, x30", "a function that calls must save the return address")
}

// Static data reaches the object as bytes, a symbol, and -- where it names
// another symbol -- a relocation. Checking those directly says what the data
// actually is; the directives it used to be spelled with were only a rendering
// of it.
func TestCompileModuleWithData(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data,
		// One byte first, so g's alignment has to pad rather than happening to hold.
		&ir.Data{Name: "pad", Items: []ir.DataItem{{Sub: ir.SubB, Ints: []int64{7}}}},
		&ir.Data{
			Name:    "g",
			Linkage: ir.Linkage{Export: true},
			Align:   8,
			Items: []ir.DataItem{
				{Sub: ir.SubW, Ints: []int64{1, 2}},
				{Zero: 4},
				{Str: "hi"},
				{Sub: ir.SubL, Sym: "other", Off: 8},
			},
		})
	f := m.NewFuncVoid("noop")
	f.Entry().RetVoid()

	o, err := CompileToObject(m)
	require.NoError(t, err)

	g := findSym(t, o, "g")
	assert.Equal(t, obj.SecData, g.Section)
	assert.True(t, g.Global, "an exported datum is visible to the linker")
	assert.Zero(t, g.Value%8, "Align: 8 is honoured, so the leading byte is padded over")

	// The items in order: two words, four zero bytes, the string, and then eight
	// bytes left for the linker to fill in.
	assert.Equal(t, []byte{
		1, 0, 0, 0,
		2, 0, 0, 0,
		0, 0, 0, 0,
		'h', 'i',
		0, 0, 0, 0, 0, 0, 0, 0,
	}, o.Data[g.Value:g.Value+g.Size])

	// The address of another symbol is not something we can write down, so it is
	// a relocation over those last eight bytes.
	assert.Contains(t, o.DataRelocs, obj.Reloc{
		Offset: g.Value + 14, Sym: "other", Type: obj.R_AARCH64_ABS64, Addend: 8,
	})
}

func TestCompileModuleWithRelativeAndWordSymbolData(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "base", Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}},
		&ir.Data{Name: "target", Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{42}}}},
		&ir.Data{Name: "offset", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Sym: "target", RelativeTo: "base"}}},
		&ir.Data{Name: "address", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Sym: "function"}}},
		&ir.Data{Name: "textoffset", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Sym: "target_function", RelativeTo: "function"}}},
	)
	function := m.NewFuncVoid("function")
	function.Entry().RetVoid()
	targetFunction := m.NewFuncVoid("target_function")
	targetFunction.Entry().RetVoid()

	o, err := CompileToObject(m)
	require.NoError(t, err)

	base := findSym(t, o, "base")
	target := findSym(t, o, "target")
	offset := findSym(t, o, "offset")
	address := findSym(t, o, "address")
	textOffset := findSym(t, o, "textoffset")
	functionSymbol := findSym(t, o, "function")
	targetFunctionSymbol := findSym(t, o, "target_function")
	assert.Equal(t, uint32(target.Value-base.Value), binary.LittleEndian.Uint32(o.Data[offset.Value:]))
	assert.Equal(t, uint32(targetFunctionSymbol.Value-functionSymbol.Value), binary.LittleEndian.Uint32(o.Data[textOffset.Value:]))
	assert.Contains(t, o.DataRelocs, obj.Reloc{
		Offset: address.Value, Sym: "function", Type: obj.R_AARCH64_ABS32,
	})
}

// findSym returns the named symbol, failing if the object has no such thing.
func findSym(t *testing.T, o *obj.Object, name string) obj.Sym {
	t.Helper()
	for _, s := range o.Syms {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no symbol %q in %v", name, o.Syms)
	return obj.Sym{}
}

func TestFuseArithShift(t *testing.T) {
	// A single-use constant shift feeding an add or subtract folds into the shifted-
	// register form, dropping the standalone shift. add fuses either operand; sub only
	// its subtrahend.
	build := func(name string, body func(f *ir.Func, e *ir.Block, a, b ir.Ref) ir.Ref) string {
		m := ir.NewModule()
		f := m.NewFunc(name, ir.ClsL).Export()
		a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
		e := f.Entry()
		e.Ret(body(f, e, a, b))
		return disasmModule(t, m)
	}

	add := build("f", func(f *ir.Func, e *ir.Block, a, b ir.Ref) ir.Ref {
		return e.Add(ir.ClsL, a, e.Shl(ir.ClsL, b, f.Long(3)))
	})
	assert.Contains(t, add, "add x0, x0, x1, lsl #3", "a + (b<<3) fuses")
	assert.NotContains(t, add, "lsl x", "the standalone shift is gone")

	sub := build("g", func(f *ir.Func, e *ir.Block, a, b ir.Ref) ir.Ref {
		return e.Sub(ir.ClsL, a, e.Sar(ir.ClsL, b, f.Long(2)))
	})
	assert.Contains(t, sub, "sub x0, x0, x1, asr #2", "a - (b>>2 arithmetic) fuses")
	assert.NotContains(t, sub, "asr x", "the standalone shift is gone")
}

func TestBlockLayoutFallThrough(t *testing.T) {
	// A conditional's fall-through-capable successor (the false edge of a JmpJnz) is
	// laid out immediately after it, so the branch to it is elided. Here `mid` sits
	// between the conditional and its false target in creation order; the layout must
	// pull the false target up to fall through, leaving only the taken branch to `hi`.
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	e := f.Entry()
	hi := f.NewBlock("hi")
	lo := f.NewBlock("lo")
	e.Jnz(e.Cmp(ir.CmpSgt, ir.ClsL, x, f.Long(10)), hi, lo)
	hi.Ret(f.Long(1))
	lo.Ret(f.Long(0))

	order := layoutBlocks(f)
	// Entry first, then its false target (lo) so the branch to it is elided, then hi.
	// Creation order is [entry, hi, lo]; the layout pulls lo up behind the conditional.
	require.Equal(t, []string{e.Name, "lo", "hi"}, blockNames(order),
		"the false edge is laid out to fall through")
}

func TestLayoutPullsUpEitherArm(t *testing.T) {
	// The matching maximizes fall-throughs across both arms of a conditional, not
	// just its false (To2) arm. Here block a's non-zero (To) arm, b, must be pulled up
	// to fall through -- forced because b then flows to c, so the chain entry-a-b-c
	// realizes three fall-throughs where following only To2 would strand b out of
	// line. The emitter inverts a's condition to branch to c instead (verified for
	// correctness by the interpreter differential).
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	e := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	c := f.NewBlock("c")
	e.Goto(a)
	a.Jnz(a.Cmp(ir.CmpSgt, ir.ClsL, x, f.Long(10)), b, c) // To=b (x>10), To2=c
	b.Goto(c)
	c.Ret(f.Long(0))

	require.Equal(t, []string{e.Name, "a", "b", "c"}, blockNames(layoutBlocks(f)),
		"a's non-zero arm b is pulled up to fall through")
}

func blockNames(bs []*ir.Block) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out
}

func TestCompileModulePropagatesError(t *testing.T) {
	// A parameterless caller cannot tail-call through stack arguments (it has no
	// incoming-argument area to reuse); the backend rejects it, and the error must
	// surface with the function name.
	m := ir.NewModule()
	f := m.NewFuncVoid("bad")
	e := f.Entry()
	var args []ir.Ref
	for i := 0; i < 10; i++ {
		args = append(args, f.Word(int64(i)))
	}
	e.TailCallVoid(f.Sym("sink", 0), args...)
	_, err := CompileToObject(m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad")
}

func TestStackAggregateCopyUsesLegalWidthForFourByteAlignedDestination(t *testing.T) {
	machine := &mc{prog: a64.NewProgram()}
	machine.emitStackAggregateCopy(mcSP, 268, mcGP2, 0, 260)

	_, err := machine.prog.Bytes()
	require.NoError(t, err)
}

// disasmModule compiles a module and reads its machine code back as assembly,
// which is how these tests check what was selected.
func disasmModule(t *testing.T, m *ir.Module) string {
	t.Helper()
	o, err := CompileToObject(m)
	require.NoError(t, err)
	return Disassemble(o)
}

// newMC builds a bare machine-code emitter for exercising one piece of it in
// isolation, without going through a whole compile.
func newMC(f *ir.Func) *mc {
	return &mc{f: f, prog: a64.NewProgram(), instrPC: map[*ir.Instr]uint64{},
		frameLayout: frameLayout{allocOff: map[*ir.Instr]int{}}}
}

// mcText reads back what the emitter produced, as assembly.
func mcText(t *testing.T, m *mc) string {
	t.Helper()
	code, err := m.prog.Bytes()
	require.NoError(t, err)
	return a64.DisasmBytes(code)
}

func TestParallelMoveRegisterSwap(t *testing.T) {
	m := newMC(ir.NewModule().NewFuncVoid("x"))
	m.parallelMove([]movePairLoc{
		{dst: loc{reg: X0, size: 8}, src: loc{reg: X1, size: 8}},
		{dst: loc{reg: X1, size: 8}, src: loc{reg: X0, size: 8}},
	})
	out := mcText(t, m)
	// Neither move can go first without destroying the other's source, so the
	// cycle is broken through the scratch register and costs a third move.
	assert.Contains(t, out, "x15")
	assert.Equal(t, 3, strings.Count(out, "mov "), out)
}

func TestParallelMoveNoOpsAndChain(t *testing.T) {
	m := newMC(ir.NewModule().NewFuncVoid("x"))
	m.parallelMove([]movePairLoc{
		{dst: loc{reg: X0, size: 8}, src: loc{reg: X0, size: 8}}, // no-op, dropped
		{dst: loc{reg: X2, size: 8}, src: loc{reg: X3, size: 8}},
	})
	assert.Equal(t, "mov x2, x3\n", mcText(t, m))
}

func TestEmitMoveLocCombos(t *testing.T) {
	cases := []struct {
		name string
		dst  loc
		src  loc
		want string
	}{
		{"reg<-reg", loc{reg: X0, size: 8}, loc{reg: X1, size: 8}, "mov x0, x1"},
		{"reg<-mem", loc{reg: X0, size: 8}, loc{mem: true, slot: 8, size: 8}, "ldr x0, [x29, #8]"},
		{"reg<-imm", loc{reg: X0, size: 4}, loc{imm: true, val: 7, size: 4}, "movz w0, #0x7"},
		{"mem<-reg", loc{mem: true, slot: 16, size: 8}, loc{reg: X2, size: 8}, "str x2, [x29, #16]"},
		{"mem<-imm", loc{mem: true, slot: 0, size: 4}, loc{imm: true, val: 3, size: 4}, "str w16, [x29]"},
		{"mem<-mem", loc{mem: true, slot: 24, size: 8}, loc{mem: true, slot: 8, size: 8}, "ldr x16, [x29, #8]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMC(ir.NewModule().NewFuncVoid("x"))
			m.emitMoveLoc(c.dst, c.src)
			assert.Contains(t, mcText(t, m), c.want)
		})
	}
}

func TestLocOf(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("x")

	reg := f.NewTemp("r", ir.ClsL)
	f.Temp(reg).Reg = int(X5)
	l, ok := locOf(f, reg)
	require.True(t, ok)
	assert.Equal(t, loc{reg: X5, size: 8}, l)

	sp := f.NewTemp("s", ir.ClsW)
	f.Temp(sp).Reg = ir.NoReg
	f.Temp(sp).Slot = 12
	l, ok = locOf(f, sp)
	require.True(t, ok)
	assert.Equal(t, loc{mem: true, slot: 12, size: 4}, l)

	l, ok = locOf(f, f.Word(9))
	require.True(t, ok)
	assert.Equal(t, loc{imm: true, val: 9, size: 4}, l)

	// A symbol's address is not somewhere a value already is: it has to be
	// materialized, so it has no location to report.
	_, ok = locOf(f, f.Sym("g", 0))
	assert.False(t, ok)
}

func TestEmitCallErrors(t *testing.T) {
	f := ir.NewModule().NewFunc("x", ir.ClsW)

	// A constant integer address is a legal indirect call target: it is
	// materialized and branched to (blr), so no error.
	m := newMC(f)
	(&sel{f: m.f, b: &mcAsm{prog: m.prog, m: m}}).call(&ir.Instr{Op: ir.OCall, Args: []ir.Ref{f.Word(5)}})
	require.NoError(t, m.err)
	assert.Contains(t, mcText(t, m), "blr ")

	// A slot reference cannot be a call target.
	m2 := newMC(f)
	(&sel{f: m2.f, b: &mcAsm{prog: m2.prog, m: m2}}).call(&ir.Instr{Op: ir.OCall, Args: []ir.Ref{{Kind: ir.RefSlot}}})
	require.Error(t, m2.err)
}

func TestEmitTermHltAndMissing(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("x")
	b := f.Entry()

	m := newMC(f)
	b.Hlt()
	m.term(b)
	assert.Contains(t, mcText(t, m), "brk #0")

	m2 := newMC(f)
	b.Jmp = ir.Jmp{Kind: ir.JmpNone}
	m2.term(b)
	require.Error(t, m2.err)
}

func TestSrcRegFailsOnUnsupportedRef(t *testing.T) {
	m := newMC(ir.NewModule().NewFuncVoid("x"))
	(&sel{f: m.f, b: &mcAsm{prog: m.prog, m: m}}).src(ir.Ref{Kind: ir.RefSlot}, 0, 8)
	require.Error(t, m.err)
}

func TestFailKeepsFirstError(t *testing.T) {
	m := &mc{}
	m.fail("first %d", 1)
	m.fail("second")
	require.EqualError(t, m.err, "first 1")
}

// movImm builds a value out of 16-bit chunks, and the point is that it skips the
// chunks it does not need: whichever of MOVZ (start from zero) or MOVN (start
// from all-ones) leaves less to fill.
func TestMovImmPatterns(t *testing.T) {
	m := newMC(ir.NewModule().NewFuncVoid("x"))
	m.movImm(a64.Reg(0), 0x1234, true)
	m.movImm(a64.Reg(1), -1, false)
	m.movImm(a64.Reg(2), 0xABCD0000, true)
	m.movImm(a64.Reg(3), 0x1234ABCD, true)
	out := mcText(t, m)
	assert.Contains(t, out, "movz x0, #0x1234")          // a single chunk
	assert.Contains(t, out, "movn w1, #0x0")             // -1 in one instruction
	assert.Contains(t, out, "movz x2, #0xabcd, lsl #16") // the zero low chunk is skipped
	assert.Contains(t, out, "movz x3, #0xabcd")          // 0x1234ABCD: low chunk
	assert.Contains(t, out, "movk x3, #0x1234, lsl #16") // ... then high chunk
}
