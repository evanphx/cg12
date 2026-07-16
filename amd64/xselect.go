package amd64

import "github.com/evanphx/cg12/ir"

// xsel drives amd64 instruction selection against an xasm builder, resolving
// operands to registers through the builder's move primitive so the selection is
// written once for both the machine-code and text emitters (mirroring arm64's
// sel).
type xsel struct {
	f *ir.Func
	b xasm
}

// gpValue returns a GPR holding ref's value, loading it into scratch if it is not
// already in a register.
func (s *xsel) gpValue(ref ir.Ref, scratch Reg) Reg {
	l := s.b.refLoc(ref)
	if l.kind == locReg {
		return l.reg
	}
	s.b.move(regLoc(scratch, l.size, false), l)
	return scratch
}

// gpInto places ref's value into GPR d.
func (s *xsel) gpInto(d Reg, ref ir.Ref) {
	l := s.b.refLoc(ref)
	s.b.move(regLoc(d, l.size, false), l)
}

// gpDst returns the destination GPR for an integer result and a commit closure
// that stores it back when the result is spilled.
func (s *xsel) gpDst(ref ir.Ref) (Reg, func()) {
	t := s.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	size := t.Cls.Size()
	slot := t.Slot
	return gpScratch0, func() { s.b.spillStore(gpScratch0, slot, size) }
}

// selectInt handles the two-operand integer arithmetic instructions
// (add/sub/mul/and/or/xor) through the builder. It reports whether it handled the
// instruction; everything else falls back to the emitter's own logic.
func (s *xsel) selectInt(in *ir.Instr) bool {
	switch in.Op {
	case ir.OAdd, ir.OSub, ir.OMul, ir.OAnd, ir.OOr, ir.OXor:
		if in.Cls.IsFloat() {
			return false
		}
		s.binInt(in)
	case ir.ONeg:
		if in.Cls.IsFloat() {
			return false
		}
		w := in.Cls == ir.ClsL
		d, commit := s.gpDst(in.To)
		s.gpInto(d, in.Arg(0))
		s.b.negGP(w, d)
		commit()
	case ir.OShl, ir.OShr, ir.OSar:
		s.shift(in)
	case ir.ODiv:
		if in.Cls.IsFloat() {
			return false
		}
		s.div(in, true, false)
	case ir.OUDiv:
		s.div(in, false, false)
	case ir.ORem:
		s.div(in, true, true)
	case ir.OURem:
		s.div(in, false, true)
	case ir.OCmp:
		if in.Cmp.IsFloat() {
			return false
		}
		s.cmp(in)
	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw:
		w := in.Cls == ir.ClsL
		rs := s.gpValue(in.Arg(0), gpScratch1)
		d, commit := s.gpDst(in.To)
		s.b.extGP(in.Op, w, d, rs)
		commit()
	case ir.OCopy:
		s.b.move(s.b.refLoc(in.To), s.b.refLoc(in.Arg(0)))
	case ir.OAllocN:
		size := s.gpValue(in.Args[0], gpScratch0)
		d, commit := s.gpDst(in.To)
		s.b.allocNSP(d, size)
		commit()
	case ir.OStackSave:
		d, commit := s.gpDst(in.To)
		s.b.movFromSP(d)
		commit()
	case ir.OStackRestore:
		s.b.movToSP(s.gpValue(in.Args[0], gpScratch0))
	default:
		return false
	}
	return true
}

// term selects a block terminator through the builder, handling the simple
// branch forms. It returns false for the return and jump-table terminators,
// which stay on each emitter's own path (frame epilogue and PC-relative table).
func (s *xsel) term(b *ir.Block) bool {
	switch b.Jmp.Kind {
	case ir.JmpJmp:
		s.b.jmp(b.Jmp.To)
	case ir.JmpJnz:
		w := s.f.ClassOf(b.Jmp.Arg) == ir.ClsL
		s.b.jnz(s.gpValue(b.Jmp.Arg, gpScratch0), w, b.Jmp.To, b.Jmp.To2)
	case ir.JmpHlt:
		s.b.hlt()
	case ir.JmpBr:
		s.b.jmpReg(s.gpValue(b.Jmp.Arg, gpScratch0))
	default:
		return false
	}
	return true
}

// div emits an integer divide/remainder: place the dividend in RAX, divide, then
// move the quotient (RAX) or remainder (RDX) to the destination.
func (s *xsel) div(in *ir.Instr, signed, rem bool) {
	w := in.Cls == ir.ClsL
	rb := s.gpValue(in.Arg(1), gpScratch1)
	s.gpInto(RAX, in.Arg(0))
	s.b.divGP(w, signed, rb)
	d, commit := s.gpDst(in.To)
	res := RAX
	if rem {
		res = RDX
	}
	s.b.movReg(w, d, res)
	commit()
}

// shift emits a shift by an immediate count or by %cl.
func (s *xsel) shift(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	d, commit := s.gpDst(in.To)
	if c := intConstAMD(s.f, in.Arg(1)); c != nil {
		s.gpInto(d, in.Arg(0))
		s.b.shiftImmGP(in.Op, w, d, byte(c.Int&63))
		commit()
		return
	}
	s.gpInto(RCX, in.Arg(1))
	s.gpInto(d, in.Arg(0))
	s.b.shiftCLGP(in.Op, w, d)
	commit()
}

// cmp emits a compare and a movzx'd conditional-set of the boolean result.
func (s *xsel) cmp(in *ir.Instr) {
	argW := s.f.ClassOf(in.Arg(0)) == ir.ClsL
	ra := s.gpValue(in.Arg(0), gpScratch0)
	rb := s.gpValue(in.Arg(1), gpScratch1)
	s.b.cmpGP(argW, ra, rb)
	d, commit := s.gpDst(in.To)
	s.b.setccMovzx(in.Cmp, d)
	commit()
}

// intConstAMD returns ref's integer constant, or nil.
func intConstAMD(f *ir.Func, ref ir.Ref) *ir.Const {
	if ref.Kind == ir.RefConst {
		if c := f.Consts[ref.ID]; c.Kind == ir.ConstInt {
			return &c
		}
	}
	return nil
}

// binInt computes dst = arg0 OP arg1 in x86's two-operand form: place arg0 in the
// destination, then apply the op with arg1. If arg1 already occupies the
// destination register it is moved aside first.
func (s *xsel) binInt(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	d, commit := s.gpDst(in.To)
	rb := s.gpValue(in.Arg(1), gpScratch1)
	if rb == d {
		s.b.movReg(w, gpScratch1, rb)
		rb = gpScratch1
	}
	s.gpInto(d, in.Arg(0))
	s.b.binGP(in.Op, w, d, rb)
	commit()
}
