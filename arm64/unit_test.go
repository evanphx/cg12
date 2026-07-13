package arm64

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
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

func TestCondCode(t *testing.T) {
	want := map[ir.Cmp]string{
		ir.CmpEq: "eq", ir.CmpNe: "ne",
		ir.CmpSlt: "lt", ir.CmpSle: "le", ir.CmpSgt: "gt", ir.CmpSge: "ge",
		ir.CmpUlt: "lo", ir.CmpUle: "ls", ir.CmpUgt: "hi", ir.CmpUge: "hs",
	}
	for pred, code := range want {
		got, ok := condCode(pred)
		assert.Truef(t, ok, "%v", pred)
		assert.Equal(t, code, got)
	}
	_, ok := condCode(ir.CmpFeq) // float predicates unsupported here
	assert.False(t, ok)
}

func TestLoadStoreInfo(t *testing.T) {
	loads := []struct {
		op   ir.Op
		cls  ir.Cls
		mn   string
		size int
	}{
		{ir.OLoadub, ir.ClsW, "ldrb", 4},
		{ir.OLoaduh, ir.ClsW, "ldrh", 4},
		{ir.OLoaduw, ir.ClsL, "ldr", 4},
		{ir.OLoadsb, ir.ClsL, "ldrsb", 8},
		{ir.OLoadsh, ir.ClsW, "ldrsh", 4},
		{ir.OLoadsw, ir.ClsL, "ldrsw", 8},
		{ir.OLoadl, ir.ClsL, "ldr", 8},
	}
	for _, c := range loads {
		mn, sz := loadInfo(c.op, c.cls)
		assert.Equal(t, c.mn, mn, c.op.String())
		assert.Equal(t, c.size, sz, c.op.String())
	}
	stores := []struct {
		op   ir.Op
		mn   string
		size int
	}{
		{ir.OStoreb, "strb", 4},
		{ir.OStoreh, "strh", 4},
		{ir.OStorew, "str", 4},
		{ir.OStorel, "str", 8},
	}
	for _, c := range stores {
		mn, sz := storeInfo(c.op)
		assert.Equal(t, c.mn, mn, c.op.String())
		assert.Equal(t, c.size, sz, c.op.String())
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

func TestDataDirective(t *testing.T) {
	assert.Equal(t, ".byte", dataDirective(ir.SubB))
	assert.Equal(t, ".hword", dataDirective(ir.SubH))
	assert.Equal(t, ".word", dataDirective(ir.SubW))
	assert.Equal(t, ".quad", dataDirective(ir.SubL))
	assert.Equal(t, ".word", dataDirective(ir.SubS))
	assert.Equal(t, ".quad", dataDirective(ir.SubD))
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
	require.NoError(t, lower(f))
}

func TestLowerManyParamsUseStack(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	for i := 0; i < 10; i++ {
		f.Param("p", ir.ClsW)
	}
	f.Entry().RetVoid()
	require.NoError(t, lower(f))
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
	require.NoError(t, lower(f))
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
	require.NoError(t, lower(f), "6 int + 6 float args fit in x0..x5 and v0..v5")
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
	require.NoError(t, lower(f))
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

func TestSequentializeCyclicCopy(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("s")
	t1 := f.NewTemp("t1", ir.ClsW)
	t2 := f.NewTemp("t2", ir.ClsW)
	// A swap: t1<-t2, t2<-t1 — must break the cycle with a fresh temp.
	seq := sequentializeCopies(f, []movePair{{dst: t1, src: t2}, {dst: t2, src: t1}})
	assert.Len(t, seq, 3, "a 2-cycle needs one extra move")
	assert.Greater(t, len(f.Temps), 2, "a fresh temp was introduced")
}

func TestSplitCriticalEdges(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	mid := f.NewBlock("mid")
	join := f.NewBlock("join")
	start.Jnz(cond, mid, join) // start->join is critical (start:2 succ, join:2 pred)
	mid.Goto(join)
	r := join.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(1)}, ir.PhiEdge{From: mid, Val: f.Word(2)})
	join.Ret(r)

	before := len(f.Blocks)
	splitCriticalEdges(f)
	assert.Greater(t, len(f.Blocks), before, "a split block was inserted")
	// The start->join edge no longer targets join directly.
	assert.NotSame(t, join, start.Jmp.To2)
	assert.Same(t, join, start.Jmp.To2.Jmp.To, "the split block jumps to join")
}

func TestDestructSSARemovesPhis(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	join := f.NewBlock("join")
	start.Jnz(cond, a, b)
	a.Goto(join)
	b.Goto(join)
	join.Ret(join.Phi(ir.ClsW, ir.PhiEdge{From: a, Val: f.Word(1)}, ir.PhiEdge{From: b, Val: f.Word(2)}))

	splitCriticalEdges(f)
	destructSSA(f)
	for _, blk := range f.Blocks {
		assert.Empty(t, blk.Phis, "phis must be gone after destruction")
	}
}

func TestCompileFloatFunction(t *testing.T) {
	// A float add now compiles; the parameter comes in s0 and the result too.
	f := ir.NewModule().NewFunc("fadd", ir.ClsS).Export()
	a := f.Param("a", ir.ClsS)
	b := f.Param("b", ir.ClsS)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsS, a, b))

	asm, err := Compile(f)
	require.NoError(t, err)
	assert.Contains(t, asm, "fadd s")
}

func TestCompileLargeFrame(t *testing.T) {
	// A frame larger than the stp pre-index reach (504 bytes) must adjust sp
	// separately in the prologue and epilogue. A 2KB alloc lands in adjustSP's
	// immediate branch; a >4KB alloc that is not a clean multiple of 4096 lands
	// in the scratch-register (movImm) branch.
	compileAlloc := func(size int) string {
		f := ir.NewModule().NewFunc("bf", ir.ClsW).Export()
		x := f.Param("x", ir.ClsW)
		e := f.Entry()
		p := e.Alloc(4, size)
		e.Store(x, p)
		e.Ret(e.Load(ir.ClsW, p))
		asm, err := Compile(f)
		require.NoError(t, err)
		return asm
	}

	small := compileAlloc(2048) // frame ~2064, fits an immediate
	assert.Contains(t, small, "sub sp, sp, #")
	assert.Contains(t, small, "add sp, sp, #")
	assert.NotContains(t, small, "stp x29, x30, [sp, #-") // not the pre-index form

	huge := compileAlloc(9000) // frame > 4095 and not a multiple of 4096
	assert.Contains(t, huge, "sub sp, sp, "+scratch0.xName())
	assert.Contains(t, huge, "add sp, sp, "+scratch0.xName())
}

func TestCompileModuleWithData(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
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

	asm, err := CompileModule(m)
	require.NoError(t, err)
	assert.Contains(t, asm, ".globl g")
	assert.Contains(t, asm, ".balign 8")
	assert.Contains(t, asm, ".word 1")
	assert.Contains(t, asm, ".zero 4")
	assert.Contains(t, asm, `.ascii "hi"`)
	assert.Contains(t, asm, ".quad other+8")
}

func TestCompileModulePropagatesError(t *testing.T) {
	// A stack-passed aggregate argument is not yet supported; the error must
	// surface with the function name.
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	f := m.NewFuncVoid("bad")
	e := f.Entry()
	ptr := e.Alloc(8, 8)
	args := []ir.Ref{f.Word(0), f.Word(1), f.Word(2), f.Word(3), f.Word(4), f.Word(5), f.Word(6), f.Word(7), ptr}
	e.CallVoid(f.Sym("sink", 0), args...)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = make([]*ir.AggType, 9)
	call.AggArgs[8] = pair
	e.RetVoid()
	_, err := CompileModule(m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad")
}

func newEmitter(f *ir.Func, sb *strings.Builder) *emitter {
	return &emitter{f: f, sb: sb, allocOff: map[*ir.Instr]int{}, label: "t"}
}

func TestParallelMoveRegisterSwap(t *testing.T) {
	var sb strings.Builder
	e := newEmitter(ir.NewModule().NewFuncVoid("x"), &sb)
	e.parallelMove([]movePairLoc{
		{dst: loc{reg: X0, size: 8}, src: loc{reg: X1, size: 8}},
		{dst: loc{reg: X1, size: 8}, src: loc{reg: X0, size: 8}},
	})
	out := sb.String()
	// The cycle is broken through scratch2 (x15) and takes three moves.
	assert.Contains(t, out, "x15")
	assert.Equal(t, 3, strings.Count(out, "mov "))
}

func TestParallelMoveNoOpsAndChain(t *testing.T) {
	var sb strings.Builder
	e := newEmitter(ir.NewModule().NewFuncVoid("x"), &sb)
	e.parallelMove([]movePairLoc{
		{dst: loc{reg: X0, size: 8}, src: loc{reg: X0, size: 8}}, // no-op, dropped
		{dst: loc{reg: X2, size: 8}, src: loc{reg: X3, size: 8}},
	})
	out := sb.String()
	assert.Equal(t, "\tmov x2, x3\n", out)
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
		{"reg<-imm", loc{reg: X0, size: 4}, loc{imm: true, val: 7, size: 4}, "movz w0, #7"},
		{"mem<-reg", loc{mem: true, slot: 16, size: 8}, loc{reg: X2, size: 8}, "str x2, [x29, #16]"},
		{"mem<-imm", loc{mem: true, slot: 0, size: 4}, loc{imm: true, val: 3, size: 4}, "str w16, [x29, #0]"},
		{"mem<-mem", loc{mem: true, slot: 24, size: 8}, loc{mem: true, slot: 8, size: 8}, "ldr x16, [x29, #8]"},
	}
	for _, c := range cases {
		var sb strings.Builder
		e := newEmitter(ir.NewModule().NewFuncVoid("x"), &sb)
		e.emitMoveLoc(c.dst, c.src)
		assert.Containsf(t, sb.String(), c.want, c.name)
	}
}

func TestLocOf(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("x")
	var sb strings.Builder
	e := newEmitter(f, &sb)

	reg := f.NewTemp("r", ir.ClsL)
	f.Temp(reg).Reg = int(X5)
	assert.Equal(t, loc{reg: X5, size: 8}, e.locOf(reg))

	sp := f.NewTemp("s", ir.ClsW)
	f.Temp(sp).Reg = ir.NoReg
	f.Temp(sp).Slot = 12
	assert.Equal(t, loc{mem: true, slot: 12, size: 4}, e.locOf(sp))

	im := f.Word(9)
	assert.Equal(t, loc{imm: true, val: 9, size: 4}, e.locOf(im))

	// A symbol constant cannot be a move source: it must fail.
	e.locOf(f.Sym("g", 0))
	require.Error(t, e.err)
}

func TestEmitCallErrors(t *testing.T) {
	f := ir.NewModule().NewFunc("x", ir.ClsW)
	var sb strings.Builder

	e := newEmitter(f, &sb)
	e.emitCall(&ir.Instr{Op: ir.OCall, Args: []ir.Ref{f.Word(5)}}) // int const target
	require.Error(t, e.err)

	e2 := newEmitter(f, &sb)
	e2.emitCall(&ir.Instr{Op: ir.OCall, Args: []ir.Ref{{Kind: ir.RefSlot}}})
	require.Error(t, e2.err)
}

func TestEmitTermHltAndMissing(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("x")
	b := f.Entry()

	var sb strings.Builder
	e := newEmitter(f, &sb)
	b.Hlt()
	e.emitTerm(b)
	assert.Contains(t, sb.String(), "brk #0")

	var sb2 strings.Builder
	e2 := newEmitter(f, &sb2)
	b.Jmp = ir.Jmp{Kind: ir.JmpNone}
	e2.emitTerm(b)
	require.Error(t, e2.err)
}

func TestSrcRegFailsOnUnsupportedRef(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("x")
	var sb strings.Builder
	e := newEmitter(f, &sb)
	e.srcReg(ir.Ref{Kind: ir.RefSlot}, 0, 8)
	require.Error(t, e.err)
}

func TestFailKeepsFirstError(t *testing.T) {
	e := &emitter{}
	e.fail("first %d", 1)
	e.fail("second")
	require.EqualError(t, e.err, "first 1")
}

func TestMovImmPatterns(t *testing.T) {
	var sb strings.Builder
	e := &emitter{f: ir.NewModule().NewFuncVoid("x"), sb: &sb}
	e.movImm(X0, 0x1234, 8)
	e.movImm(X1, -1, 4)
	e.movImm(X2, 0xABCD0000, 8)
	out := sb.String()
	assert.Contains(t, out, "movz x0, #4660") // 0x1234
	assert.Contains(t, out, "movz w1, #65535")
	assert.Contains(t, out, "movk x2, #43981, lsl #16") // 0xABCD << 16
}
