package amd64

import (
	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// xasmInt is integer computation in general-purpose registers: the two-operand
// arithmetic, the shifts, the compare-and-materialize pair, the width extensions,
// and the divide.
type xasmInt interface {
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
}

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
