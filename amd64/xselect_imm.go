package amd64

import (
	"github.com/evanphx/cg12/ir"
	lowerpass "github.com/evanphx/cg12/lower"
)

// selectImm selects the instruction forms that take a constant operand in the
// instruction itself, and the conditional select.
//
// The two belong together because they are the same omission. Every encoder this
// file reaches -- x64.AddImm, AndImm, OrImm, XorImm, ImulImm, CmpImm, Cmovcc --
// already existed and was called zero times: selection materialized every
// constant into a scratch register and then did a register-register operation,
// and it turned every conditional select into a branch diamond, on a machine that
// has one instruction for each.
//
// This family *overrides* selectCore, which is why it probes first (see
// xselect_registry.go). selectCore's binInt would handle ir.OAdd and friends
// perfectly well; it would just handle them one instruction and one scratch
// register more expensively. Whenever no constant folds, this returns false and
// the generic path runs unchanged -- so the register-operand behaviour of the
// shared integer path is untouched by construction, not by care.
//
// What is deliberately left out, on the precedent xselect_bits.go sets:
//
//   - The immediate *compare* is not claimed here even though it is part of the
//     same work. A compare that feeds its block's branch never reaches selectInt
//     at all -- mc.block emits it flags-only through xsel.cmpFlags and lets the
//     terminator branch on the result -- so claiming ir.OCmp here would fold the
//     constant for the boolean-producing compare and silently miss the fused one,
//     which is the common case. The fold lives in cmpFlags instead, where both
//     paths pass through it.
//   - No shift, and no immediate form of ir.ONeg or the extensions: shifts already
//     had shiftImmGP (xasm_int.go) before this file existed, and the others take
//     one operand, so there is no second operand to be constant.
//   - No memory-destination or memory-source immediate (ADD [mem], imm). Selection
//     produces results in registers and commits them itself, and a constant is
//     never in memory to begin with.
func selectImm(s *xsel, in *ir.Instr) bool {
	switch in.Op {
	case ir.OSel:
		return s.sel(in)
	case ir.OAdd, ir.OSub, ir.OMul, ir.OAnd, ir.OOr, ir.OXor:
		if in.Cls != ir.ClsW && in.Cls != ir.ClsL {
			return false // float arithmetic, and the 128-bit class selectWide owns
		}
		w := in.Cls == ir.ClsL
		reg, imm, ok := immBinOperands(s.f, in, w)
		if !ok {
			return false
		}
		s.binImm(in, w, reg, imm)
		return true
	}
	return false
}

// binImm emits a two-operand integer instruction whose second operand folded into
// an immediate field.
//
// Compared with binInt this needs no scratch register and no aliasing check: with
// only one register operand there is no second one that might already occupy the
// destination, so the "move the other operand aside first" case cannot arise.
//
// Multiplication is the exception and gets the better instruction for it. IMUL's
// immediate form (69 /r id) is genuinely three-operand -- dst = src * imm -- so
// the source is only read, and nothing has to be copied into the destination
// first. dst and src being the same register is fine for the same reason.
func (s *xsel) binImm(in *ir.Instr, w bool, reg ir.Ref, imm int32) {
	if in.Op == ir.OMul {
		rs := s.gpValue(reg, gpScratch1)
		d, commit := s.gpDst(in.To)
		s.b.imulImmGP(w, d, rs, imm)
		commit()
		return
	}
	d, commit := s.gpDst(in.To)
	s.gpInto(d, reg)
	s.b.binImmGP(in.Op, w, d, imm)
	commit()
}

// sel emits a conditional select. It reports false for a class with no branchless
// form, which lowerSelects has already rewritten into a control-flow diamond, so
// in practice it always succeeds.
func (s *xsel) sel(in *ir.Instr) bool {
	switch in.Cls {
	case ir.ClsW, ir.ClsL:
		s.selInt(in)
	case ir.ClsS, ir.ClsD:
		s.selFloat(in)
	default:
		return false
	}
	return true
}

// selInt emits an integer select as TEST + CMOVNE.
//
// The order is forced by two constraints pulling in opposite directions. TEST
// writes the flags CMOV reads, so every operand has to be in place before it --
// and every materialization used here is flags-preserving (MOV, a load, MOV
// imm32, MOVABS, LEA; see mc.moveToReg), so putting them first costs nothing.
// Against that, the destination register is written before the CMOV, which means
// anything still needed afterwards must be somewhere the destination is not.
//
// Two values are still needed at the CMOV: the condition and the true arm.
//
//   - The condition is read into selCondScratch when it shares the destination
//     register. That is not a hypothetical: the condition dies at this
//     instruction, so the allocator is free to give the result its register, and
//     writing the false arm into the destination would then destroy the very
//     value the TEST has to look at.
//   - The true arm is always staged in gpScratch1. It could be left where the
//     allocator put it, but the same aliasing case applies, and gpScratch1 is
//     free by construction -- reg.go keeps it out of allocation -- so the
//     unconditional copy buys the whole hazard away for one MOV that the register
//     form would have emitted anyway.
//
// The false arm goes straight to the destination because that is what CMOV
// leaves behind when the condition is false: it is the fall-through value, not a
// third operand.
func (s *xsel) selInt(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	condW := s.f.ClassOf(in.Arg(0)) == ir.ClsL
	d, commit := s.gpDst(in.To)

	rc := s.gpValue(in.Arg(0), selCondScratch)
	if rc == d {
		s.gpInto(selCondScratch, in.Arg(0))
		rc = selCondScratch
	}
	s.gpInto(gpScratch1, in.Arg(1)) // true arm, out of the destination's way
	s.gpInto(d, in.Arg(2))          // false arm: CMOV's fall-through value
	s.b.selGP(w, condW, d, gpScratch1, rc)
	commit()
}

// selFloat emits a float select by choosing between the two arms' *bit patterns*
// in general registers and moving the winner back into an XMM.
//
// x86-64 has no SSE conditional move (the FCMOVcc family is x87 only), and the
// alternative -- a mask built with CMPSD and combined with ANDPD/ANDNPD/ORPD --
// needs two more XMM registers than the emitter has spare. Going through GPRs is
// exact regardless: a select chooses one of two values and performs no
// arithmetic, so NaN payloads, signalling NaNs and the two signed zeros all
// survive bit-for-bit, which is more than the mask sequence could promise.
//
// The bits never pass through fpScratch0: floatBitsGP reads a spilled or constant
// arm as an integer directly, which matters because mc.materializeFloat stages a
// float constant through gpScratch0 -- the register holding the false arm here.
func (s *xsel) selFloat(in *ir.Instr) {
	long := in.Cls == ir.ClsD
	condW := s.f.ClassOf(in.Arg(0)) == ir.ClsL

	rc := s.gpValue(in.Arg(0), selCondScratch)
	s.floatBitsGP(gpScratch1, in.Arg(1)) // true arm
	s.floatBitsGP(gpScratch0, in.Arg(2)) // false arm
	s.b.selGP(long, condW, gpScratch0, gpScratch1, rc)

	d, commit := s.fpDst(in.To)
	s.b.castG2F(long, d, gpScratch0)
	commit()
}

// floatBitsGP places the raw bits of a float-class operand into a general
// register. A value already in an XMM needs the cross-bank move (MOVD/MOVQ); a
// spilled one is just an integer load of its slot, and a float constant is
// already carried as its bit pattern, so both take the ordinary move machinery
// with the operand's float flag cleared.
func (s *xsel) floatBitsGP(dst Reg, ref ir.Ref) {
	l := s.b.refLoc(ref)
	if l.kind == locReg {
		s.b.castF2G(l.size == 8, dst, l.reg)
		return
	}
	l.float = false
	s.b.move(regLoc(dst, l.size, false), l)
}

// lowerSelects prepares f's conditional selects for emission, replacing the
// unconditional lower.Selects call amd64 used to make.
//
// Every select selectImm can emit is left alone for it. Anything else -- today
// only a 128-bit ClsQ select, which nothing on amd64 produces -- falls back to
// lower.Selects, which rewrites selects into control-flow diamonds with phis.
//
// The fallback is whole-function rather than per-select because lower.Selects
// takes a function, and making it selective would mean changing a package three
// other backends share. That coarseness costs nothing that was not already being
// paid: a function it fires on is compiled exactly the way every function was
// compiled before this change.
func lowerSelects(f *ir.Func) {
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if in := &b.Instrs[i]; in.Op == ir.OSel && !cmovClass(in.Cls) {
				lowerpass.Selects(f)
				return
			}
		}
	}
}
