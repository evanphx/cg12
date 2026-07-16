package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// xasm is the amd64 instruction builder: an x86-idiomatic assembler surface whose
// operations mirror the x64 encoders. Two backends implement it -- mcXasm encodes
// each instruction into machine code, textXasm renders it as an AT&T assembly
// line -- so the instruction selection that drives it (xsel) is written once and
// serves both the object and the assembly-text outputs, the same way asmb unifies
// the arm64 emitters.
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

	// Two-operand integer arithmetic: dst = dst OP src.
	binGP(op ir.Op, w bool, dst, src Reg)
	negGP(w bool, dst Reg)
	shiftImmGP(op ir.Op, w bool, dst Reg, imm byte)
	shiftCLGP(op ir.Op, w bool, dst Reg)
	cmpGP(w bool, a, b Reg)
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

	// Control flow. Branch targets are blocks so each backend formats its label;
	// jnz tests a register and branches to `to`, else falls through to `to2`.
	jmp(to *ir.Block)
	jnz(r Reg, w bool, to, to2 *ir.Block)
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
	castG2F(dbl bool, dst, src Reg)
	castF2G(long bool, dst, src Reg)

	// Memory. Each method resolves the address (base+disp, RIP-relative symbol, or
	// a computed pointer) internally, so the addressing model stays per-backend.
	loadGP(op ir.Op, w bool, dst Reg, addr ir.Ref)
	loadFP(op ir.Op, dst Reg, addr ir.Ref)
	storeGP(op ir.Op, val Reg, addr ir.Ref)
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
func (b *mcXasm) jmp(to *ir.Block)  { b.m.prog.Jmp(to.Name) }
func (b *mcXasm) jnz(r Reg, w bool, to, to2 *ir.Block) {
	b.m.emit(x64.TestReg(w, r.mreg(), r.mreg()))
	b.m.prog.Jcc(x64.NE, to.Name)
	b.m.prog.Jmp(to2.Name)
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
func (b *mcXasm) castG2F(dbl bool, dst, src Reg) {
	b.m.emit(x64.MovqToXmm(dbl, dst.mreg(), src.mreg()))
}
func (b *mcXasm) castF2G(long bool, dst, src Reg) {
	b.m.emit(x64.MovqFromXmm(long, dst.mreg(), src.mreg()))
}
func (b *mcXasm) loadGP(op ir.Op, w bool, dst Reg, addr ir.Ref) {
	mem, fixup := b.m.memAddr(addr, gpScratch1)
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
func (b *mcXasm) storeGP(op ir.Op, val Reg, addr ir.Ref) {
	mem, fixup := b.m.memAddr(addr, gpScratch1)
	sz := map[ir.Op]int{ir.OStoreb: 8, ir.OStoreh: 16, ir.OStorew: 32, ir.OStorel: 64}[op]
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

// --- text backend ----------------------------------------------------------

type textXasm struct{ e *emitter }

func (b *textXasm) refLoc(ref ir.Ref) loc { return b.e.refLoc(ref) }
func (b *textXasm) move(dst, src loc)     { b.e.move(dst, src) }
func (b *textXasm) movReg(w bool, dst, src Reg) {
	b.e.line("mov%s %s, %s", suf(clsSizeW(w)), gpn(src, clsSizeW(w)), gpn(dst, clsSizeW(w)))
}
func (b *textXasm) binGP(op ir.Op, w bool, dst, src Reg) {
	sz := clsSizeW(w)
	if op == ir.OMul {
		b.e.line("imul%s %s, %s", suf(sz), gpn(src, sz), gpn(dst, sz))
		return
	}
	mn := map[ir.Op]string{ir.OAdd: "add", ir.OSub: "sub", ir.OAnd: "and", ir.OOr: "or", ir.OXor: "xor"}[op]
	if mn == "" {
		b.fail("amd64: %s is not a two-operand arithmetic op", op)
		return
	}
	b.e.line("%s%s %s, %s", mn, suf(sz), gpn(src, sz), gpn(dst, sz))
}
func (b *textXasm) negGP(w bool, dst Reg) {
	sz := clsSizeW(w)
	b.e.line("neg%s %s", suf(sz), gpn(dst, sz))
}
func (b *textXasm) shiftImmGP(op ir.Op, w bool, dst Reg, imm byte) {
	sz := clsSizeW(w)
	b.e.line("%s%s $%d, %s", shiftMn[op], suf(sz), imm, gpn(dst, sz))
}
func (b *textXasm) shiftCLGP(op ir.Op, w bool, dst Reg) {
	sz := clsSizeW(w)
	b.e.line("%s%s %%cl, %s", shiftMn[op], suf(sz), gpn(dst, sz))
}
func (b *textXasm) cmpGP(w bool, a, bb Reg) {
	sz := clsSizeW(w)
	b.e.line("cmp%s %s, %s", suf(sz), gpn(bb, sz), gpn(a, sz))
}
func (b *textXasm) setccMovzx(cmp ir.Cmp, dst Reg) {
	b.e.line("set%s %s", intCC[cmp], gpn(dst, 1))
	b.e.line("movzbl %s, %s", gpn(dst, 1), gpn(dst, 4))
}
func (b *textXasm) extGP(op ir.Op, w bool, dst, src Reg) {
	dw, tail := 4, "l"
	if w {
		dw, tail = 8, "q"
	}
	switch op {
	case ir.OExtsb:
		b.e.line("movsb%s %s, %s", tail, gpn(src, 1), gpn(dst, dw))
	case ir.OExtub:
		b.e.line("movzb%s %s, %s", tail, gpn(src, 1), gpn(dst, dw))
	case ir.OExtsh:
		b.e.line("movsw%s %s, %s", tail, gpn(src, 2), gpn(dst, dw))
	case ir.OExtuh:
		b.e.line("movzw%s %s, %s", tail, gpn(src, 2), gpn(dst, dw))
	case ir.OExtsw:
		b.e.line("movslq %s, %s", gpn(src, 4), gpn(dst, 8))
	case ir.OExtuw:
		b.e.line("movl %s, %s", gpn(src, 4), gpn(dst, 4))
	}
}
func (b *textXasm) allocNSP(dst, size Reg) {
	b.e.line("subq %s, %%rsp", gpn(size, 8))
	b.e.line("movq %%rsp, %s", gpn(dst, 8))
}
func (b *textXasm) movFromSP(dst Reg) { b.e.line("movq %%rsp, %s", gpn(dst, 8)) }
func (b *textXasm) movToSP(src Reg)   { b.e.line("movq %s, %%rsp", gpn(src, 8)) }
func (b *textXasm) jmp(to *ir.Block)  { b.e.line("jmp %s", b.e.blabel(to)) }
func (b *textXasm) jnz(r Reg, w bool, to, to2 *ir.Block) {
	sz := clsSizeW(w)
	b.e.line("test%s %s, %s", suf(sz), gpn(r, sz), gpn(r, sz))
	b.e.line("jne %s", b.e.blabel(to))
	b.e.line("jmp %s", b.e.blabel(to2))
}
func (b *textXasm) jmpReg(r Reg) { b.e.line("jmp *%s", gpn(r, 8)) }
func (b *textXasm) hlt()         { b.e.line("ud2") }
func (b *textXasm) blockAddrLea(dst Reg, blk *ir.Block) {
	b.e.line("leaq %s(%%rip), %s", b.e.blabel(blk), gpn(dst, 8))
}
func (b *textXasm) jmpTable(idx Reg, blk *ir.Block, targets []*ir.Block) {
	tbl := b.e.blabel(blk) + "_tbl"
	b.e.line("leaq %s(%%rip), %s", tbl, gpn(gpScratch0, 8))
	b.e.line("movslq (%s,%s,4), %s", gpn(gpScratch0, 8), gpn(idx, 8), gpn(gpScratch1, 8))
	b.e.line("addq %s, %s", gpn(gpScratch1, 8), gpn(gpScratch0, 8))
	b.e.line("jmp *%s", gpn(gpScratch0, 8))
	b.e.line("%s:", tbl)
	for _, t := range targets {
		b.e.line(".long %s - %s", b.e.blabel(t), tbl)
	}
}
func (b *textXasm) callSym(sym string, off int64) { b.e.line("call %s", sanitize(sym)) }
func (b *textXasm) callReg(r Reg)                 { b.e.line("call *%s", gpn(r, 8)) }
func (b *textXasm) restoreGP(r Reg, off int32)    { b.e.line("movq %s, %s", memn(RBP, off), gpn(r, 8)) }
func (b *textXasm) framePop() {
	b.e.line("movq %%rbp, %%rsp")
	b.e.line("popq %%rbp")
}
func (b *textXasm) ret() { b.e.line("ret") }
func (b *textXasm) divGP(w, signed bool, divisor Reg) {
	sz := clsSizeW(w)
	if signed {
		if w {
			b.e.line("cqto")
		} else {
			b.e.line("cltd")
		}
		b.e.line("idiv%s %s", suf(sz), gpn(divisor, sz))
	} else {
		b.e.line("xorl %%edx, %%edx")
		b.e.line("div%s %s", suf(sz), gpn(divisor, sz))
	}
}
func (b *textXasm) binFP(op ir.Op, dbl bool, dst, src Reg) {
	base := map[ir.Op]string{ir.OAdd: "add", ir.OSub: "sub", ir.OMul: "mul", ir.ODiv: "div"}[op]
	if base == "" {
		b.fail("amd64: unsupported float op %s", op)
		return
	}
	sfx := "ss"
	if dbl {
		sfx = "sd"
	}
	b.e.line("%s%s %s, %s", base, sfx, xmmn(src), xmmn(dst))
}
func (b *textXasm) movFP(dbl bool, dst, src Reg) {
	mn := "movss"
	if dbl {
		mn = "movsd"
	}
	b.e.line("%s %s, %s", mn, xmmn(src), xmmn(dst))
}
func (b *textXasm) fnegFP(dbl bool, dst Reg) {
	if dbl {
		b.e.line("movabsq $0x8000000000000000, %s", gpn(gpScratch0, 8))
		b.e.line("movq %s, %s", gpn(gpScratch0, 8), xmmn(fpScratch1))
		b.e.line("xorpd %s, %s", xmmn(fpScratch1), xmmn(dst))
	} else {
		b.e.line("movl $0x80000000, %s", gpn(gpScratch0, 4))
		b.e.line("movd %s, %s", gpn(gpScratch0, 4), xmmn(fpScratch1))
		b.e.line("xorps %s, %s", xmmn(fpScratch1), xmmn(dst))
	}
}
func (b *textXasm) fcmpSet(cmp ir.Cmp, dbl bool, dst, a, bb Reg) {
	d8 := gpn(dst, 1)
	ucomi := func(x, y Reg) {
		mn := "ucomiss"
		if dbl {
			mn = "ucomisd"
		}
		b.e.line("%s %s, %s", mn, xmmn(y), xmmn(x))
	}
	switch cmp {
	case ir.CmpFgt:
		ucomi(a, bb)
		b.e.line("seta %s", d8)
	case ir.CmpFge:
		ucomi(a, bb)
		b.e.line("setae %s", d8)
	case ir.CmpFlt:
		ucomi(bb, a)
		b.e.line("seta %s", d8)
	case ir.CmpFle:
		ucomi(bb, a)
		b.e.line("setae %s", d8)
	case ir.CmpFo:
		ucomi(a, bb)
		b.e.line("setnp %s", d8)
	case ir.CmpFuo:
		ucomi(a, bb)
		b.e.line("setp %s", d8)
	case ir.CmpFeq:
		ucomi(a, bb)
		b.e.line("setnp %s", gpn(gpScratch0, 1))
		b.e.line("sete %s", d8)
		b.e.line("andb %s, %s", gpn(gpScratch0, 1), d8)
	case ir.CmpFne:
		ucomi(a, bb)
		b.e.line("setp %s", gpn(gpScratch0, 1))
		b.e.line("setne %s", d8)
		b.e.line("orb %s, %s", gpn(gpScratch0, 1), d8)
	default:
		b.fail("amd64: unsupported float compare %v", cmp)
	}
	b.e.line("movzbl %s, %s", d8, gpn(dst, 4))
}
func (b *textXasm) cvtSS2SD(dst, src Reg) { b.e.line("cvtss2sd %s, %s", xmmn(src), xmmn(dst)) }
func (b *textXasm) cvtSD2SS(dst, src Reg) { b.e.line("cvtsd2ss %s, %s", xmmn(src), xmmn(dst)) }
func (b *textXasm) cvtF2SI(w, srcD bool, dst, src Reg) {
	mn := "cvttss2si"
	if srcD {
		mn = "cvttsd2si"
	}
	b.e.line("%s %s, %s", mn, xmmn(src), gpn(dst, clsSizeW(w)))
}
func (b *textXasm) cvtSI2F(w, dstD bool, dst, src Reg) {
	mn := "cvtsi2ss"
	if dstD {
		mn = "cvtsi2sd"
	}
	if w {
		mn += "q"
	} else {
		mn += "l"
	}
	b.e.line("%s %s, %s", mn, gpn(src, clsSizeW(w)), xmmn(dst))
}
func (b *textXasm) castG2F(dbl bool, dst, src Reg) {
	mn, sz := "movd", 4
	if dbl {
		mn, sz = "movq", 8
	}
	b.e.line("%s %s, %s", mn, gpn(src, sz), xmmn(dst))
}
func (b *textXasm) castF2G(long bool, dst, src Reg) {
	mn, sz := "movd", 4
	if long {
		mn, sz = "movq", 8
	}
	b.e.line("%s %s, %s", mn, xmmn(src), gpn(dst, sz))
}
func (b *textXasm) loadGP(op ir.Op, w bool, dst Reg, addr ir.Ref) {
	mem := b.e.memAddr(addr, gpScratch1)
	dw, tail := 4, "l"
	if w {
		dw, tail = 8, "q"
	}
	switch op {
	case ir.OLoadub:
		b.e.line("movzb%s %s, %s", tail, mem, gpn(dst, dw))
	case ir.OLoadsb:
		b.e.line("movsb%s %s, %s", tail, mem, gpn(dst, dw))
	case ir.OLoaduh:
		b.e.line("movzw%s %s, %s", tail, mem, gpn(dst, dw))
	case ir.OLoadsh:
		b.e.line("movsw%s %s, %s", tail, mem, gpn(dst, dw))
	case ir.OLoaduw:
		b.e.line("movl %s, %s", mem, gpn(dst, 4)) // a 32-bit load zero-extends
	case ir.OLoadsw:
		b.e.line("movslq %s, %s", mem, gpn(dst, 8))
	case ir.OLoadl:
		b.e.line("movq %s, %s", mem, gpn(dst, 8))
	}
}
func (b *textXasm) loadFP(op ir.Op, dst Reg, addr ir.Ref) {
	mem := b.e.memAddr(addr, gpScratch1)
	mn := "movss"
	if op == ir.OLoadd {
		mn = "movsd"
	}
	b.e.line("%s %s, %s", mn, mem, xmmn(dst))
}
func (b *textXasm) storeGP(op ir.Op, val Reg, addr ir.Ref) {
	mem := b.e.memAddr(addr, gpScratch1)
	size := map[ir.Op]int{ir.OStoreb: 1, ir.OStoreh: 2, ir.OStorew: 4, ir.OStorel: 8}[op]
	b.e.line("mov%s %s, %s", suf(size), gpn(val, size), mem)
}
func (b *textXasm) storeFP(op ir.Op, val Reg, addr ir.Ref) {
	mem := b.e.memAddr(addr, gpScratch1)
	mn := "movss"
	if op == ir.OStored {
		mn = "movsd"
	}
	b.e.line("%s %s, %s", mn, xmmn(val), mem)
}
func (b *textXasm) spillStore(r Reg, slot, size int) {
	b.e.line("mov%s %s, %s", suf(size), gpn(r, size), memn(RBP, b.e.slotAddr(slot)))
}
func (b *textXasm) spillStoreFP(r Reg, slot, size int) {
	mn := "movss"
	if size == 8 {
		mn = "movsd"
	}
	b.e.line("%s %s, %s", mn, xmmn(r), memn(RBP, b.e.slotAddr(slot)))
}

var shiftMn = map[ir.Op]string{ir.OShl: "shl", ir.OShr: "shr", ir.OSar: "sar"}

func (b *textXasm) fail(format string, a ...any) { panic(fmt.Sprintf(format, a...)) }

// clsSizeW returns the operand byte size for the w (64-bit) flag.
func clsSizeW(w bool) int {
	if w {
		return 8
	}
	return 4
}
