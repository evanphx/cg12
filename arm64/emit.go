package arm64

import (
	"fmt"
	"math"
	"strings"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
)

// emitter turns a lowered, register-allocated function into GNU-assembler text.
type emitter struct {
	f     *ir.Func
	sb    *strings.Builder
	alloc *allocation

	frame       int               // total frame size in bytes (16-aligned)
	calleeSaved []Reg             // callee-saved registers to preserve
	spillBase   int               // byte offset of the spill area within the frame
	allocOff    map[*ir.Instr]int // OAlloc instruction -> frame offset
	hasDynAlloc bool              // the function has a VLA (dynamic) alloca
	label       string            // sanitized function label
	err         error             // first emission error, if any
	blockDone   bool              // the current block emitted its own terminator (a tail branch)
	lastPos     ir.SrcPos         // last source position emitted as a .loc directive

	// Variadic support: the register save area and how many registers the named
	// parameters consumed.
	variadic   bool
	gpSaveOff  int // frame offset of the x0..x7 save area
	fpSaveOff  int // frame offset of the v0..v7 save area
	namedGr    int // GP registers used by named parameters
	namedSr    int // SIMD registers used by named parameters
	namedStack int // bytes of stack used by named parameters
	vaSeq      int // counter for unique vaarg labels
}

// location kinds for parallel moves.
type loc struct {
	reg  Reg  // valid when !mem && !imm
	mem  bool // spilled: use slot offset
	slot int
	imm  bool // immediate constant
	val  int64
	size int
}

func emitFunc(f *ir.Func, alloc *allocation, sb *strings.Builder) error {
	e := &emitter{f: f, sb: sb, alloc: alloc, allocOff: map[*ir.Instr]int{}, label: sanitize(f.Name)}
	e.planFrame()
	e.prologueHeader()
	for _, b := range f.Blocks {
		e.emitBlock(b)
	}
	if e.err != nil {
		return e.err
	}
	fmt.Fprintf(sb, "\t.size %s, .-%s\n", e.label, e.label)
	return nil
}

// err is stashed on the emitter so per-instruction helpers can fail out.
func (e *emitter) fail(format string, a ...any) {
	if e.err == nil {
		e.err = fmt.Errorf(format, a...)
	}
}

func (e *emitter) line(format string, a ...any) {
	fmt.Fprintf(e.sb, "\t"+format+"\n", a...)
}

// planFrame decides callee-saved registers to preserve, the spill area, and any
// stack allocations, then computes the (16-aligned) frame size.
func (e *emitter) planFrame() {
	used := map[Reg]bool{}
	for _, t := range e.f.Temps {
		if t.Reg != ir.NoReg {
			r := Reg(t.Reg)
			if calleeSavedReg(r) {
				used[r] = true
			}
		}
	}
	for _, r := range intAllocOrder {
		if used[r] {
			e.calleeSaved = append(e.calleeSaved, r)
		}
	}
	for _, r := range floatAllocOrder {
		if used[r] {
			e.calleeSaved = append(e.calleeSaved, r)
		}
	}

	off := 16 + 8*len(e.calleeSaved) // x29/x30 occupy [0,16)
	e.spillBase = off
	off += e.alloc.spillBytes

	// Reserve stack for each OAlloc, honouring its alignment.
	for _, b := range e.f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() {
				align, size := allocShape(e.f, in)
				off = roundUp(off, align)
				e.allocOff[in] = off
				off += size
			}
			if in.Op == ir.OAllocN {
				e.hasDynAlloc = true
			}
		}
	}

	if e.f.Variadic {
		e.variadic = true
		e.namedGr, e.namedSr, e.namedStack = computeNamedCounts(e.f)
		off = roundUp(off, 8)
		e.gpSaveOff = off
		off += 8 * 8 // x0..x7
		off = roundUp(off, 16)
		e.fpSaveOff = off
		off += 8 * 16 // v0..v7 (16-byte stride)
	}

	e.frame = roundUp(off, 16)
}

func (e *emitter) prologueHeader() {
	fmt.Fprintf(e.sb, "\t.text\n")
	if e.f.Linkage.Export {
		fmt.Fprintf(e.sb, "\t.globl %s\n", e.label)
	}
	fmt.Fprintf(e.sb, "\t.type %s, @function\n", e.label)
	fmt.Fprintf(e.sb, "%s:\n", e.label)
	e.allocFrame()
	for i, r := range e.calleeSaved {
		e.line("str %s, [sp, #%d]", r.xName(), 16+i*8)
	}
	if e.variadic {
		// Spill all argument registers so vaarg can walk them. This must precede
		// the parameter parallel move, which may overwrite them.
		for i := 0; i < 8; i++ {
			e.line("str %s, [x29, #%d]", Reg(int(X0)+i).xName(), e.gpSaveOff+i*8)
		}
		for i := 0; i < 8; i++ {
			e.line("str %s, [x29, #%d]", vReg(i).xName(), e.fpSaveOff+i*16)
		}
	}
}

// allocFrame lowers the stack pointer by the frame size and saves x29/x30.
// stp's pre-index immediate only reaches -512, so larger frames subtract sp
// separately (materialising the amount in a scratch register when it exceeds an
// add/sub immediate).
func (e *emitter) allocFrame() {
	if e.frame <= 504 {
		e.line("stp x29, x30, [sp, #-%d]!", e.frame)
		e.line("mov x29, sp")
		return
	}
	e.adjustSP("sub", e.frame)
	e.line("stp x29, x30, [sp, #0]")
	e.line("mov x29, sp")
}

// frameTeardown restores callee-saved registers and the frame pointer and pops
// the frame, leaving lr set to the caller's return address but not returning.
func (e *emitter) frameTeardown() {
	if e.hasDynAlloc {
		e.line("mov sp, x29") // undo any VLA growth before the sp-relative reloads
	}
	for i, r := range e.calleeSaved {
		e.line("ldr %s, [sp, #%d]", r.xName(), 16+i*8)
	}
	if e.frame <= 504 {
		e.line("ldp x29, x30, [sp], #%d", e.frame)
	} else {
		e.line("ldp x29, x30, [sp, #0]")
		e.adjustSP("add", e.frame)
	}
}

func (e *emitter) epilogue() {
	e.frameTeardown()
	e.line("ret")
}

// emitTailBranch emits a mandatory tail call: after the arguments are in place,
// the frame is torn down and control branches to the callee, which returns
// directly to our caller. A computed target is moved to a scratch register that
// survives the teardown.
func (e *emitter) emitTailBranch(in *ir.Instr) {
	callee := in.Args[0]
	switch callee.Kind {
	case ir.RefConst:
		c := e.f.Consts[callee.ID]
		if c.Kind != ir.ConstSym {
			e.fail("arm64: tail call target must be a symbol or register")
			return
		}
		e.frameTeardown()
		e.line("b %s", sanitize(c.Sym))
	case ir.RefTemp:
		r := e.srcReg(callee, 0, 8)
		e.line("mov %s, %s", scratch1.xName(), r) // survive the teardown
		e.frameTeardown()
		e.line("br %s", scratch1.xName())
	default:
		e.fail("arm64: invalid tail call target")
	}
}

// adjustSP emits "sub"/"add sp, sp, #n", using a scratch register when n exceeds
// the 12-bit (optionally shifted) immediate range.
func (e *emitter) adjustSP(op string, n int) {
	switch {
	case n <= 4095:
		e.line("%s sp, sp, #%d", op, n)
	case n%4096 == 0 && n>>12 <= 4095:
		e.line("%s sp, sp, #%d, lsl #12", op, n>>12)
	default:
		e.movImm(scratch0, int64(n), 8)
		e.line("%s sp, sp, %s", op, scratch0.xName())
	}
}

func (e *emitter) blockLabel(b *ir.Block) string {
	return fmt.Sprintf(".L%s_%s", e.label, sanitize(b.Name))
}

func (e *emitter) emitBlock(b *ir.Block) {
	if b.Sym != "" { // object symbol for an address-taken block (&&label in data)
		fmt.Fprintf(e.sb, "%s:\n", sanitize(b.Sym))
	}
	fmt.Fprintf(e.sb, "%s:\n", e.blockLabel(b))
	i := 0
	// Entry parameter materialisation: register params are a parallel move out
	// of x0..xN/v0..vN; stacked params are loaded from the incoming area.
	if b == e.f.Start {
		var pairs []movePairLoc
		var stackParams []*ir.Instr
		for i < len(b.Instrs) && b.Instrs[i].Op == ir.OPar {
			in := &b.Instrs[i]
			if len(in.Args) == 0 {
				stackParams = append(stackParams, in)
			} else {
				pairs = append(pairs, movePairLoc{dst: e.locOf(in.To), src: e.locOf(in.Args[0])})
			}
			i++
		}
		// Move register params first so the incoming argument registers are
		// consumed before a stacked-param load can overwrite one of them.
		e.parallelMove(pairs)
		for _, in := range stackParams {
			e.emitStackParam(in)
		}
	}
	e.blockDone = false
	for i < len(b.Instrs) {
		in := &b.Instrs[i]
		e.emitLoc(in.Pos)
		if in.Op == ir.OArg {
			i = e.emitCallSequence(b, i)
			continue
		}
		e.emitInstr(b, in)
		i++
	}
	if !e.blockDone { // a tail call emits its own terminator (b/br)
		e.emitTerm(b)
	}
}

// emitStackParam loads a stacked parameter from the incoming argument area,
// which sits just above this frame (hence frame + offset from the frame pointer).
func (e *emitter) emitStackParam(in *ir.Instr) {
	off := e.frame + int(in.Aux)
	t := e.f.Temps[in.To.ID]
	if t.Agg != nil {
		// A stacked aggregate parameter is the address of the incoming bytes.
		if t.Reg != ir.NoReg {
			e.frameAddr(Reg(t.Reg).xName(), off)
			return
		}
		e.frameAddr(scratch0.xName(), off)
		e.line("str %s, [x29, #%d]", scratch0.xName(), e.spillBase+t.Slot)
		return
	}
	sz := in.Cls.Size()
	if t.Reg != ir.NoReg {
		e.line("ldr %s, [x29, #%d]", Reg(t.Reg).Name(sz), off)
		return
	}
	sc := scratch0
	if in.Cls.IsFloat() {
		sc = fscratch0
	}
	e.line("ldr %s, [x29, #%d]", sc.Name(sz), off)
	e.line("str %s, [x29, #%d]", sc.Name(sz), e.spillBase+t.Slot)
}

// emitCallSequence emits an argument run and the call that consumes it, starting
// at the OArg at index i, and returns the index just past the call. Register
// arguments become a parallel move; stacked arguments are stored into a
// temporary outgoing area carved out of the stack around the call.
func (e *emitter) emitCallSequence(b *ir.Block, i int) int {
	var regPairs []movePairLoc
	var stackArgs, symArgs []*ir.Instr
	for i < len(b.Instrs) && b.Instrs[i].Op == ir.OArg {
		a := &b.Instrs[i]
		switch {
		case a.To.IsNone():
			stackArgs = append(stackArgs, a)
		case e.isConstSym(a.Args[0]):
			// A symbol address needs adrp/add; it can't go through the register
			// parallel move, but it depends on nothing, so materialise it after.
			symArgs = append(symArgs, a)
		default:
			regPairs = append(regPairs, movePairLoc{dst: e.locOf(a.To), src: e.locOf(a.Args[0])})
		}
		i++
	}
	call := &b.Instrs[i]
	e.emitLoc(call.Pos) // the OArg run carries no position; the call does

	if call.Tail {
		// Stacked arguments go into our own incoming-argument area — the space at
		// [x29, #frame..] that sp points at once the frame is torn down — rather
		// than below sp. Write them (and the register args) before the branch.
		for _, a := range stackArgs {
			r := e.srcReg(a.Args[0], 0, a.Cls.Size())
			e.line("str %s, [x29, #%d]", r, e.frame+int(a.Aux))
		}
		e.parallelMove(regPairs)
		for _, a := range symArgs {
			e.materializeSym(Reg(e.f.Temps[a.To.ID].Reg), e.f.Consts[a.Args[0].ID])
		}
		e.emitTailBranch(call)
		e.blockDone = true
		return i + 1
	}

	stackBytes := int(call.Aux)
	if stackBytes > 0 {
		e.line("sub sp, sp, #%d", stackBytes)
	}
	// Stacked arguments first, before the parallel move reuses a source register.
	for _, a := range stackArgs {
		sz := a.Cls.Size()
		r := e.srcReg(a.Args[0], 0, sz)
		e.line("str %s, [sp, #%d]", r, int(a.Aux))
	}
	e.parallelMove(regPairs)
	for _, a := range symArgs {
		e.materializeSym(Reg(e.f.Temps[a.To.ID].Reg), e.f.Consts[a.Args[0].ID])
	}
	e.emitCall(call)
	if stackBytes > 0 {
		e.line("add sp, sp, #%d", stackBytes)
	}
	return i + 1 // consume the call
}

// emitLoc emits a .loc directive when the source position changes; the assembler
// turns the resulting .file/.loc stream into a DWARF line-number program mapping
// machine code back to source lines and columns.
func (e *emitter) emitLoc(pos ir.SrcPos) {
	if !pos.Valid() || pos == e.lastPos {
		return
	}
	e.lastPos = pos
	e.line(".loc %d %d %d", pos.File, pos.Line, pos.Col)
}

func (e *emitter) emitTerm(b *ir.Block) {
	switch b.Jmp.Kind {
	case ir.JmpRet:
		e.epilogue()
	case ir.JmpJmp:
		e.line("b %s", e.blockLabel(b.Jmp.To))
	case ir.JmpJnz:
		r := e.srcReg(b.Jmp.Arg, 0, e.f.ClassOf(b.Jmp.Arg).Size())
		e.line("cbnz %s, %s", r, e.blockLabel(b.Jmp.To))
		e.line("b %s", e.blockLabel(b.Jmp.To2))
	case ir.JmpHlt:
		e.line("brk #0")
	case ir.JmpBr:
		r := e.srcReg(b.Jmp.Arg, 0, 8)
		e.line("br %s", r) // computed goto
	case ir.JmpTable:
		// Indexed branch through a PC-relative offset table (see the machine-code
		// emitter). The assembler computes each entry as (case - table).
		idx := e.srcReg(b.Jmp.Arg, 0, 4)
		tbl := e.blockLabel(b) + "_tbl"
		e.line("adr x17, %s", tbl)
		e.line("ldrsw x15, [x17, %s, uxtw #2]", idx)
		e.line("add x15, x17, x15")
		e.line("br x15")
		e.line("%s:", tbl)
		for _, t := range b.Jmp.Targets {
			e.line(".word %s - %s", e.blockLabel(t), tbl)
		}
	default:
		e.fail("arm64: block %q has no terminator", b.Name)
	}
}

func (e *emitter) emitInstr(b *ir.Block, in *ir.Instr) {
	// Integer data-processing instructions are selected once, through the shared
	// builder (here backed by the assembly-text renderer).
	if (&sel{f: e.f, b: &textAsm{e: e}, spillBase: e.spillBase}).selectData(in) {
		return
	}
	sz := in.Cls.Size()
	switch in.Op {
	case ir.ONop:
		return
	case ir.OAdd:
		if in.Cls.IsFloat() {
			e.binop("fadd", in, sz)
		} else {
			e.addSubImm(in, false, sz)
		}
	case ir.OSub:
		if in.Cls.IsFloat() {
			e.binop("fsub", in, sz)
		} else {
			e.addSubImm(in, true, sz)
		}
	case ir.OMul:
		e.binop(fltMn(in.Cls, "mul", "fmul"), in, sz)
	case ir.ODiv:
		e.binop(fltMn(in.Cls, "sdiv", "fdiv"), in, sz)
	case ir.OUDiv:
		e.binop("udiv", in, sz)
	case ir.OAnd:
		e.logicalImm(in, "and", sz)
	case ir.OOr:
		e.logicalImm(in, "orr", sz)
	case ir.OXor:
		if !e.tryMvn(in, sz) {
			e.logicalImm(in, "eor", sz)
		}
	case ir.OBic:
		e.binop("bic", in, sz)
	case ir.OClz:
		s := e.srcReg(in.Args[0], 1, sz)
		d, done := e.dstReg(in.To, sz)
		e.line("clz %s, %s", d, s)
		done()
	case ir.OShl:
		e.shiftImm(in, "lsl", sz)
	case ir.OShr:
		e.shiftImm(in, "lsr", sz)
	case ir.OSar:
		e.shiftImm(in, "asr", sz)
	case ir.ORotr:
		e.rotrImm(in, sz)
	case ir.ORem:
		e.remop("sdiv", in, sz)
	case ir.OURem:
		e.remop("udiv", in, sz)
	case ir.ONeg:
		s := e.srcReg(in.Args[0], 1, sz)
		d, done := e.dstReg(in.To, sz)
		e.line("%s %s, %s", fltMn(in.Cls, "neg", "fneg"), d, s)
		done()
	case ir.OCopy, ir.OPar, ir.OArg:
		e.emitCopy(in, sz)
	case ir.OCmp:
		e.emitCmp(in)
	case ir.OExtsb:
		e.emitConv("sxtb", in)
	case ir.OExtub:
		e.emitConv("uxtb", in)
	case ir.OExtsh:
		e.emitConv("sxth", in)
	case ir.OExtuh:
		e.emitConv("uxth", in)
	case ir.OExtsw:
		e.emitConv("sxtw", in)
	case ir.OExtuw:
		// Writing a W register zero-extends into its X register.
		e.emitConvSz("mov", in, 4, 4)
	case ir.OExts:
		e.emitConv("fcvt", in)
	case ir.OTruncd:
		e.emitConv("fcvt", in)
	case ir.OStosi:
		e.emitConv("fcvtzs", in)
	case ir.OStoui:
		e.emitConv("fcvtzu", in)
	case ir.OSltof:
		e.emitConv("scvtf", in)
	case ir.OUltof:
		e.emitConv("ucvtf", in)
	case ir.OCast:
		e.emitConv("fmov", in)
	case ir.OCall:
		if in.Tail { // an argument-less tail call reaches here directly
			e.emitTailBranch(in)
			e.blockDone = true
		} else {
			e.emitCall(in)
		}
	case ir.OVaStart:
		e.emitVaStart(in)
	case ir.OVaArg:
		e.emitVaArg(in)
	case ir.OAlloc4, ir.OAlloc8, ir.OAlloc16:
		d, done := e.dstReg(in.To, 8)
		e.frameAddr(d, e.allocOff[in])
		done()
	case ir.OAllocN:
		size := e.srcReg(in.Args[0], 1, 8)
		d, done := e.dstReg(in.To, 8)
		e.line("mov x15, sp")
		e.line("sub %s, x15, %s", d, size)
		e.line("mov sp, %s", d)
		done()
	case ir.OStackSave:
		d, done := e.dstReg(in.To, 8)
		e.line("mov %s, sp", d)
		done()
	case ir.OStackRestore:
		e.line("mov sp, %s", e.srcReg(in.Args[0], 0, 8))
	case ir.OBlockAddr:
		d, done := e.dstReg(in.To, 8)
		e.line("adr %s, %s", d, e.blockLabel(in.Blk)) // &&label
		done()
	case ir.OAsm:
		e.emitAsm(in)
	default:
		if in.Op.IsLoad() {
			e.emitLoad(in)
		} else if in.Op.IsStore() {
			e.emitStore(in)
		} else {
			e.fail("arm64: unsupported op %q", in.Op)
		}
	}
}

// frameAddr emits reg = x29 + off, splitting a large offset into a shifted-high
// and low add since the ADD immediate is only 12 bits — otherwise a frame offset
// above 4095 wraps and aliases another slot.
func (e *emitter) frameAddr(reg string, off int) {
	hi, lo := off>>12, off&0xfff
	if hi > 0 {
		e.line("add %s, x29, #%d, lsl #12", reg, hi)
		if lo > 0 {
			e.line("add %s, %s, #%d", reg, reg, lo)
		}
		return
	}
	e.line("add %s, x29, #%d", reg, lo)
}

// binop emits a three-operand instruction, loading spilled/immediate operands
// into scratch registers and storing a spilled result.
func (e *emitter) binop(mn string, in *ir.Instr, sz int) {
	s1 := e.srcReg(in.Args[0], 0, sz)
	s2 := e.srcReg(in.Args[1], 1, sz)
	d, done := e.dstReg(in.To, sz)
	e.line("%s %s, %s, %s", mn, d, s1, s2)
	done()
}

// shiftImm emits a shift, using the immediate form for a constant amount.
func (e *emitter) shiftImm(in *ir.Instr, mn string, sz int) {
	if v, ok := intConst(e.f, in.Args[1]); ok {
		if sh, ok := shiftImm(v, sz); ok {
			s1 := e.srcReg(in.Args[0], 0, sz)
			d, done := e.dstReg(in.To, sz)
			e.line("%s %s, %s, #%d", mn, d, s1, sh)
			done()
			return
		}
	}
	e.binop(mn, in, sz)
}

// addSubImm emits an add/sub, folding a constant operand into a 12-bit
// immediate and flipping add<->sub for a negative constant.
func (e *emitter) addSubImm(in *ir.Instr, sub bool, sz int) {
	aRef, bRef := in.Args[0], in.Args[1]
	if !sub {
		if _, ok := intConst(e.f, bRef); !ok {
			if _, ok := intConst(e.f, aRef); ok {
				aRef, bRef = bRef, aRef
			}
		}
	}
	if v, ok := intConst(e.f, bRef); ok {
		if imm, lsl12, flip, ok := addSubImm(v); ok {
			s1 := e.srcReg(aRef, 0, sz)
			d, done := e.dstReg(in.To, sz)
			mn := "add"
			if sub != flip {
				mn = "sub"
			}
			if lsl12 {
				e.line("%s %s, %s, #%d, lsl #12", mn, d, s1, imm)
			} else {
				e.line("%s %s, %s, #%d", mn, d, s1, imm)
			}
			done()
			return
		}
	}
	e.binop(map[bool]string{false: "add", true: "sub"}[sub], in, sz)
}

// logicalImm emits a bitwise op, folding a constant operand into a logical
// (bitmask) immediate when it encodes as one.
func (e *emitter) logicalImm(in *ir.Instr, mn string, sz int) {
	aRef, bRef := in.Args[0], in.Args[1]
	if _, ok := intConst(e.f, bRef); !ok {
		if _, ok := intConst(e.f, aRef); ok {
			aRef, bRef = bRef, aRef
		}
	}
	if v, ok := intConst(e.f, bRef); ok {
		val := uint64(v)
		if sz == 4 {
			val &= 0xffffffff
		}
		if _, _, _, ok := a64.EncodeBitmask(val, sz); ok {
			s1 := e.srcReg(aRef, 0, sz)
			d, done := e.dstReg(in.To, sz)
			e.line("%s %s, %s, #%d", mn, d, s1, int64(val))
			done()
			return
		}
	}
	e.binop(mn, in, sz)
}

// tryMvn emits a bitwise NOT (MVN) when in is xor(x, all-ones).
func (e *emitter) tryMvn(in *ir.Instr, sz int) bool {
	var x ir.Ref
	switch {
	case allOnes(e.f, in.Cls, in.Args[1]):
		x = in.Args[0]
	case allOnes(e.f, in.Cls, in.Args[0]):
		x = in.Args[1]
	default:
		return false
	}
	s := e.srcReg(x, 0, sz)
	d, done := e.dstReg(in.To, sz)
	e.line("mvn %s, %s", d, s)
	done()
	return true
}

// rotrImm emits a rotate-right by a constant amount (ORotr).
func (e *emitter) rotrImm(in *ir.Instr, sz int) {
	v, _ := intConst(e.f, in.Args[1])
	s := e.srcReg(in.Args[0], 0, sz)
	d, done := e.dstReg(in.To, sz)
	e.line("ror %s, %s, #%d", d, s, v)
	done()
}

// remop computes a remainder as a - (a/b)*b using div + msub (integer only).
func (e *emitter) remop(divmn string, in *ir.Instr, sz int) {
	a := e.srcReg(in.Args[0], 0, sz)
	b := e.srcReg(in.Args[1], 1, sz)
	q := scratch2.Name(sz)
	e.line("%s %s, %s, %s", divmn, q, a, b)
	d, done := e.dstReg(in.To, sz)
	e.line("msub %s, %s, %s, %s", d, q, b, a)
	done()
}

// emitConv emits a one-operand conversion where the operand and result may be in
// different register classes and widths.
func (e *emitter) emitConv(mn string, in *ir.Instr) {
	e.emitConvSz(mn, in, e.f.ClassOf(in.Args[0]).Size(), in.Cls.Size())
}

// emitConvSz is emitConv with explicit operand and result register widths, for
// the cases where they differ from the values' natural class sizes.
func (e *emitter) emitConvSz(mn string, in *ir.Instr, srcSz, dstSz int) {
	s := e.srcReg(in.Args[0], 1, srcSz)
	d, done := e.dstReg(in.To, dstSz)
	e.line("%s %s, %s", mn, d, s)
	done()
}

func (e *emitter) emitCopy(in *ir.Instr, sz int) {
	s := e.srcReg(in.Args[0], 0, sz)
	d, done := e.dstReg(in.To, sz)
	if d != s {
		if in.Cls == ir.ClsQ {
			e.line("mov %s, %s", vec16(d), vec16(s)) // full 128-bit copy
		} else {
			e.line("%s %s, %s", fltMn(in.Cls, "mov", "fmov"), d, s)
		}
	}
	done()
}

// vec16 turns a Q register name ("q5") into its .16B vector form ("v5.16b") for
// a full-width SIMD move.
func vec16(qName string) string {
	if len(qName) >= 2 && qName[0] == 'q' {
		return "v" + qName[1:] + ".16b"
	}
	return qName
}

func (e *emitter) emitCmp(in *ir.Instr) {
	argCls := e.f.ClassOf(in.Args[0])
	sz := argCls.Size()

	var cond string
	var ok bool
	if argCls.IsFloat() {
		s1 := e.srcReg(in.Args[0], 0, sz)
		s2 := e.srcReg(in.Args[1], 1, sz)
		e.line("fcmp %s, %s", s1, s2)
		cond, ok = fpCondCode(in.Cmp)
	} else {
		s1 := e.srcReg(in.Args[0], 0, sz)
		folded := false
		if v, vok := intConst(e.f, in.Args[1]); vok {
			if imm, lsl12, flip, iok := addSubImm(v); iok && !lsl12 && !flip {
				e.line("cmp %s, #%d", s1, imm)
				folded = true
			}
		}
		if !folded {
			s2 := e.srcReg(in.Args[1], 1, sz)
			e.line("cmp %s, %s", s1, s2)
		}
		cond, ok = condCode(in.Cmp)
	}
	if !ok {
		e.fail("arm64: unsupported comparison predicate %v", in.Cmp)
		return
	}
	d, done := e.dstReg(in.To, in.Cls.Size())
	e.line("cset %s, %s", d, cond)
	done()
}

// fltMn selects the integer or floating-point mnemonic for a class.
func fltMn(cls ir.Cls, intMn, floatMn string) string {
	if cls.IsFloat() {
		return floatMn
	}
	return intMn
}

func (e *emitter) emitCall(in *ir.Instr) {
	callee := in.Args[0]
	switch callee.Kind {
	case ir.RefConst:
		if c := e.f.Consts[callee.ID]; c.Kind == ir.ConstSym {
			e.line("bl %s", sanitize(c.Sym))
			break
		}
		// A call through a constant address (e.g. a function pointer the
		// optimizer folded to a literal): materialize it and branch indirectly.
		e.line("blr %s", e.srcReg(callee, 0, 8))
	case ir.RefTemp:
		r := e.srcReg(callee, 0, 8)
		e.line("blr %s", r)
	default:
		e.fail("arm64: invalid call target")
	}
}

func (e *emitter) emitLoad(in *ir.Instr) {
	mn, sz := loadInfo(in.Op, in.Cls)
	if len(in.Args) == 2 { // indexed
		base := e.srcReg(in.Args[0], 1, 8)
		option, s := decodeAmode(in.Aux)
		index := e.srcReg(in.Args[1], 0, indexSize(option))
		d, done := e.dstReg(in.To, sz)
		e.line("%s %s, [%s, %s%s]", mn, d, base, index, amodeSuffix(option, s, in.Op))
		done()
		return
	}
	addr := e.srcReg(in.Args[0], 1, 8)
	d, done := e.dstReg(in.To, sz)
	e.line("%s %s, [%s]", mn, d, addr)
	done()
}

func (e *emitter) emitStore(in *ir.Instr) {
	mn, valSz := storeInfo(in.Op)
	v := e.srcReg(in.Args[0], 0, valSz)
	if len(in.Args) == 3 { // indexed
		base := e.srcReg(in.Args[1], 1, 8)
		option, s := decodeAmode(in.Aux)
		index := e.srcReg(in.Args[2], 2, indexSize(option))
		e.line("%s %s, [%s, %s%s]", mn, v, base, index, amodeSuffix(option, s, in.Op))
		return
	}
	addr := e.srcReg(in.Args[1], 1, 8)
	e.line("%s %s, [%s]", mn, v, addr)
}

// amodeSuffix renders the ", <extend> #shift" part of a register-offset operand.
func amodeSuffix(option, s uint32, op ir.Op) string {
	kw := "lsl"
	switch option {
	case a64.ExtSXTW:
		kw = "sxtw"
	case a64.ExtUXTW:
		kw = "uxtw"
	}
	if s == 0 {
		if kw == "lsl" {
			return "" // [base, index]
		}
		return ", " + kw // [base, index, sxtw]
	}
	return fmt.Sprintf(", %s #%d", kw, log2Bytes(accessBytes(op)))
}

// intScratch and fscratch map a slot to a reserved scratch register. Slot 2
// (x15) is used only by 3-operand indexed stores.
var intScratchRegs = [3]Reg{scratch0, scratch1, scratch2}
var floatScratchRegs = [2]Reg{fscratch0, fscratch1}

// srcReg returns the assembler name of a register holding ref's value at the
// given width, loading from a spill slot or materialising a constant into a
// class-appropriate scratch register when necessary. slot (0 or 1) selects which
// scratch register to borrow.
func (e *emitter) srcReg(ref ir.Ref, slot, size int) string {
	if e.f.ClassOf(ref).IsFloat() {
		return e.srcFloat(ref, slot, size)
	}
	return e.srcInt(ref, slot, size)
}

func (e *emitter) srcInt(ref ir.Ref, slot, size int) string {
	scratch := intScratchRegs[slot]
	switch ref.Kind {
	case ir.RefTemp:
		t := e.f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return Reg(t.Reg).Name(size)
		}
		e.line("ldr %s, [x29, #%d]", scratch.Name(size), e.spillBase+t.Slot)
		return scratch.Name(size)
	case ir.RefConst:
		c := e.f.Consts[ref.ID]
		switch c.Kind {
		case ir.ConstInt:
			e.movImm(scratch, c.Int, size)
			return scratch.Name(size)
		case ir.ConstSym:
			e.materializeSym(scratch, c)
			return scratch.Name(size)
		}
	}
	e.fail("arm64: cannot materialise operand %v", ref)
	return scratch.Name(size)
}

func (e *emitter) srcFloat(ref ir.Ref, slot, size int) string {
	fs := floatScratchRegs[slot]
	switch ref.Kind {
	case ir.RefTemp:
		t := e.f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return Reg(t.Reg).Name(size)
		}
		e.line("ldr %s, [x29, #%d]", fs.Name(size), e.spillBase+t.Slot)
		return fs.Name(size)
	case ir.RefConst:
		c := e.f.Consts[ref.ID]
		// A float constant carries its value in Flt; QBE also permits a float
		// operand written as a raw integer bit pattern (a ConstInt of float
		// class), whose Int field is the bits directly.
		bits, ok := floatConstBits(c)
		if ok {
			// Materialise the bit pattern in an integer scratch, then move it
			// across to the SIMD register.
			e.movImm(scratch0, bits, size)
			e.line("fmov %s, %s", fs.Name(size), scratch0.Name(size))
			return fs.Name(size)
		}
	}
	e.fail("arm64: cannot materialise float operand %v", ref)
	return fs.Name(size)
}

// floatConstBits returns the bit pattern of a float-valued constant, whether
// written as a float (Flt) or as a raw integer bit pattern (Int of float class).
func floatConstBits(c ir.Const) (int64, bool) {
	switch c.Kind {
	case ir.ConstFloat:
		return floatBits(c), true
	case ir.ConstInt:
		return c.Int, true
	}
	return 0, false
}

// dstReg returns the register to write a result into and a finaliser that
// stores it back to the stack when the result is spilled.
func (e *emitter) dstReg(ref ir.Ref, size int) (string, func()) {
	t := e.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg).Name(size), func() {}
	}
	scratch := scratch0
	if t.Cls.IsFloat() {
		scratch = fscratch0
	}
	name := scratch.Name(size)
	return name, func() {
		e.line("str %s, [x29, #%d]", name, e.spillBase+t.Slot)
	}
}

// isConstSym reports whether ref is a symbol-address constant.
func (e *emitter) isConstSym(ref ir.Ref) bool {
	return ref.Kind == ir.RefConst && e.f.Consts[ref.ID].Kind == ir.ConstSym
}

// materializeSym loads a symbol address (plus offset) into r via adrp/add.
func (e *emitter) materializeSym(r Reg, c ir.Const) {
	sym := sanitize(c.Sym) // match the sanitized symbol emitted by emitData
	e.line("adrp %s, %s", r.xName(), sym)
	e.line("add %s, %s, :lo12:%s", r.xName(), r.xName(), sym)
	if c.Int != 0 {
		e.line("add %s, %s, #%d", r.xName(), r.xName(), c.Int)
	}
}

// floatBits returns the IEEE-754 bit pattern of a floating constant.
func floatBits(c ir.Const) int64 {
	if c.Cls == ir.ClsS {
		return int64(math.Float32bits(float32(c.Flt)))
	}
	return int64(math.Float64bits(c.Flt))
}

// locOf resolves a ref to a parallel-move location.
func (e *emitter) locOf(ref ir.Ref) loc {
	switch ref.Kind {
	case ir.RefTemp:
		t := e.f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return loc{reg: Reg(t.Reg), size: t.Cls.Size()}
		}
		return loc{mem: true, slot: t.Slot, size: t.Cls.Size()}
	case ir.RefConst:
		c := e.f.Consts[ref.ID]
		if c.Kind == ir.ConstFloat {
			// Carry the bit pattern; emitMoveLoc moves it via a GPR into the
			// SIMD destination.
			return loc{imm: true, val: floatBits(c), size: c.Cls.Size()}
		}
		if c.Kind == ir.ConstInt {
			return loc{imm: true, val: c.Int, size: c.Cls.Size()}
		}
	}
	e.fail("arm64: cannot move operand %v", ref)
	return loc{}
}

func (e *emitter) movImm(r Reg, val int64, size int) {
	u := uint64(val)
	chunks := 4
	if size == 4 {
		u &= 0xffffffff
		chunks = 2
	}
	name := r.Name(size)
	// Prefer a MOVN base when the value is dominated by set bits, so constants
	// like -1 cost a single instruction.
	ones, zeros := 0, 0
	for i := 0; i < chunks; i++ {
		switch (u >> (16 * i)) & 0xffff {
		case 0xffff:
			ones++
		case 0:
			zeros++
		}
	}
	if ones > zeros {
		first := true
		for i := 0; i < chunks; i++ {
			c := (u >> (16 * i)) & 0xffff
			if c == 0xffff {
				continue
			}
			if first {
				e.line("movn %s, #%d, lsl #%d", name, (^c)&0xffff, 16*i)
				first = false
			} else {
				e.line("movk %s, #%d, lsl #%d", name, c, 16*i)
			}
		}
		if first {
			e.line("movn %s, #0", name)
		}
		return
	}
	first := true
	for i := 0; i < chunks; i++ {
		c := (u >> (16 * i)) & 0xffff
		if c == 0 {
			continue
		}
		if first {
			e.line("movz %s, #%d, lsl #%d", name, c, 16*i)
			first = false
		} else {
			e.line("movk %s, #%d, lsl #%d", name, c, 16*i)
		}
	}
	if first {
		e.line("movz %s, #0", name)
	}
}
