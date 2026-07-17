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
	m := ir.NewModule()
	f := m.NewFunc("fadd", ir.ClsS).Export()
	a := f.Param("a", ir.ClsS)
	b := f.Param("b", ir.ClsS)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsS, a, b))

	assert.Contains(t, disasmModule(t, m), "fadd s")
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
