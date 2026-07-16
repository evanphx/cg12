package arm64

import (
	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
)

// asmb is the arm64 instruction builder: an arm64-idiomatic assembler surface
// whose methods mirror the a64 encoders and take physical registers. Two
// backends implement it -- mcAsm encodes each instruction into machine-code
// words, textAsm renders it as an ARM assembly line -- so the instruction
// selection that drives it is written once and serves both the object and the
// assembly-text outputs. It is also the seam for a text-input assembler that
// turns machine-specific instruction sequences into either output.
//
// This is the foundation of the emitter unification: selection code targets
// asmb rather than duplicating an encode path and a text path.
type asmb interface {
	// Data processing (register forms).
	addReg(w64 bool, rd, rn, rm Reg)
	subReg(w64 bool, rd, rn, rm Reg)
	mul(w64 bool, rd, rn, rm Reg)
	madd(w64 bool, rd, rn, rm, ra Reg)
	msub(w64 bool, rd, rn, rm, ra Reg)
	sdiv(w64 bool, rd, rn, rm Reg)
	udiv(w64 bool, rd, rn, rm Reg)
	logicalReg(op logicalOp, w64 bool, rd, rn, rm Reg)
	shiftReg(op shiftOp, w64 bool, rd, rn, rm Reg)
	neg(w64 bool, rd, rm Reg)
	mvn(w64 bool, rd, rm Reg)
	clz(w64 bool, rd, rn Reg)
	movReg(w64 bool, rd, rm Reg)

	// Data processing (immediate forms).
	addImm(w64 bool, rd, rn Reg, imm uint32, lsl12 bool)
	subImm(w64 bool, rd, rn Reg, imm uint32, lsl12 bool)
	logicalImm(op logicalOp, w64 bool, rd, rn Reg, imm uint64)
	shiftImm(op shiftOp, w64 bool, rd, rn Reg, sh uint32)
	rotrImm(w64 bool, rd, rn Reg, sh uint32)

	// Floating point.
	fop(op floatOp, dbl bool, rd, rn, rm Reg)
	fneg(dbl bool, rd, rn Reg)
	fcvtStoD(rd, rn Reg)
	fcvtDtoS(rd, rn Reg)
	fcvtzs(dstW64, srcDbl bool, rd, rn Reg)
	fcvtzu(dstW64, srcDbl bool, rd, rn Reg)
	scvtf(dstDbl, srcW64 bool, rd, rn Reg)
	ucvtf(dstDbl, srcW64 bool, rd, rn Reg)
	fmovFromGP(dbl bool, rd, rn Reg)
	fmovToGP(dbl bool, rd, rn Reg)

	// Integer sub-word extends (dstSize/srcSize are the register widths in bytes).
	ext(op extOp, rd, rn Reg, dstSize, srcSize int)

	// Compare and conditional set.
	cmpReg(w64 bool, rn, rm Reg)
	cmpImm(w64 bool, rn Reg, imm uint32)
	fcmp(dbl bool, rn, rm Reg)
	cset(rd Reg, cmp ir.Cmp, float bool)

	// Constant materialization (movz/movk/movn sequence).
	movImm(rd Reg, val int64, w64 bool)

	// Spill traffic: a load into / store from a frame slot at x29+off.
	ldrSpill(rd Reg, float bool, off, size int)
	strSpill(rs Reg, float bool, off, size int)

	// Escape hatch: a fully-encoded instruction word (for forms not yet modelled).
	raw(word uint32)

	fail(format string, a ...any)
}

// logicalOp and shiftOp name the bitwise and shift instruction families so one
// builder method covers each, keeping the interface compact.
type logicalOp uint8

const (
	logAnd logicalOp = iota
	logOrr
	logEor
	logBic
)

type shiftOp uint8

const (
	shLsl shiftOp = iota
	shLsr
	shAsr
)

type floatOp uint8

const (
	fAdd floatOp = iota
	fSub
	fMul
	fDiv
)

// extOp names a sub-word sign/zero extend: sxtb/uxtb/sxth/uxth/sxtw.
type extOp uint8

const (
	extSb extOp = iota
	extUb
	extSh
	extUh
	extSw
)

// --- machine-code backend --------------------------------------------------

// mcAsm encodes instructions into a64 words via the program assembler.
type mcAsm struct {
	prog *a64.Program
	m    *mc // for spill offset splitting and error reporting
}

func (b *mcAsm) addReg(w64 bool, rd, rn, rm Reg) {
	b.prog.Emit(a64.AddReg(w64, mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) subReg(w64 bool, rd, rn, rm Reg) {
	b.prog.Emit(a64.SubReg(w64, mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) mul(w64 bool, rd, rn, rm Reg) {
	b.prog.Emit(a64.Mul(w64, mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) madd(w64 bool, rd, rn, rm, ra Reg) {
	b.prog.Emit(a64.Madd(w64, mreg(rd), mreg(rn), mreg(rm), mreg(ra)))
}
func (b *mcAsm) msub(w64 bool, rd, rn, rm, ra Reg) {
	b.prog.Emit(a64.Msub(w64, mreg(rd), mreg(rn), mreg(rm), mreg(ra)))
}
func (b *mcAsm) sdiv(w64 bool, rd, rn, rm Reg) {
	b.prog.Emit(a64.Sdiv(w64, mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) udiv(w64 bool, rd, rn, rm Reg) {
	b.prog.Emit(a64.Udiv(w64, mreg(rd), mreg(rn), mreg(rm)))
}

func (b *mcAsm) logicalReg(op logicalOp, w64 bool, rd, rn, rm Reg) {
	var enc func(bool, a64.Reg, a64.Reg, a64.Reg) uint32
	switch op {
	case logAnd:
		enc = a64.AndReg
	case logOrr:
		enc = a64.OrrReg
	case logEor:
		enc = a64.EorReg
	case logBic:
		enc = a64.BicReg
	}
	b.prog.Emit(enc(w64, mreg(rd), mreg(rn), mreg(rm)))
}

func (b *mcAsm) shiftReg(op shiftOp, w64 bool, rd, rn, rm Reg) {
	var enc func(bool, a64.Reg, a64.Reg, a64.Reg) uint32
	switch op {
	case shLsl:
		enc = a64.Lslv
	case shLsr:
		enc = a64.Lsrv
	case shAsr:
		enc = a64.Asrv
	}
	b.prog.Emit(enc(w64, mreg(rd), mreg(rn), mreg(rm)))
}

func (b *mcAsm) neg(w64 bool, rd, rm Reg)    { b.prog.Emit(a64.NegReg(w64, mreg(rd), mreg(rm))) }
func (b *mcAsm) mvn(w64 bool, rd, rm Reg)    { b.prog.Emit(a64.MvnReg(w64, mreg(rd), mreg(rm))) }
func (b *mcAsm) clz(w64 bool, rd, rn Reg)    { b.prog.Emit(a64.Clz(w64, mreg(rd), mreg(rn))) }
func (b *mcAsm) movReg(w64 bool, rd, rm Reg) { b.prog.Emit(a64.MovReg(w64, mreg(rd), mreg(rm))) }

func (b *mcAsm) addImm(w64 bool, rd, rn Reg, imm uint32, lsl12 bool) {
	if lsl12 {
		b.prog.Emit(a64.AddImmLSL12(w64, mreg(rd), mreg(rn), imm))
	} else {
		b.prog.Emit(a64.AddImm(w64, mreg(rd), mreg(rn), imm))
	}
}
func (b *mcAsm) subImm(w64 bool, rd, rn Reg, imm uint32, lsl12 bool) {
	if lsl12 {
		b.prog.Emit(a64.SubImmLSL12(w64, mreg(rd), mreg(rn), imm))
	} else {
		b.prog.Emit(a64.SubImm(w64, mreg(rd), mreg(rn), imm))
	}
}
func (b *mcAsm) logicalImm(op logicalOp, w64 bool, rd, rn Reg, imm uint64) {
	size := 8
	if !w64 {
		size = 4
	}
	n, immr, imms, ok := a64.EncodeBitmask(imm, size)
	if !ok {
		b.fail("arm64: %#x is not a logical immediate", imm)
		return
	}
	var enc func(bool, a64.Reg, a64.Reg, uint32, uint32, uint32) uint32
	switch op {
	case logAnd:
		enc = a64.AndImm
	case logOrr:
		enc = a64.OrrImm
	case logEor:
		enc = a64.EorImm
	default:
		b.fail("arm64: bic has no immediate form")
		return
	}
	b.prog.Emit(enc(w64, mreg(rd), mreg(rn), n, immr, imms))
}
func (b *mcAsm) shiftImm(op shiftOp, w64 bool, rd, rn Reg, sh uint32) {
	var w uint32
	switch op {
	case shLsl:
		w = a64.LslImm(w64, mreg(rd), mreg(rn), sh)
	case shLsr:
		w = a64.LsrImm(w64, mreg(rd), mreg(rn), sh)
	case shAsr:
		w = a64.AsrImm(w64, mreg(rd), mreg(rn), sh)
	}
	b.prog.Emit(w)
}
func (b *mcAsm) rotrImm(w64 bool, rd, rn Reg, sh uint32) {
	b.prog.Emit(a64.RorImm(w64, mreg(rd), mreg(rn), sh))
}
func (b *mcAsm) fop(op floatOp, dbl bool, rd, rn, rm Reg) {
	var enc func(bool, a64.Reg, a64.Reg, a64.Reg) uint32
	switch op {
	case fAdd:
		enc = a64.Fadd
	case fSub:
		enc = a64.Fsub
	case fMul:
		enc = a64.Fmul
	case fDiv:
		enc = a64.Fdiv
	}
	b.prog.Emit(enc(dbl, mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) fneg(dbl bool, rd, rn Reg) { b.prog.Emit(a64.Fneg(dbl, mreg(rd), mreg(rn))) }
func (b *mcAsm) fcvtStoD(rd, rn Reg)       { b.prog.Emit(a64.FcvtStoD(mreg(rd), mreg(rn))) }
func (b *mcAsm) fcvtDtoS(rd, rn Reg)       { b.prog.Emit(a64.FcvtDtoS(mreg(rd), mreg(rn))) }
func (b *mcAsm) fcvtzs(dstW64, srcDbl bool, rd, rn Reg) {
	b.prog.Emit(a64.Fcvtzs(dstW64, srcDbl, mreg(rd), mreg(rn)))
}
func (b *mcAsm) fcvtzu(dstW64, srcDbl bool, rd, rn Reg) {
	b.prog.Emit(a64.Fcvtzu(dstW64, srcDbl, mreg(rd), mreg(rn)))
}
func (b *mcAsm) scvtf(dstDbl, srcW64 bool, rd, rn Reg) {
	b.prog.Emit(a64.Scvtf(dstDbl, srcW64, mreg(rd), mreg(rn)))
}
func (b *mcAsm) ucvtf(dstDbl, srcW64 bool, rd, rn Reg) {
	b.prog.Emit(a64.Ucvtf(dstDbl, srcW64, mreg(rd), mreg(rn)))
}
func (b *mcAsm) fmovFromGP(dbl bool, rd, rn Reg) {
	b.prog.Emit(a64.FmovFromGP(dbl, mreg(rd), mreg(rn)))
}
func (b *mcAsm) fmovToGP(dbl bool, rd, rn Reg) { b.prog.Emit(a64.FmovToGP(dbl, mreg(rd), mreg(rn))) }
func (b *mcAsm) ext(op extOp, rd, rn Reg, dstSize, srcSize int) {
	w64 := dstSize == 8
	var w uint32
	switch op {
	case extSb:
		w = a64.Sxtb(w64, mreg(rd), mreg(rn))
	case extUb:
		w = a64.Uxtb(mreg(rd), mreg(rn))
	case extSh:
		w = a64.Sxth(w64, mreg(rd), mreg(rn))
	case extUh:
		w = a64.Uxth(mreg(rd), mreg(rn))
	case extSw:
		w = a64.Sxtw(mreg(rd), mreg(rn))
	}
	b.prog.Emit(w)
}
func (b *mcAsm) cmpReg(w64 bool, rn, rm Reg)         { b.prog.Emit(a64.CmpReg(w64, mreg(rn), mreg(rm))) }
func (b *mcAsm) cmpImm(w64 bool, rn Reg, imm uint32) { b.prog.Emit(a64.CmpImm(w64, mreg(rn), imm)) }
func (b *mcAsm) fcmp(dbl bool, rn, rm Reg)           { b.prog.Emit(a64.Fcmp(dbl, mreg(rn), mreg(rm))) }
func (b *mcAsm) cset(rd Reg, cmp ir.Cmp, float bool) {
	var cond a64.Cond
	var ok bool
	if float {
		cond, ok = fpCondOf(cmp)
	} else {
		cond, ok = intCondOf(cmp)
	}
	if !ok {
		b.fail("arm64: unsupported comparison predicate %v", cmp)
		return
	}
	b.prog.Emit(a64.Cset(false, mreg(rd), cond))
}
func (b *mcAsm) movImm(rd Reg, val int64, w64 bool) { b.m.movImm(mreg(rd), val, w64) }
func (b *mcAsm) ldrSpill(rd Reg, float bool, off, size int) {
	b.m.spillLoad(mreg(rd), float, off, size)
}
func (b *mcAsm) strSpill(rs Reg, float bool, off, size int) {
	b.m.spillStore(mreg(rs), float, off, size)
}
func (b *mcAsm) raw(word uint32)              { b.prog.Emit(word) }
func (b *mcAsm) fail(format string, a ...any) { b.m.fail(format, a...) }

// --- text backend ----------------------------------------------------------

// textAsm renders instructions as ARM assembly lines.
type textAsm struct {
	e *emitter
}

func (b *textAsm) line(format string, a ...any) { b.e.line(format, a...) }

func (b *textAsm) addReg(w64 bool, rd, rn, rm Reg) { b.r3("add", w64, rd, rn, rm) }
func (b *textAsm) subReg(w64 bool, rd, rn, rm Reg) { b.r3("sub", w64, rd, rn, rm) }
func (b *textAsm) mul(w64 bool, rd, rn, rm Reg)    { b.r3("mul", w64, rd, rn, rm) }
func (b *textAsm) sdiv(w64 bool, rd, rn, rm Reg)   { b.r3("sdiv", w64, rd, rn, rm) }
func (b *textAsm) udiv(w64 bool, rd, rn, rm Reg)   { b.r3("udiv", w64, rd, rn, rm) }
func (b *textAsm) madd(w64 bool, rd, rn, rm, ra Reg) {
	s := regSize(w64)
	b.line("madd %s, %s, %s, %s", rd.Name(s), rn.Name(s), rm.Name(s), ra.Name(s))
}
func (b *textAsm) msub(w64 bool, rd, rn, rm, ra Reg) {
	s := regSize(w64)
	b.line("msub %s, %s, %s, %s", rd.Name(s), rn.Name(s), rm.Name(s), ra.Name(s))
}
func (b *textAsm) logicalReg(op logicalOp, w64 bool, rd, rn, rm Reg) {
	b.r3([]string{"and", "orr", "eor", "bic"}[op], w64, rd, rn, rm)
}
func (b *textAsm) shiftReg(op shiftOp, w64 bool, rd, rn, rm Reg) {
	b.r3([]string{"lsl", "lsr", "asr"}[op], w64, rd, rn, rm)
}
func (b *textAsm) neg(w64 bool, rd, rm Reg)    { b.r2("neg", w64, rd, rm) }
func (b *textAsm) mvn(w64 bool, rd, rm Reg)    { b.r2("mvn", w64, rd, rm) }
func (b *textAsm) clz(w64 bool, rd, rn Reg)    { b.r2("clz", w64, rd, rn) }
func (b *textAsm) movReg(w64 bool, rd, rm Reg) { b.r2("mov", w64, rd, rm) }
func (b *textAsm) addImm(w64 bool, rd, rn Reg, imm uint32, lsl12 bool) {
	b.addSubImmLine("add", w64, rd, rn, imm, lsl12)
}
func (b *textAsm) subImm(w64 bool, rd, rn Reg, imm uint32, lsl12 bool) {
	b.addSubImmLine("sub", w64, rd, rn, imm, lsl12)
}
func (b *textAsm) logicalImm(op logicalOp, w64 bool, rd, rn Reg, imm uint64) {
	s := regSize(w64)
	b.line("%s %s, %s, #%d", []string{"and", "orr", "eor", "bic"}[op], rd.Name(s), rn.Name(s), int64(imm))
}
func (b *textAsm) shiftImm(op shiftOp, w64 bool, rd, rn Reg, sh uint32) {
	s := regSize(w64)
	b.line("%s %s, %s, #%d", []string{"lsl", "lsr", "asr"}[op], rd.Name(s), rn.Name(s), sh)
}
func (b *textAsm) rotrImm(w64 bool, rd, rn Reg, sh uint32) {
	s := regSize(w64)
	b.line("ror %s, %s, #%d", rd.Name(s), rn.Name(s), sh)
}
func (b *textAsm) movImm(rd Reg, val int64, w64 bool) { b.e.movImm(rd, val, boolToSize(w64)) }
func (b *textAsm) ldrSpill(rd Reg, float bool, off, size int) {
	b.line("ldr %s, [x29, #%d]", rd.Name(size), off)
}
func (b *textAsm) strSpill(rs Reg, float bool, off, size int) {
	b.line("str %s, [x29, #%d]", rs.Name(size), off)
}
func (b *textAsm) raw(word uint32)              { b.line(".inst 0x%08x", word) }
func (b *textAsm) fail(format string, a ...any) { b.e.fail(format, a...) }

func (b *textAsm) fop(op floatOp, dbl bool, rd, rn, rm Reg) {
	s := regSize(dbl)
	b.line("%s %s, %s, %s", []string{"fadd", "fsub", "fmul", "fdiv"}[op], rd.Name(s), rn.Name(s), rm.Name(s))
}
func (b *textAsm) fneg(dbl bool, rd, rn Reg) {
	s := regSize(dbl)
	b.line("fneg %s, %s", rd.Name(s), rn.Name(s))
}
func (b *textAsm) fcvtStoD(rd, rn Reg) { b.line("fcvt %s, %s", rd.Name(8), rn.Name(4)) }
func (b *textAsm) fcvtDtoS(rd, rn Reg) { b.line("fcvt %s, %s", rd.Name(4), rn.Name(8)) }
func (b *textAsm) fcvtzs(dstW64, srcDbl bool, rd, rn Reg) {
	b.line("fcvtzs %s, %s", rd.Name(regSize(dstW64)), rn.Name(regSize(srcDbl)))
}
func (b *textAsm) fcvtzu(dstW64, srcDbl bool, rd, rn Reg) {
	b.line("fcvtzu %s, %s", rd.Name(regSize(dstW64)), rn.Name(regSize(srcDbl)))
}
func (b *textAsm) scvtf(dstDbl, srcW64 bool, rd, rn Reg) {
	b.line("scvtf %s, %s", rd.Name(regSize(dstDbl)), rn.Name(regSize(srcW64)))
}
func (b *textAsm) ucvtf(dstDbl, srcW64 bool, rd, rn Reg) {
	b.line("ucvtf %s, %s", rd.Name(regSize(dstDbl)), rn.Name(regSize(srcW64)))
}
func (b *textAsm) fmovFromGP(dbl bool, rd, rn Reg) {
	s := regSize(dbl)
	b.line("fmov %s, %s", rd.Name(s), rn.Name(s))
}
func (b *textAsm) fmovToGP(dbl bool, rd, rn Reg) {
	s := regSize(dbl)
	b.line("fmov %s, %s", rd.Name(s), rn.Name(s))
}
func (b *textAsm) ext(op extOp, rd, rn Reg, dstSize, srcSize int) {
	b.line("%s %s, %s", []string{"sxtb", "uxtb", "sxth", "uxth", "sxtw"}[op], rd.Name(dstSize), rn.Name(srcSize))
}
func (b *textAsm) cmpReg(w64 bool, rn, rm Reg) {
	s := regSize(w64)
	b.line("cmp %s, %s", rn.Name(s), rm.Name(s))
}
func (b *textAsm) cmpImm(w64 bool, rn Reg, imm uint32) {
	b.line("cmp %s, #%d", rn.Name(regSize(w64)), imm)
}
func (b *textAsm) fcmp(dbl bool, rn, rm Reg) {
	s := regSize(dbl)
	b.line("fcmp %s, %s", rn.Name(s), rm.Name(s))
}
func (b *textAsm) cset(rd Reg, cmp ir.Cmp, float bool) {
	var cond string
	var ok bool
	if float {
		cond, ok = fpCondCode(cmp)
	} else {
		cond, ok = condCode(cmp)
	}
	if !ok {
		b.e.fail("arm64: unsupported comparison predicate %v", cmp)
		return
	}
	b.line("cset %s, %s", rd.Name(4), cond)
}

func (b *textAsm) r3(mn string, w64 bool, rd, rn, rm Reg) {
	s := regSize(w64)
	b.line("%s %s, %s, %s", mn, rd.Name(s), rn.Name(s), rm.Name(s))
}
func (b *textAsm) r2(mn string, w64 bool, rd, rm Reg) {
	s := regSize(w64)
	b.line("%s %s, %s", mn, rd.Name(s), rm.Name(s))
}
func (b *textAsm) addSubImmLine(mn string, w64 bool, rd, rn Reg, imm uint32, lsl12 bool) {
	s := regSize(w64)
	if lsl12 {
		b.line("%s %s, %s, #%d, lsl #12", mn, rd.Name(s), rn.Name(s), imm)
	} else {
		b.line("%s %s, %s, #%d", mn, rd.Name(s), rn.Name(s), imm)
	}
}

func regSize(w64 bool) int {
	if w64 {
		return 8
	}
	return 4
}
func boolToSize(w64 bool) int { return regSize(w64) }
