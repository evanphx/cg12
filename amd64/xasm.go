package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
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

	// spillStore writes a scratch register back to a result's frame slot.
	spillStore(r Reg, slot, size int)

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
func (b *mcXasm) spillStore(r Reg, slot, size int) {
	b.m.emit(x64.Store(size*8, r.mreg(), x64.At(RBP.mreg(), b.m.slotAddr(slot))))
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
func (b *textXasm) spillStore(r Reg, slot, size int) {
	b.e.line("mov%s %s, %s", suf(size), gpn(r, size), memn(RBP, b.e.slotAddr(slot)))
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
