package amd64

import (
	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// xasmImm is the immediate-operand surface: the same arithmetic and comparison
// xasmInt already encodes, with the second operand spelled as a constant field of
// the instruction rather than as a register the selector had to fill first. It
// also carries CMOVcc, which is here rather than in xasmInt because the only op
// that reaches it -- the conditional select -- is claimed by the same family.
//
// x86-64's immediate forms are near-total for the ops cg12 produces, so what is
// absent is a short list. There is no TEST-immediate here: nothing selects a
// flags-only mask test (arm64's tst comes from its own fuseCmpSel, which has no
// amd64 counterpart). There is no memory-destination immediate form (ADD [mem],
// imm), because every result of an amd64 selection lands in a register and is
// committed by the selector's own spill store. And there is no immediate form of
// SHL/SHR/SAR here because xasmInt already has one -- shiftImmGP predates this
// file and is the one immediate the backend always used.
//
// Nothing folds an immediate into the *first* operand, because x86-64 cannot: the
// 81/83/69 encodings put the constant where the second operand goes. immBinOperands
// commutes the operands where the op allows it; `2 - x` stays a register subtract.
type xasmImm interface {
	// binImmGP computes dst = dst OP imm in place, for the ops with an 81/83
	// encoding (add, sub, and, or, xor). The instruction chooses the sign-extended
	// imm8 form for a small constant on its own, so the caller need not.
	binImmGP(op ir.Op, w bool, dst Reg, imm int32)

	// imulImmGP computes dst = src * imm. Unlike the ops above this one is a true
	// three-operand instruction (69 /r id), so src is only read: the selector does
	// not have to copy it into dst first, and dst may equal src.
	imulImmGP(w bool, dst, src Reg, imm int32)

	// cmpImmGP sets the flags from a - imm, the flags-only compare against a
	// constant that both the materialized boolean and the fused conditional branch
	// are built on.
	cmpImmGP(w bool, a Reg, imm int32)

	// selGP writes src into dst when cond is non-zero and leaves dst unchanged
	// otherwise -- the branchless form of `dst = cond ? src : dst`.
	//
	// The two instructions are one method because the second reads the flags the
	// first writes and nothing may come between them. condW gives the width of the
	// condition's own class, which is not the width of the values being selected.
	selGP(w, condW bool, dst, src, cond Reg)
}

func (b *mcXasm) binImmGP(op ir.Op, w bool, dst Reg, imm int32) {
	d := dst.mreg()
	switch op {
	case ir.OAdd:
		b.m.emit(x64.AddImm(w, d, imm))
	case ir.OSub:
		b.m.emit(x64.SubImm(w, d, imm))
	case ir.OAnd:
		b.m.emit(x64.AndImm(w, d, imm))
	case ir.OOr:
		b.m.emit(x64.OrImm(w, d, imm))
	case ir.OXor:
		b.m.emit(x64.XorImm(w, d, imm))
	default:
		b.fail("amd64: %s has no immediate ALU form", op)
	}
}

func (b *mcXasm) imulImmGP(w bool, dst, src Reg, imm int32) {
	b.m.emit(x64.ImulImm(w, dst.mreg(), src.mreg(), imm))
}

func (b *mcXasm) cmpImmGP(w bool, a Reg, imm int32) {
	b.m.emit(x64.CmpImm(w, a.mreg(), imm))
}

// selGP is TEST followed by CMOVNE.
//
// TEST rather than CMP $0: the two set ZF identically for `x == 0`, and TEST is
// the shorter encoding at every width because it needs no immediate field at all.
//
// CMOVcc has no immediate form, so a constant arm has already been materialized
// into src by the time this runs; that is why the selector stages the true arm in
// a scratch register instead of using it where the allocator left it.
func (b *mcXasm) selGP(w, condW bool, dst, src, cond Reg) {
	b.m.emit(x64.TestReg(condW, cond.mreg(), cond.mreg()))
	b.m.emit(x64.Cmovcc(w, x64.NE, dst.mreg(), src.mreg()))
}

// selCondScratch holds a conditional select's condition while the destination
// register is being written.
//
// It is RCX, which reg.go's intAllocOrder holds out of allocation for the
// fixed-register instructions, so nothing live can be sitting in it -- the same
// standing arrangement clzScratch/clzAux rely on for the leading-zero count and
// divGP relies on for RDX:RAX. Naming it here rather than writing RCX at the call
// site keeps the choice next to the reservation that justifies it.
const selCondScratch = RCX
