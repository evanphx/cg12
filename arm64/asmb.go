package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
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
	addShiftedReg(w64 bool, sh shiftOp, amt uint32, rd, rn, rm Reg)
	subShiftedReg(w64 bool, sh shiftOp, amt uint32, rd, rn, rm Reg)
	mul(w64 bool, rd, rn, rm Reg)
	madd(w64 bool, rd, rn, rm, ra Reg)
	msub(w64 bool, rd, rn, rm, ra Reg)
	umulh(rd, rn, rm Reg)
	smulh(rd, rn, rm Reg)
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
	tstImm(w64 bool, rn Reg, imm uint64)
	tstReg(w64 bool, rn, rm Reg)
	shiftImm(op shiftOp, w64 bool, rd, rn Reg, sh uint32)
	rotrImm(w64 bool, rd, rn Reg, sh uint32)
	ubfx(w64 bool, rd, rn Reg, lsb, width uint32)

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
	ccmpReg(w64 bool, rn, rm Reg, nzcv uint32, cond a64.Cond)
	ccmpImm(w64 bool, rn Reg, imm5, nzcv uint32, cond a64.Cond)
	fcmp(dbl bool, rn, rm Reg)
	cset(rd Reg, cmp ir.Cmp, float bool)
	csel(w64 bool, rd, rn, rm Reg, cond a64.Cond)
	fcsel(dbl bool, rd, rn, rm Reg, cond a64.Cond)

	// Constant materialization: an integer (movz/movk/movn) or a symbol address
	// (adrp+add, recording relocations or emitting :lo12: syntax per backend).
	movImm(rd Reg, val int64, w64 bool)
	materializeSym(rd Reg, c ir.Const)

	// rematerialize recomputes a rematerialisable value (a stack-slot address,
	// constant, or symbol address) into rd, in place of reloading it from a slot.
	rematerialize(rd Reg, rule rematRule, size int)

	// moveLoc emits a single move between two locations (register or frame slot),
	// the primitive the shared parallel-move ordering drives.
	moveLoc(dst, src loc)

	// Thread-local storage. threadPtr reads the running thread's pointer;
	// tlsOffset loads a variable's distance from it, the way the chosen model
	// says. Each needs only the one register it writes -- an address is the sum of
	// the two, which the selector emits as an ordinary add.
	threadPtr(rd Reg)
	tlsOffset(rd Reg, c ir.Const)
	tlsIndexAddr(rd Reg, c ir.Const)

	// Memory. load/store use [base]; loadIdx/storeIdx use [base, index] with the
	// extend/scale encoded in aux. The op fixes the width and signedness.
	load(op ir.Op, cls ir.Cls, rd, base Reg, off uint32)
	loadIdx(op ir.Op, cls ir.Cls, rd, base, index Reg, amode int32)
	store(op ir.Op, val, base Reg, off uint32)
	storeIdx(op ir.Op, val, base, index Reg, amode int32)
	// loadSym/storeSym access a symbol at offset 0 with the low-12 bits folded into
	// the access: `adrp page, sym; ldr/str [page, #:lo12:sym]`, no separate address
	// add. page is a scratch register the adrp writes.
	loadSym(op ir.Op, cls ir.Cls, rd, page Reg, sym string)
	storeSym(op ir.Op, val, page Reg, sym string)
	// loadWB/storeWB fold a pointer advance into the access with pre/post-indexed
	// write-back; they report false for forms not covered (FP), leaving the caller
	// to emit the access and the advance separately.
	loadWB(op ir.Op, cls ir.Cls, rd, base Reg, imm int32, wb a64.WriteBack) bool
	storeWB(op ir.Op, val, base Reg, imm int32, wb a64.WriteBack) bool

	// Control flow. Branch targets are blocks so each backend formats its own
	// label; brind is a computed goto through a register, brk halts.
	branch(to *ir.Block)
	cbnz(w64 bool, rn Reg, to *ir.Block)
	cbz(w64 bool, rn Reg, to *ir.Block)
	tbz(bit uint32, rn Reg, to *ir.Block)
	tbnz(bit uint32, rn Reg, to *ir.Block)
	brind(rn Reg)
	brk()

	// Atomics and barriers. ldar/stlr are an acquire load and release store; the
	// exclusive pair (ldaxr/stlxr) plus these build an atomic read-modify-write as
	// a retry loop, and dmb is the fence. The retry loop is emitted with a
	// selector-local label: freshLabel mints a unique name, label plants it, and
	// branchTo/cbnzTo/bcondTo branch to it by name.
	ldar(bytes int, rt, rn Reg)
	stlr(bytes int, rt, rn Reg)
	ldaxr(bytes int, rt, rn Reg)
	stlxr(bytes int, rs, rt, rn Reg)
	dmb(o a64.BarrierOption)
	freshLabel(prefix string) string
	label(name string)
	branchTo(name string)
	cbzTo(w64 bool, rn Reg, name string)
	cbnzTo(w64 bool, rn Reg, name string)
	bcondTo(cond a64.Cond, name string)

	// jumpTable emits an indexed branch through a PC-relative offset table placed
	// just past the branch: target = table + (int32)table[idx]. idx is already
	// bounds-checked. x17/x15 are free scratch at a terminator.
	jumpTable(idx Reg, blk *ir.Block, targets []*ir.Block)

	// Calls. callSym is a direct bl to a named function (recorded as a CALL26
	// relocation in object code); callReg is an indirect blr through a register.
	callSym(sym string)
	callReg(rn Reg)

	// Frame teardown, shared by the return epilogue and the tail-call branch:
	// movSPFromFP restores sp from the frame pointer (undoing any VLA growth),
	// frameClose restores x29/x30 and pops the frame (an ldp with the size-dependent
	// index/offset form), and ret returns through x30.
	movSPFromFP()
	frameClose(frame int)
	ret()

	// Stack pointer and block addresses. allocN grows the stack by a runtime size
	// (VLA); movFromSP/movToSP snapshot and restore it; adr materializes a block's
	// PC-relative address.
	allocN(rd, size Reg)
	movFromSP(rd Reg)
	movToSP(rn Reg)
	callerPC(rd Reg)
	callerSP(rd Reg)
	adr(rd Reg, to *ir.Block)

	// Spill traffic: a load into / store from a frame slot at x29+off.
	frameAddr(rd Reg, off int)
	ldrSpill(rd Reg, float bool, off, size int)
	strSpill(rs Reg, float bool, off, size int)
	// ldpFrame restores two integer registers from adjacent frame slots at
	// [x29, #off] with one ldp -- the epilogue counterpart of the prologue's stp.
	ldpFrame(rt, rt2 Reg, off int)

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
func (b *mcAsm) addShiftedReg(w64 bool, sh shiftOp, amt uint32, rd, rn, rm Reg) {
	b.prog.Emit(a64.AddShiftedReg(w64, uint32(sh), amt, mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) subShiftedReg(w64 bool, sh shiftOp, amt uint32, rd, rn, rm Reg) {
	b.prog.Emit(a64.SubShiftedReg(w64, uint32(sh), amt, mreg(rd), mreg(rn), mreg(rm)))
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
func (b *mcAsm) umulh(rd, rn, rm Reg) {
	b.prog.Emit(a64.Umulh(mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) smulh(rd, rn, rm Reg) {
	b.prog.Emit(a64.Smulh(mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) sdiv(w64 bool, rd, rn, rm Reg) {
	b.prog.Emit(a64.Sdiv(w64, mreg(rd), mreg(rn), mreg(rm)))
}
func (b *mcAsm) udiv(w64 bool, rd, rn, rm Reg) {
	b.prog.Emit(a64.Udiv(w64, mreg(rd), mreg(rn), mreg(rm)))
}

// tstImm/tstReg emit TST -- ANDS into XZR, a flags-only mask test for a fused
// `if (x & mask)` branch.
func (b *mcAsm) tstImm(w64 bool, rn Reg, imm uint64) {
	size := 8
	if !w64 {
		size = 4
	}
	n, immr, imms, ok := a64.EncodeBitmask(imm, size)
	if !ok {
		b.fail("arm64: %#x is not a logical immediate", imm)
		return
	}
	b.prog.Emit(a64.AndsImm(w64, a64.Reg(31), mreg(rn), n, immr, imms))
}
func (b *mcAsm) tstReg(w64 bool, rn, rm Reg) {
	b.prog.Emit(a64.AndsReg(w64, a64.Reg(31), mreg(rn), mreg(rm)))
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
func (b *mcAsm) ubfx(w64 bool, rd, rn Reg, lsb, width uint32) {
	b.prog.Emit(a64.Ubfm(w64, mreg(rd), mreg(rn), lsb, lsb+width-1))
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
func (b *mcAsm) ccmpReg(w64 bool, rn, rm Reg, nzcv uint32, cond a64.Cond) {
	b.prog.Emit(a64.CcmpReg(w64, mreg(rn), mreg(rm), nzcv, cond))
}
func (b *mcAsm) ccmpImm(w64 bool, rn Reg, imm5, nzcv uint32, cond a64.Cond) {
	b.prog.Emit(a64.CcmpImm(w64, mreg(rn), imm5, nzcv, cond))
}
func (b *mcAsm) fcmp(dbl bool, rn, rm Reg) { b.prog.Emit(a64.Fcmp(dbl, mreg(rn), mreg(rm))) }
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
func (b *mcAsm) csel(w64 bool, rd, rn, rm Reg, cond a64.Cond) {
	b.prog.Emit(a64.Csel(w64, mreg(rd), mreg(rn), mreg(rm), cond))
}
func (b *mcAsm) fcsel(dbl bool, rd, rn, rm Reg, cond a64.Cond) {
	b.prog.Emit(a64.Fcsel(dbl, mreg(rd), mreg(rn), mreg(rm), cond))
}
func (b *mcAsm) load(op ir.Op, cls ir.Cls, rd, base Reg, off uint32) {
	d, a := mreg(rd), mreg(base)
	sz := loadSize(op, cls)
	switch op {
	case ir.OLoadl:
		b.prog.Emit(a64.LdrImm(true, d, a, off))
	case ir.OLoaduw:
		b.prog.Emit(a64.LdrImm(false, d, a, off))
	case ir.OLoadsw:
		b.prog.Emit(a64.LdrswImm(d, a, off))
	case ir.OLoadub:
		b.prog.Emit(a64.LdrbImm(d, a, off))
	case ir.OLoadsb:
		b.prog.Emit(a64.LdrsbImm(sz == 8, d, a, off))
	case ir.OLoaduh:
		b.prog.Emit(a64.LdrhImm(d, a, off))
	case ir.OLoadsh:
		b.prog.Emit(a64.LdrshImm(sz == 8, d, a, off))
	case ir.OLoads:
		b.prog.Emit(a64.LdrFP(false, d, a, off))
	case ir.OLoadd:
		b.prog.Emit(a64.LdrFP(true, d, a, off))
	case ir.OLoadq:
		b.prog.Emit(a64.LdrQ(d, a, off))
	default:
		b.fail("arm64: unsupported load %s", op)
	}
}

// loadWB emits a load with pre/post-indexed write-back: the base register is
// updated by imm (an unscaled signed byte offset) as a side effect. Only the
// integer forms are handled; the caller (the write-back peephole) declines FP.
func (b *mcAsm) loadWB(op ir.Op, cls ir.Cls, rd, base Reg, imm int32, wb a64.WriteBack) bool {
	d, a := mreg(rd), mreg(base)
	sz := loadSize(op, cls)
	switch op {
	case ir.OLoadl:
		b.prog.Emit(a64.LdrWB(true, d, a, imm, wb))
	case ir.OLoaduw:
		b.prog.Emit(a64.LdrWB(false, d, a, imm, wb))
	case ir.OLoadsw:
		b.prog.Emit(a64.LdrswWB(d, a, imm, wb))
	case ir.OLoadub:
		b.prog.Emit(a64.LdrbWB(d, a, imm, wb))
	case ir.OLoadsb:
		b.prog.Emit(a64.LdrsbWB(sz == 8, d, a, imm, wb))
	case ir.OLoaduh:
		b.prog.Emit(a64.LdrhWB(d, a, imm, wb))
	case ir.OLoadsh:
		b.prog.Emit(a64.LdrshWB(sz == 8, d, a, imm, wb))
	default:
		return false
	}
	return true
}

// storeWB emits a store with pre/post-indexed write-back.
func (b *mcAsm) storeWB(op ir.Op, val, base Reg, imm int32, wb a64.WriteBack) bool {
	v, a := mreg(val), mreg(base)
	switch op {
	case ir.OStorel:
		b.prog.Emit(a64.StrWB(true, v, a, imm, wb))
	case ir.OStorew:
		b.prog.Emit(a64.StrWB(false, v, a, imm, wb))
	case ir.OStoreb:
		b.prog.Emit(a64.StrbWB(v, a, imm, wb))
	case ir.OStoreh:
		b.prog.Emit(a64.StrhWB(v, a, imm, wb))
	default:
		return false
	}
	return true
}

func (b *mcAsm) loadIdx(op ir.Op, cls ir.Cls, rd, base, index Reg, amode int32) {
	d, ba, ix := mreg(rd), mreg(base), mreg(index)
	option, s := decodeAmode(amode)
	sz := loadSize(op, cls)
	switch op {
	case ir.OLoadl:
		b.prog.Emit(a64.LdrReg(true, d, ba, ix, option, s))
	case ir.OLoaduw:
		b.prog.Emit(a64.LdrReg(false, d, ba, ix, option, s))
	case ir.OLoadsw:
		b.prog.Emit(a64.LdrswReg(d, ba, ix, option, s))
	case ir.OLoadub:
		b.prog.Emit(a64.LdrbReg(d, ba, ix, option, s))
	case ir.OLoadsb:
		b.prog.Emit(a64.LdrsbReg(sz == 8, d, ba, ix, option, s))
	case ir.OLoaduh:
		b.prog.Emit(a64.LdrhReg(d, ba, ix, option, s))
	case ir.OLoadsh:
		b.prog.Emit(a64.LdrshReg(sz == 8, d, ba, ix, option, s))
	default:
		b.fail("arm64: unsupported indexed load %s", op)
	}
}
func (b *mcAsm) store(op ir.Op, val, base Reg, off uint32) {
	v, a := mreg(val), mreg(base)
	switch op {
	case ir.OStorel:
		b.prog.Emit(a64.StrImm(true, v, a, off))
	case ir.OStorew:
		b.prog.Emit(a64.StrImm(false, v, a, off))
	case ir.OStoreb:
		b.prog.Emit(a64.StrbImm(v, a, off))
	case ir.OStoreh:
		b.prog.Emit(a64.StrhImm(v, a, off))
	case ir.OStores:
		b.prog.Emit(a64.StrFP(false, v, a, off))
	case ir.OStored:
		b.prog.Emit(a64.StrFP(true, v, a, off))
	case ir.OStoreq:
		b.prog.Emit(a64.StrQ(v, a, off))
	default:
		b.fail("arm64: unsupported store %s", op)
	}
}
func (b *mcAsm) loadSym(op ir.Op, cls ir.Cls, rd, page Reg, sym string) {
	s := sanitize(sym)
	b.m.reloc(s, obj.R_AARCH64_ADR_PREL_PG_HI21)
	b.prog.Emit(a64.Adrp(mreg(page), 0))
	b.m.reloc(s, ldstLo12Reloc(accessWidth(op)))
	b.load(op, cls, rd, page, 0) // the reloc just recorded patches this access's offset
}
func (b *mcAsm) storeSym(op ir.Op, val, page Reg, sym string) {
	s := sanitize(sym)
	b.m.reloc(s, obj.R_AARCH64_ADR_PREL_PG_HI21)
	b.prog.Emit(a64.Adrp(mreg(page), 0))
	b.m.reloc(s, ldstLo12Reloc(accessWidth(op)))
	b.store(op, val, page, 0)
}
func (b *mcAsm) storeIdx(op ir.Op, val, base, index Reg, amode int32) {
	v, ba, ix := mreg(val), mreg(base), mreg(index)
	option, s := decodeAmode(amode)
	switch op {
	case ir.OStorel:
		b.prog.Emit(a64.StrReg(true, v, ba, ix, option, s))
	case ir.OStorew:
		b.prog.Emit(a64.StrReg(false, v, ba, ix, option, s))
	case ir.OStoreb:
		b.prog.Emit(a64.StrbReg(v, ba, ix, option, s))
	case ir.OStoreh:
		b.prog.Emit(a64.StrhReg(v, ba, ix, option, s))
	default:
		b.fail("arm64: unsupported indexed store %s", op)
	}
}
func (b *mcAsm) movImm(rd Reg, val int64, w64 bool) { b.m.movImm(mreg(rd), val, w64) }
func (b *mcAsm) materializeSym(rd Reg, c ir.Const)  { b.m.materializeSym(mreg(rd), c) }

func (b *mcAsm) rematerialize(rd Reg, rule rematRule, size int) {
	b.m.rematerialize(mreg(rd), rule, size)
}
func (b *mcAsm) moveLoc(dst, src loc)            { b.m.emitMoveLoc(dst, src) }
func (b *mcAsm) threadPtr(rd Reg)                { b.prog.Emit(a64.MrsTPIDR(mreg(rd))) }
func (b *mcAsm) tlsOffset(rd Reg, c ir.Const)    { b.m.emitTLSOffset(mreg(rd), c) }
func (b *mcAsm) tlsIndexAddr(rd Reg, c ir.Const) { b.m.emitTLSIndexAddr(mreg(rd), c) }
func (b *mcAsm) branch(to *ir.Block)             { b.prog.B(to.Name) }
func (b *mcAsm) cbnz(w64 bool, rn Reg, to *ir.Block) {
	b.prog.Cbnz(w64, mreg(rn), to.Name)
}
func (b *mcAsm) cbz(w64 bool, rn Reg, to *ir.Block) {
	b.prog.Cbz(w64, mreg(rn), to.Name)
}
func (b *mcAsm) tbz(bit uint32, rn Reg, to *ir.Block)  { b.prog.Tbz(bit, mreg(rn), to.Name) }
func (b *mcAsm) tbnz(bit uint32, rn Reg, to *ir.Block) { b.prog.Tbnz(bit, mreg(rn), to.Name) }
func (b *mcAsm) brind(rn Reg)                          { b.prog.Emit(a64.Br(mreg(rn))) }
func (b *mcAsm) brk()                                  { b.prog.Emit(a64.Brk(0)) }
func (b *mcAsm) jumpTable(idx Reg, blk *ir.Block, targets []*ir.Block) {
	tbl := blk.Name + ".tbl"
	b.prog.Adr(mcGP1, tbl)                                             // adr  x17, tbl
	b.prog.Emit(a64.LdrswReg(mcGP2, mcGP1, mreg(idx), a64.ExtUXTW, 1)) // ldrsw x15, [x17, idx, uxtw #2]
	b.prog.Emit(a64.AddReg(true, mcGP2, mcGP1, mcGP2))                 // add  x15, x17, x15
	b.prog.Emit(a64.Br(mcGP2))                                         // br   x15
	b.prog.Label(tbl)
	for _, t := range targets {
		b.prog.DataWord(t.Name, tbl) // .word t - tbl
	}
}
func (b *mcAsm) callSym(sym string) {
	b.m.reloc(sanitize(sym), obj.R_AARCH64_CALL26)
	b.prog.Emit(a64.Bl(0))
}
func (b *mcAsm) callReg(rn Reg) { b.prog.Emit(a64.Blr(mreg(rn))) }
func (b *mcAsm) movSPFromFP()   { b.prog.Emit(a64.AddImm(true, mcSP, mcX29, 0)) } // mov sp, x29
func (b *mcAsm) frameClose(frame int) {
	if frame <= 504 {
		b.prog.Emit(a64.Ldp(true, mcX29, mcX30, mcSP, frame, a64.PostIndex))
	} else {
		b.prog.Emit(a64.Ldp(true, mcX29, mcX30, mcSP, 0, a64.SignedOffset))
		b.m.adjustSP(false, frame)
	}
}
func (b *mcAsm) ret() { b.prog.Emit(a64.Ret(mcX30)) }
func (b *mcAsm) allocN(rd, size Reg) {
	b.prog.Emit(a64.AddImm(true, mcGP2, mcSP, 0))              // x15 = sp
	b.prog.Emit(a64.SubReg(true, mreg(rd), mcGP2, mreg(size))) // rd = sp - size
	b.prog.Emit(a64.AddImm(true, mcSP, mreg(rd), 0))           // sp = rd
}
func (b *mcAsm) movFromSP(rd Reg)          { b.prog.Emit(a64.AddImm(true, mreg(rd), mcSP, 0)) }
func (b *mcAsm) movToSP(rn Reg)            { b.prog.Emit(a64.AddImm(true, mcSP, mreg(rn), 0)) }
func (b *mcAsm) callerPC(rd Reg)           { b.prog.Emit(a64.LdrImm(true, mreg(rd), mcX29, 8)) }
func (b *mcAsm) callerSP(rd Reg)           { b.m.frameAddr(mreg(rd), b.m.frameTop()) }
func (b *mcAsm) adr(rd Reg, to *ir.Block)  { b.prog.Adr(mreg(rd), to.Name) }
func (b *mcAsm) frameAddr(rd Reg, off int) { b.m.frameAddr(mreg(rd), off) }
func (b *mcAsm) ldrSpill(rd Reg, float bool, off, size int) {
	b.m.spillLoad(mreg(rd), float, off, size)
}
func (b *mcAsm) strSpill(rs Reg, float bool, off, size int) {
	b.m.spillStore(mreg(rs), float, off, size)
}
func (b *mcAsm) ldpFrame(rt, rt2 Reg, off int) {
	b.prog.Emit(a64.Ldp(true, mreg(rt), mreg(rt2), mcX29, off, a64.SignedOffset))
}
func (b *mcAsm) ldar(bytes int, rt, rn Reg)  { b.prog.Emit(a64.Ldar(bytes, mreg(rt), mreg(rn))) }
func (b *mcAsm) stlr(bytes int, rt, rn Reg)  { b.prog.Emit(a64.Stlr(bytes, mreg(rt), mreg(rn))) }
func (b *mcAsm) ldaxr(bytes int, rt, rn Reg) { b.prog.Emit(a64.Ldaxr(bytes, mreg(rt), mreg(rn))) }
func (b *mcAsm) stlxr(bytes int, rs, rt, rn Reg) {
	b.prog.Emit(a64.Stlxr(bytes, mreg(rs), mreg(rt), mreg(rn)))
}
func (b *mcAsm) dmb(o a64.BarrierOption) { b.prog.Emit(a64.Dmb(o)) }
func (b *mcAsm) freshLabel(prefix string) string {
	b.m.atomicSeq++
	return fmt.Sprintf("%s_%d", prefix, b.m.atomicSeq)
}
func (b *mcAsm) label(name string)                    { b.prog.Label(name) }
func (b *mcAsm) branchTo(name string)                 { b.prog.B(name) }
func (b *mcAsm) cbzTo(w64 bool, rn Reg, name string)  { b.prog.Cbz(w64, mreg(rn), name) }
func (b *mcAsm) cbnzTo(w64 bool, rn Reg, name string) { b.prog.Cbnz(w64, mreg(rn), name) }
func (b *mcAsm) bcondTo(cond a64.Cond, name string)   { b.prog.Bcond(cond, name) }
func (b *mcAsm) raw(word uint32)                      { b.prog.Emit(word) }
func (b *mcAsm) fail(format string, a ...any)         { b.m.fail(format, a...) }

// --- text backend ----------------------------------------------------------
