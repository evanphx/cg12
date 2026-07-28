package amd64

import (
	"fmt"
	"math"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// xasm is the amd64 instruction builder: an x86-idiomatic assembler surface whose
// operations mirror the x64 encoders, implemented by mcXasm. It keeps the
// instruction selection that drives it (xsel) separate from the encoding, the
// same way asmb does on arm64.
//
// x86 is a two-operand, move-heavy machine, so the fundamental primitive is
// move(dst, src) over the shared loc abstraction; the arithmetic ops take
// registers the selector has already placed operands into.
type xasm interface {
	// refLoc maps an operand to its location (register, spill slot, immediate, or
	// symbol); move transfers between two locations.
	refLoc(ref ir.Ref) loc
	move(dst, src loc)
	movReg(w bool, dst, src Reg)

	// Two-operand integer arithmetic: dst = dst OP src. The *Mem forms read the
	// source (or, for cmp, the second operand) from memory -- a spilled operand's
	// slot -- rather than a register, folding the load into the instruction.
	binGP(op ir.Op, w bool, dst, src Reg)
	binGPMem(op ir.Op, w bool, dst, base Reg, off int32)
	negGP(w bool, dst Reg)
	shiftImmGP(op ir.Op, w bool, dst Reg, imm byte)
	shiftCLGP(op ir.Op, w bool, dst Reg)
	cmpGP(w bool, a, b Reg)
	cmpGPMem(w bool, a, base Reg, off int32)
	setccMovzx(cmp ir.Cmp, dst Reg)
	extGP(op ir.Op, w bool, dst, src Reg)

	// divGP divides RAX (already set up) by divisor, sign-extending into RDX (or
	// zeroing it, unsigned); the quotient lands in RAX, the remainder in RDX.
	divGP(w, signed bool, divisor Reg)

	// Stack pointer: allocNSP grows the stack by a runtime size (VLA) and yields
	// the new top; movFromSP/movToSP snapshot and restore rsp.
	allocNSP(dst, size Reg)
	movFromSP(dst Reg)
	movToSP(src Reg)
	callerPC(dst Reg)
	callerSP(dst Reg)

	// Control flow. Branch targets are blocks so each backend formats its label;
	// jnz tests a register and branches to `to`, else falls through to `to2`.
	jmp(to *ir.Block)
	jnz(r Reg, w bool, to, to2 *ir.Block)
	jcc(cond x64.Cond, to, to2, next *ir.Block)
	jmpReg(r Reg)
	hlt()

	// blockAddrLea materializes a block's RIP-relative address into dst (&&label).
	blockAddrLea(dst Reg, blk *ir.Block)

	// jmpTable emits an indexed branch through a PC-relative offset table placed
	// just past the branch: target = table + (int32)table[idx]. idx is already
	// bounds-checked. R10/R11 are free scratch at a terminator.
	jmpTable(idx Reg, blk *ir.Block, targets []*ir.Block)

	// Calls. callSym is a direct call to a named function (recorded as a PLT32
	// relocation in object code); callReg is an indirect call through a register.
	callSym(sym string, off int64)
	callReg(r Reg)

	// Frame teardown, shared by the return epilogue and the tail-call branch:
	// restoreGP reloads a callee-saved register from its RBP-relative slot,
	// framePop unwinds the frame (mov rsp,rbp; pop rbp), and ret returns.
	restoreGP(r Reg, off int32)
	framePop()
	ret()

	// Floating point: two-operand arithmetic (dst = dst OP src), register move,
	// sign-flip negation, and a compare that sets the boolean result.
	binFP(op ir.Op, dbl bool, dst, src Reg)
	movFP(dbl bool, dst, src Reg)
	fnegFP(dbl bool, dst Reg)
	fcmpSet(cmp ir.Cmp, dbl bool, dst, a, b Reg)

	// Float/int conversions and int<->float bitcasts.
	cvtSS2SD(dst, src Reg)
	cvtSD2SS(dst, src Reg)
	cvtF2SI(w, srcD bool, dst, src Reg)
	cvtSI2F(w, dstD bool, dst, src Reg)

	// The 64-bit unsigned directions, which x86-64 has no single instruction for
	// (arm64 has fcvtzu/ucvtf): each is a compare-and-bias sequence pivoting on
	// 2^63, spelled out at its implementation. cvtUI642F rewrites its source in
	// place, so it must be handed a scratch register.
	cvtF2UI64(srcD bool, dst, src Reg)
	cvtUI642F(dstD bool, dst, src Reg)

	castG2F(dbl bool, dst, src Reg)
	castF2G(long bool, dst, src Reg)

	// Memory. Each method resolves the address (base+disp, RIP-relative symbol, or
	// a computed pointer) internally, so the addressing model stays per-backend.
	loadGP(in *ir.Instr, w bool, dst Reg)
	loadFP(op ir.Op, dst Reg, addr ir.Ref)
	storeGP(in *ir.Instr, val Reg)
	storeFP(op ir.Op, val Reg, addr ir.Ref)

	// spillStore/spillStoreFP write a scratch register back to a result's slot.
	spillStore(r Reg, slot, size int)
	spillStoreFP(r Reg, slot, size int)

	fail(format string, a ...any)
}

// --- machine-code backend --------------------------------------------------

type mcXasm struct{ m *mc }

func (b *mcXasm) refLoc(ref ir.Ref) loc       { return b.m.refLoc(ref) }
func (b *mcXasm) move(dst, src loc)           { b.m.move(dst, src) }
func (b *mcXasm) movReg(w bool, dst, src Reg) { b.m.emit(x64.MovReg(w, dst.mreg(), src.mreg())) }
func (b *mcXasm) binGP(op ir.Op, w bool, dst, src Reg) {
	d, s := dst.mreg(), src.mreg()
	switch op {
	case ir.OAdd:
		b.m.emit(x64.AddReg(w, d, s))
	case ir.OSub:
		b.m.emit(x64.SubReg(w, d, s))
	case ir.OMul:
		b.m.emit(x64.Imul(w, d, s))
	case ir.OAnd:
		b.m.emit(x64.AndReg(w, d, s))
	case ir.OOr:
		b.m.emit(x64.OrReg(w, d, s))
	case ir.OXor:
		b.m.emit(x64.XorReg(w, d, s))
	default:
		b.fail("amd64: %s is not a two-operand arithmetic op", op)
	}
}

// binGPMem is binGP with the source read from memory (a spilled operand's slot)
// rather than a register -- the x86 memory-operand fold, saving a load.
func (b *mcXasm) binGPMem(op ir.Op, w bool, dst, base Reg, off int32) {
	d, src := dst.mreg(), x64.At(base.mreg(), off)
	switch op {
	case ir.OAdd:
		b.m.emit(x64.AddMem(w, d, src))
	case ir.OSub:
		b.m.emit(x64.SubMem(w, d, src))
	case ir.OMul:
		b.m.emit(x64.ImulMem(w, d, src))
	case ir.OAnd:
		b.m.emit(x64.AndMem(w, d, src))
	case ir.OOr:
		b.m.emit(x64.OrMem(w, d, src))
	case ir.OXor:
		b.m.emit(x64.XorMem(w, d, src))
	default:
		b.fail("amd64: %s is not a two-operand arithmetic op", op)
	}
}

// cmpGPMem is cmpGP with the second operand read from memory.
func (b *mcXasm) cmpGPMem(w bool, a, base Reg, off int32) {
	b.m.emit(x64.CmpMem(w, a.mreg(), x64.At(base.mreg(), off)))
}
func (b *mcXasm) negGP(w bool, dst Reg) { b.m.emit(x64.Neg(w, dst.mreg())) }
func (b *mcXasm) shiftImmGP(op ir.Op, w bool, dst Reg, imm byte) {
	d := dst.mreg()
	switch op {
	case ir.OShl:
		b.m.emit(x64.ShlImm(w, d, imm))
	case ir.OShr:
		b.m.emit(x64.ShrImm(w, d, imm))
	case ir.OSar:
		b.m.emit(x64.SarImm(w, d, imm))
	}
}
func (b *mcXasm) shiftCLGP(op ir.Op, w bool, dst Reg) {
	d := dst.mreg()
	switch op {
	case ir.OShl:
		b.m.emit(x64.ShlCL(w, d))
	case ir.OShr:
		b.m.emit(x64.ShrCL(w, d))
	case ir.OSar:
		b.m.emit(x64.SarCL(w, d))
	}
}
func (b *mcXasm) cmpGP(w bool, a, bb Reg) { b.m.emit(x64.CmpReg(w, a.mreg(), bb.mreg())) }
func (b *mcXasm) setccMovzx(cmp ir.Cmp, dst Reg) {
	b.m.emit(x64.Setcc(intCond(cmp), dst.mreg()))
	b.m.emit(x64.MovzxByte(false, dst.mreg(), dst.mreg()))
}

// intCond maps an integer predicate to its x64 condition code (flags from a - b).
func intCond(c ir.Cmp) x64.Cond {
	switch c {
	case ir.CmpEq:
		return x64.E
	case ir.CmpNe:
		return x64.NE
	case ir.CmpSlt:
		return x64.L
	case ir.CmpSle:
		return x64.LE
	case ir.CmpSgt:
		return x64.G
	case ir.CmpSge:
		return x64.GE
	case ir.CmpUlt:
		return x64.B
	case ir.CmpUle:
		return x64.BE
	case ir.CmpUgt:
		return x64.A
	case ir.CmpUge:
		return x64.AE
	}
	return x64.E
}

// intCC maps an integer predicate to its AT&T condition suffix (flags from a - b).
var intCC = map[ir.Cmp]string{
	ir.CmpEq: "e", ir.CmpNe: "ne", ir.CmpSlt: "l", ir.CmpSle: "le",
	ir.CmpSgt: "g", ir.CmpSge: "ge", ir.CmpUlt: "b", ir.CmpUle: "be",
	ir.CmpUgt: "a", ir.CmpUge: "ae",
}

func (b *mcXasm) extGP(op ir.Op, w bool, dst, src Reg) {
	dm, sm := dst.mreg(), src.mreg()
	switch op {
	case ir.OExtsb:
		b.m.emit(x64.MovsxByte(w, dm, sm))
	case ir.OExtub:
		b.m.emit(x64.MovzxByte(w, dm, sm))
	case ir.OExtsh:
		b.m.emit(x64.MovsxWord(w, dm, sm))
	case ir.OExtuh:
		b.m.emit(x64.MovzxWord(w, dm, sm))
	case ir.OExtsw:
		b.m.emit(x64.Movsxd(dm, sm))
	case ir.OExtuw:
		b.m.emit(x64.MovReg(false, dm, sm)) // a 32-bit mov zero-extends
	}
}
func (b *mcXasm) allocNSP(dst, size Reg) {
	b.m.emit(x64.SubReg(true, RSP.mreg(), size.mreg()))
	b.m.emit(x64.MovReg(true, dst.mreg(), RSP.mreg()))
}
func (b *mcXasm) movFromSP(dst Reg) { b.m.emit(x64.MovReg(true, dst.mreg(), RSP.mreg())) }
func (b *mcXasm) movToSP(src Reg)   { b.m.emit(x64.MovReg(true, RSP.mreg(), src.mreg())) }
func (b *mcXasm) callerPC(dst Reg)  { b.m.emit(x64.Load(true, dst.mreg(), x64.At(RBP.mreg(), 8))) }
func (b *mcXasm) callerSP(dst Reg)  { b.m.emit(x64.Lea(true, dst.mreg(), x64.At(RBP.mreg(), 16))) }
func (b *mcXasm) jmp(to *ir.Block)  { b.m.prog.Jmp(to.Name) }
func (b *mcXasm) jnz(r Reg, w bool, to, to2 *ir.Block) {
	b.m.emit(x64.TestReg(w, r.mreg(), r.mreg()))
	b.m.prog.Jcc(x64.NE, to.Name)
	b.m.prog.Jmp(to2.Name)
}

// jcc branches on the flags a preceding cmp set: to `to` when cond holds, else to
// `to2` -- with the fall-through to `to2` elided when it is the next block.
func (b *mcXasm) jcc(cond x64.Cond, to, to2, next *ir.Block) {
	b.m.prog.Jcc(cond, to.Name)
	if to2 != next {
		b.m.prog.Jmp(to2.Name)
	}
}
func (b *mcXasm) jmpReg(r Reg) { b.m.emit(x64.JmpReg(r.mreg())) }
func (b *mcXasm) hlt()         { b.m.emit(x64.Ud2()) }
func (b *mcXasm) blockAddrLea(dst Reg, blk *ir.Block) {
	b.m.prog.LeaLabel(true, dst.mreg(), blk.Name)
}
func (b *mcXasm) jmpTable(idx Reg, blk *ir.Block, targets []*ir.Block) {
	tbl := blk.Name + ".tbl"
	b.m.prog.LeaLabel(true, gpScratch0.mreg(), tbl) // lea R10, [rip+tbl]
	b.m.emit(x64.MovsxdLoad(gpScratch1.mreg(), x64.Mem{Base: gpScratch0.mreg(), Index: idx.mreg(), Scale: 4, HasIndex: true}))
	b.m.emit(x64.AddReg(true, gpScratch0.mreg(), gpScratch1.mreg())) // add R10, R11
	b.m.emit(x64.JmpReg(gpScratch0.mreg()))                          // jmp *R10
	b.m.prog.Label(tbl)
	for _, t := range targets {
		b.m.prog.DataWord(t.Name, tbl) // .long t - tbl
	}
}
func (b *mcXasm) callSym(sym string, off int64) {
	b.m.emit(x64.CallRel(0))
	b.m.recordReloc(b.m.prog.Len()-4, sym, obj.R_X86_64_PLT32, off-4)
}
func (b *mcXasm) callReg(r Reg) { b.m.emit(x64.CallReg(r.mreg())) }
func (b *mcXasm) restoreGP(r Reg, off int32) {
	b.m.emit(x64.Load(true, r.mreg(), x64.At(RBP.mreg(), off)))
}
func (b *mcXasm) framePop() {
	b.m.emit(x64.MovReg(true, RSP.mreg(), RBP.mreg()))
	b.m.emit(x64.Pop(RBP.mreg()))
}
func (b *mcXasm) ret() { b.m.emit(x64.Ret()) }
func (b *mcXasm) divGP(w, signed bool, divisor Reg) {
	if signed {
		if w {
			b.m.emit(x64.Cqo())
		} else {
			b.m.emit(x64.Cdq())
		}
		b.m.emit(x64.Idiv(w, divisor.mreg()))
	} else {
		b.m.emit(x64.XorReg(w, RDX.mreg(), RDX.mreg()))
		b.m.emit(x64.Div(w, divisor.mreg()))
	}
}
func (b *mcXasm) binFP(op ir.Op, dbl bool, dst, src Reg) {
	dm, rm := dst.mreg(), src.mreg()
	switch {
	case op == ir.OAdd && dbl:
		b.m.emit(x64.Addsd(dm, rm))
	case op == ir.OAdd:
		b.m.emit(x64.Addss(dm, rm))
	case op == ir.OSub && dbl:
		b.m.emit(x64.Subsd(dm, rm))
	case op == ir.OSub:
		b.m.emit(x64.Subss(dm, rm))
	case op == ir.OMul && dbl:
		b.m.emit(x64.Mulsd(dm, rm))
	case op == ir.OMul:
		b.m.emit(x64.Mulss(dm, rm))
	case op == ir.ODiv && dbl:
		b.m.emit(x64.Divsd(dm, rm))
	case op == ir.ODiv:
		b.m.emit(x64.Divss(dm, rm))
	default:
		b.fail("amd64: unsupported float op %s", op)
	}
}
func (b *mcXasm) movFP(dbl bool, dst, src Reg) {
	if dbl {
		b.m.emit(x64.MovsdReg(dst.mreg(), src.mreg()))
	} else {
		b.m.emit(x64.MovssReg(dst.mreg(), src.mreg()))
	}
}
func (b *mcXasm) fnegFP(dbl bool, dst Reg) {
	if dbl {
		b.m.movImm(gpScratch0, int64(-9223372036854775808), true) // 0x8000000000000000
		b.m.emit(x64.MovqToXmm(true, fpScratch1.mreg(), gpScratch0.mreg()))
		b.m.emit(x64.Xorpd(dst.mreg(), fpScratch1.mreg()))
	} else {
		b.m.movImm(gpScratch0, 0x80000000, false)
		b.m.emit(x64.MovqToXmm(false, fpScratch1.mreg(), gpScratch0.mreg()))
		b.m.emit(x64.Xorps(dst.mreg(), fpScratch1.mreg()))
	}
}
func (b *mcXasm) fcmpSet(cmp ir.Cmp, dbl bool, dst, a, bb Reg) {
	dm := dst.mreg()
	ucomi := func(x, y Reg) {
		if dbl {
			b.m.emit(x64.Ucomisd(x.mreg(), y.mreg()))
		} else {
			b.m.emit(x64.Ucomiss(x.mreg(), y.mreg()))
		}
	}
	switch cmp {
	case ir.CmpFgt:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.A, dm))
	case ir.CmpFge:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.AE, dm))
	case ir.CmpFlt:
		ucomi(bb, a)
		b.m.emit(x64.Setcc(x64.A, dm))
	case ir.CmpFle:
		ucomi(bb, a)
		b.m.emit(x64.Setcc(x64.AE, dm))
	case ir.CmpFo:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.NP, dm))
	case ir.CmpFuo:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.P, dm))
	case ir.CmpFeq:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.NP, gpScratch0.mreg()))
		b.m.emit(x64.Setcc(x64.E, dm))
		b.m.emit(x64.AndReg(false, dm, gpScratch0.mreg()))
	case ir.CmpFne:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.P, gpScratch0.mreg()))
		b.m.emit(x64.Setcc(x64.NE, dm))
		b.m.emit(x64.OrReg(false, dm, gpScratch0.mreg()))
	default:
		b.fail("amd64: unsupported float compare %v", cmp)
	}
	b.m.emit(x64.MovzxByte(false, dm, dm))
}
func (b *mcXasm) cvtSS2SD(dst, src Reg) { b.m.emit(x64.Cvtss2sd(dst.mreg(), src.mreg())) }
func (b *mcXasm) cvtSD2SS(dst, src Reg) { b.m.emit(x64.Cvtsd2ss(dst.mreg(), src.mreg())) }
func (b *mcXasm) cvtF2SI(w, srcD bool, dst, src Reg) {
	if srcD {
		b.m.emit(x64.Cvttsd2si(w, dst.mreg(), src.mreg()))
	} else {
		b.m.emit(x64.Cvttss2si(w, dst.mreg(), src.mreg()))
	}
}
func (b *mcXasm) cvtSI2F(w, dstD bool, dst, src Reg) {
	if dstD {
		b.m.emit(x64.Cvtsi2sd(w, dst.mreg(), src.mreg()))
	} else {
		b.m.emit(x64.Cvtsi2ss(w, dst.mreg(), src.mreg()))
	}
}

// 2^63 as a single- and a double-precision bit pattern. It is the pivot both
// unsigned 64-bit conversions turn on, and it is exactly representable in either
// width (its mantissa is zero), which is what makes the bias steps below exact.
const (
	biasF32Bits = 0x5f000000
	biasF64Bits = 0x43e0000000000000
)

// cvtF2UI64 truncates a float towards zero into an unsigned 64-bit integer.
//
// x86-64 only has the signed truncation CVTTS{S,D}2SI (arm64 has a native
// fcvtzu). A source at or above 2^63 has no int64 to land in, so the instruction
// yields the "integer indefinite" value 0x8000000000000000 instead of the value
// asked for -- which is exactly the range an unsigned result cares about. The
// sequence therefore pivots on 2^63: below it the signed truncation is already
// the right answer, and at or above it the source is biased down by 2^63 before
// truncating and the 2^63 is put back into the integer result afterwards. Both
// halves are exact: a float at or above 2^63 has no fractional bits left, so
// subtracting 2^63 discards nothing, and the remainder is below 2^63 and so does
// fit an int64.
//
//	movabs r11, <2^63 as a float>
//	movq   xmm14, r11
//	ucomis{s,d} src, xmm14   ; CF is set when src < 2^63 -- and when src is NaN,
//	jb     direct            ; which wants the signed instruction's own answer
//	movs{s,d} xmm15, src     ; src can be a live value, so bias a copy of it
//	subs{s,d} xmm15, xmm14
//	cvtts{s,d}2si dst, xmm15 ; the remainder, which is in [0, 2^63)
//	movabs r11, 1<<63
//	add    dst, r11          ; put the subtracted 2^63 back
//	jmp    done
//	direct:
//	cvtts{s,d}2si dst, src
//	done:
//
// This clobbers R11 and both XMM scratch registers. dst is never R11 -- gpDst
// hands out either an allocated register or R10 -- so parking the two constants
// there cannot collide with the result being built.
func (b *mcXasm) cvtF2UI64(srcD bool, dst, src Reg) {
	direct, done := b.seqLabels(".f2u64")

	bias := int64(biasF32Bits)
	if srcD {
		bias = biasF64Bits
	}
	b.m.movImm(gpScratch1, bias, srcD)
	b.m.emit(x64.MovqToXmm(srcD, fpScratch0.mreg(), gpScratch1.mreg()))
	if srcD {
		b.m.emit(x64.Ucomisd(src.mreg(), fpScratch0.mreg()))
	} else {
		b.m.emit(x64.Ucomiss(src.mreg(), fpScratch0.mreg()))
	}
	b.m.prog.Jcc(x64.B, direct)

	if src != fpScratch1 {
		b.movFP(srcD, fpScratch1, src)
	}
	if srcD {
		b.m.emit(x64.Subsd(fpScratch1.mreg(), fpScratch0.mreg()))
	} else {
		b.m.emit(x64.Subss(fpScratch1.mreg(), fpScratch0.mreg()))
	}
	b.cvtF2SI(true, srcD, dst, fpScratch1)
	b.m.movImm(gpScratch1, math.MinInt64, true) // 1<<63, the bias as an integer
	b.m.emit(x64.AddReg(true, dst.mreg(), gpScratch1.mreg()))
	b.m.prog.Jmp(done)

	b.m.prog.Label(direct)
	b.cvtF2SI(true, srcD, dst, src)
	b.m.prog.Label(done)
}

// cvtUI642F converts an unsigned 64-bit integer to a float.
//
// CVTSI2S{S,D} is signed (arm64 has a native ucvtf), so a source with its top bit
// set converts to a *negative* float. A source that fits an int64 needs nothing
// special; otherwise the value is halved so that it does fit, converted, and
// doubled back:
//
//	test src, src
//	jns  direct
//	mov  r10, src
//	and  r10, 1        ; the bit the shift is about to drop
//	shr  src, 1
//	or   src, r10      ; keep the halved value odd when a bit was dropped
//	cvtsi2s{s,d} dst, src
//	adds{s,d} dst, dst ; doubling is exact, so no second rounding happens here
//	jmp  done
//	direct:
//	cvtsi2s{s,d} dst, src
//	done:
//
// The OR is what makes the halving round correctly rather than merely closely.
// Shifting right throws bit 0 away, and converting the halved value then rounds
// to nearest-even -- so a value sitting exactly halfway between two
// representable results would break the tie on the halved value, which is not
// where the tie is. Re-inserting the dropped bit keeps the halved value odd
// whenever anything was lost, which removes the false tie. Without it
// 2^63+1025 converts to 9223372036854775808 rather than the correctly rounded
// 9223372036854777856.
//
// src must be a scratch register, because the halving rewrites it in place. This
// also clobbers R10.
func (b *mcXasm) cvtUI642F(dstD bool, dst, src Reg) {
	direct, done := b.seqLabels(".u642f")

	b.m.emit(x64.TestReg(true, src.mreg(), src.mreg()))
	b.m.prog.Jcc(x64.NS, direct)

	b.m.emit(x64.MovReg(true, gpScratch0.mreg(), src.mreg()))
	b.m.emit(x64.AndImm(true, gpScratch0.mreg(), 1))
	b.m.emit(x64.ShrImm(true, src.mreg(), 1))
	b.m.emit(x64.OrReg(true, src.mreg(), gpScratch0.mreg()))
	b.cvtSI2F(true, dstD, dst, src)
	if dstD {
		b.m.emit(x64.Addsd(dst.mreg(), dst.mreg()))
	} else {
		b.m.emit(x64.Addss(dst.mreg(), dst.mreg()))
	}
	b.m.prog.Jmp(done)

	b.m.prog.Label(direct)
	b.cvtSI2F(true, dstD, dst, src)
	b.m.prog.Label(done)
}

// seqLabels names the two local labels the conversion sequences above need: the
// fast path that needs no adjustment, and the join point after the adjusted one.
// Program labels have to be unique within the function and there is no
// instruction counter to number them by, so the byte offset the sequence starts
// at does the naming: offsets only ever grow, and every sequence emits code, so
// no two can start at the same one. The leading dot keeps them out of the way of
// block labels, which are named by the IR.
func (b *mcXasm) seqLabels(kind string) (direct, done string) {
	tag := fmt.Sprintf("%s.%d", kind, b.m.prog.Len())
	return tag + ".direct", tag + ".done"
}
func (b *mcXasm) castG2F(dbl bool, dst, src Reg) {
	b.m.emit(x64.MovqToXmm(dbl, dst.mreg(), src.mreg()))
}
func (b *mcXasm) castF2G(long bool, dst, src Reg) {
	b.m.emit(x64.MovqFromXmm(long, dst.mreg(), src.mreg()))
}
func (b *mcXasm) loadGP(in *ir.Instr, w bool, dst Reg) {
	op := in.Op
	mem, fixup := b.m.memFor(in, 0)
	dm := dst.mreg()
	switch op {
	case ir.OLoadub:
		b.m.emit(x64.MovzxLoadByte(w, dm, mem))
	case ir.OLoadsb:
		b.m.emit(x64.MovsxLoadByte(w, dm, mem))
	case ir.OLoaduh:
		b.m.emit(x64.MovzxLoadWord(w, dm, mem))
	case ir.OLoadsh:
		b.m.emit(x64.MovsxLoadWord(w, dm, mem))
	case ir.OLoaduw:
		b.m.emit(x64.Load(false, dm, mem)) // a 32-bit load zero-extends
	case ir.OLoadsw:
		b.m.emit(x64.MovsxdLoad(dm, mem))
	case ir.OLoadl:
		b.m.emit(x64.Load(true, dm, mem))
	}
	fixup()
}
func (b *mcXasm) loadFP(op ir.Op, dst Reg, addr ir.Ref) {
	mem, fixup := b.m.memAddr(addr, gpScratch1)
	if op == ir.OLoadd {
		b.m.emit(x64.MovsdLoad(dst.mreg(), mem))
	} else {
		b.m.emit(x64.MovssLoad(dst.mreg(), mem))
	}
	fixup()
}
func (b *mcXasm) storeGP(in *ir.Instr, val Reg) {
	mem, fixup := b.m.memFor(in, 1)
	sz := map[ir.Op]int{ir.OStoreb: 8, ir.OStoreh: 16, ir.OStorew: 32, ir.OStorel: 64}[in.Op]
	b.m.emit(x64.Store(sz, val.mreg(), mem))
	fixup()
}
func (b *mcXasm) storeFP(op ir.Op, val Reg, addr ir.Ref) {
	mem, fixup := b.m.memAddr(addr, gpScratch1)
	if op == ir.OStored {
		b.m.emit(x64.MovsdStore(val.mreg(), mem))
	} else {
		b.m.emit(x64.MovssStore(val.mreg(), mem))
	}
	fixup()
}
func (b *mcXasm) spillStore(r Reg, slot, size int) {
	b.m.emit(x64.Store(size*8, r.mreg(), x64.At(RBP.mreg(), b.m.slotAddr(slot))))
}
func (b *mcXasm) spillStoreFP(r Reg, slot, size int) {
	mem := x64.At(RBP.mreg(), b.m.slotAddr(slot))
	if size == 8 {
		b.m.emit(x64.MovsdStore(r.mreg(), mem))
	} else {
		b.m.emit(x64.MovssStore(r.mreg(), mem))
	}
}
func (b *mcXasm) fail(format string, a ...any) { b.m.fail(fmt.Errorf(format, a...)) }
