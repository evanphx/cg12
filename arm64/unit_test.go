package arm64

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64/a64"
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
	require.NoError(t, lower(f, TLSLocalExec))
}

func TestLowerManyParamsUseStack(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	for i := 0; i < 10; i++ {
		f.Param("p", ir.ClsW)
	}
	f.Entry().RetVoid()
	require.NoError(t, lower(f, TLSLocalExec))
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
	require.NoError(t, lower(f, TLSLocalExec))
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
	require.NoError(t, lower(f, TLSLocalExec), "6 int + 6 float args fit in x0..x5 and v0..v5")
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
	require.NoError(t, lower(f, TLSLocalExec))
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
	f.GoABI = true
	entry := f.Entry()
	value := entry.Alloc(8, 8)
	pointerSlot := entry.Alloc(8, 8)
	entry.Store(value, pointerSlot)
	entry.Call(ir.ClsL, f.Sym("observe", 0), value)
	entry.Ret(f.Word(0))

	assert.Contains(t, disasmModule(t, module), "str xzr, [x29")
}

func TestGoABISpillsPointerLiveAcrossCall(t *testing.T) {
	module := ir.NewModule()
	f := module.NewFunc("pointer_across_call", ir.ClsP)
	f.GoABI = true
	pointer := f.ParamRef("pointer")
	entry := f.Entry()
	entry.CallVoid(f.Sym("observe", 0))
	entry.Ret(pointer)

	assembly := disasmModule(t, module)
	assert.Equal(t, ir.NoReg, f.Temps[pointer.ID].Reg)
	assert.Contains(t, assembly, "str xzr, [x29")
}

func TestGoABIReportsPointerResultSlotLiveAcrossCall(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("multi_result", ir.ClsP)
	function.GoABI = true
	function.Param("size", ir.ClsL)
	result := function.ParamRef("result1")
	entry := function.Entry()
	entry.CallVoid(function.Sym("observe", 0))
	entry.Store(function.Long(8), result)
	entry.Ret(function.ConstInt(ir.ClsP, 0))

	prepareGoABI(function)
	ir.LowerPointers(function, ptrCls)
	require.NoError(t, lower(function, TLSLocalExec))
	allocation, err := regAlloc(function)
	require.NoError(t, err)
	frame := computeFrame(function, allocation)
	words := goPointerWordIndexes(function, frame.allocOff, frame.spillBase)
	assert.Contains(t, words, 0)
}

func TestGoPointerFrameOffsetsIgnoreUnassignedSpillSlots(t *testing.T) {
	f := ir.NewModule().NewFunc("pointer_slots", ir.ClsW)
	f.GoABI = true
	unassigned := f.NewTemp("unassigned", ir.ClsP)
	f.Temps[unassigned.ID].GCRef = true
	spilled := f.NewTemp("spilled", ir.ClsP)
	f.Temps[spilled.ID].GCRef = true
	f.Temps[spilled.ID].Slot = 8

	assert.Equal(t, []int{40}, goPointerFrameOffsets(f, nil, 32))
}

func TestGoAssemblyFunctionInfoOnlyMarksRealTopFrames(t *testing.T) {
	functions := goAssemblyFunctionInfo()
	topFrames := make([]string, 0, 2)
	var morestackRestore *goFunctionInfo
	for _, function := range functions {
		if function.funcFlag&1 != 0 {
			topFrames = append(topFrames, function.name)
		}
		if function.name == "runtime_morestack_restore" {
			function := function
			morestackRestore = &function
		}
	}

	assert.Equal(t, []string{"runtime_mstart", "runtime_goexit", "runtime_asmcgocall"}, topFrames)
	for _, function := range functions {
		assert.NotEqual(t, "runtime_morestack_restore_end", function.name)
	}
	require.NotNil(t, morestackRestore)
	assert.Equal(t, 320, morestackRestore.frameSize)
	assert.Equal(t, []int{25, 26, 27, 28, 29, 30, 31, 32, 33, 34}, morestackRestore.pointerWords)
}

func TestGoRegisterPointerMaskTracksABIRegisters(t *testing.T) {
	function := ir.NewModule().NewFunc("pointer_args", ir.ClsW)
	function.Param("integer", ir.ClsL)
	function.Param("floating", ir.ClsD)
	function.ParamRef("firstPointer")
	function.Param("word", ir.ClsW)
	function.ParamRef("secondPointer")

	assert.Equal(t, uint8(0b1010), goRegisterPointerMask(function))
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
