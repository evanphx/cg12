package arm64

import (
	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
)

// sel drives instruction selection against an asmb builder. It resolves each
// operand to a physical register -- loading a spilled temporary or materializing
// a constant through the builder -- so the selection logic is written once and
// serves both the machine-code and the text emitter. Each emitter constructs a
// sel over its own builder backend.
type sel struct {
	f         *ir.Func
	b         asmb
	spillBase int
}

// src resolves an integer source operand to a register, loading a spilled
// temporary into scratch register slot `slot` or materializing a constant there.
func (s *sel) src(ref ir.Ref, slot, size int) Reg {
	scr := intScratchRegs[slot]
	switch ref.Kind {
	case ir.RefTemp:
		t := s.f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return Reg(t.Reg)
		}
		s.b.ldrSpill(scr, false, s.spillBase+t.Slot, size)
		return scr
	case ir.RefConst:
		if c := s.f.Consts[ref.ID]; c.Kind == ir.ConstInt {
			s.b.movImm(scr, c.Int, size == 8)
			return scr
		}
	}
	s.b.fail("arm64: cannot materialize integer operand %v", ref)
	return scr
}

// dst resolves a destination operand to a register plus a finalizer that stores a
// spilled result back. A spilled result uses scratch slot 0, which a single
// three-operand instruction may share with its first source (read before write).
func (s *sel) dst(ref ir.Ref, size int) (Reg, func()) {
	t := s.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	scr := intScratchRegs[0]
	off := s.spillBase + t.Slot
	return scr, func() { s.b.strSpill(scr, false, off, size) }
}

// selectInt handles the integer data-processing instructions through the builder.
// It reports whether it handled the instruction; float and other ops fall back to
// the emitter's own logic during the migration.
func (s *sel) selectInt(in *ir.Instr) bool {
	if in.Cls.IsFloat() || s.hasSymOperand(in) {
		return false
	}
	sz := in.Cls.Size()
	w64 := sz == 8
	switch in.Op {
	case ir.OAdd:
		s.addSub(in, false)
	case ir.OSub:
		s.addSub(in, true)
	case ir.OMul:
		s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.mul(w64, rd, rn, rm) })
	case ir.ODiv:
		s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.sdiv(w64, rd, rn, rm) })
	case ir.OUDiv:
		s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.udiv(w64, rd, rn, rm) })
	case ir.ORem:
		s.rem(in, sz, true)
	case ir.OURem:
		s.rem(in, sz, false)
	case ir.OAnd:
		s.logical(in, logAnd)
	case ir.OOr:
		s.logical(in, logOrr)
	case ir.OXor:
		s.logical(in, logEor)
	case ir.OBic:
		s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.logicalReg(logBic, w64, rd, rn, rm) })
	case ir.OShl:
		s.shift(in, shLsl)
	case ir.OShr:
		s.shift(in, shLsr)
	case ir.OSar:
		s.shift(in, shAsr)
	case ir.ORotr:
		s.rotr(in)
	case ir.ONeg:
		d, done := s.dst(in.To, sz)
		s.b.neg(w64, d, s.src(in.Args[0], 1, sz))
		done()
	case ir.OClz:
		d, done := s.dst(in.To, sz)
		s.b.clz(w64, d, s.src(in.Args[0], 1, sz))
		done()
	default:
		return false
	}
	return true
}

// hasSymOperand reports whether any operand is a symbol constant, which the
// shared integer path does not yet materialize (those instructions stay on the
// emitter's own path during the migration).
func (s *sel) hasSymOperand(in *ir.Instr) bool {
	for _, a := range in.Args {
		if a.Kind == ir.RefConst && s.f.Consts[a.ID].Kind != ir.ConstInt {
			return true
		}
	}
	return false
}

// binReg emits a three-operand register instruction via the given builder call.
func (s *sel) binReg(in *ir.Instr, sz int, emit func(rd, rn, rm Reg)) {
	s1 := s.src(in.Args[0], 0, sz)
	s2 := s.src(in.Args[1], 1, sz)
	d, done := s.dst(in.To, sz)
	emit(d, s1, s2)
	done()
}

// addSub emits an add/sub, folding a constant operand into a 12-bit immediate and
// flipping add<->sub for a negative constant.
func (s *sel) addSub(in *ir.Instr, sub bool) {
	sz := in.Cls.Size()
	w64 := sz == 8
	aRef, bRef := in.Args[0], in.Args[1]
	if !sub {
		if _, ok := intConst(s.f, bRef); !ok {
			if _, ok := intConst(s.f, aRef); ok {
				aRef, bRef = bRef, aRef
			}
		}
	}
	if v, ok := intConst(s.f, bRef); ok {
		if imm, lsl12, flip, ok := addSubImm(v); ok {
			s1 := s.src(aRef, 0, sz)
			d, done := s.dst(in.To, sz)
			if sub != flip {
				s.b.subImm(w64, d, s1, imm, lsl12)
			} else {
				s.b.addImm(w64, d, s1, imm, lsl12)
			}
			done()
			return
		}
	}
	s.binReg(in, sz, func(rd, rn, rm Reg) {
		if sub {
			s.b.subReg(w64, rd, rn, rm)
		} else {
			s.b.addReg(w64, rd, rn, rm)
		}
	})
}

// logical emits a bitwise op, folding a constant operand into a logical (bitmask)
// immediate when it encodes as one.
func (s *sel) logical(in *ir.Instr, op logicalOp) {
	sz := in.Cls.Size()
	w64 := sz == 8
	aRef, bRef := in.Args[0], in.Args[1]
	if _, ok := intConst(s.f, bRef); !ok {
		if _, ok := intConst(s.f, aRef); ok {
			aRef, bRef = bRef, aRef
		}
	}
	if v, ok := intConst(s.f, bRef); ok {
		val := uint64(v)
		if sz == 4 {
			val &= 0xffffffff
		}
		if _, _, _, ok := a64.EncodeBitmask(val, sz); ok {
			s1 := s.src(aRef, 0, sz)
			d, done := s.dst(in.To, sz)
			s.b.logicalImm(op, w64, d, s1, val)
			done()
			return
		}
	}
	s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.logicalReg(op, w64, rd, rn, rm) })
}

// shift emits a shift, using the immediate form for a constant amount.
func (s *sel) shift(in *ir.Instr, op shiftOp) {
	sz := in.Cls.Size()
	w64 := sz == 8
	if v, ok := intConst(s.f, in.Args[1]); ok {
		if sh, ok := shiftImm(v, sz); ok {
			s1 := s.src(in.Args[0], 0, sz)
			d, done := s.dst(in.To, sz)
			s.b.shiftImm(op, w64, d, s1, sh)
			done()
			return
		}
	}
	s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.shiftReg(op, w64, rd, rn, rm) })
}

// rotr emits a right rotate by a constant amount.
func (s *sel) rotr(in *ir.Instr) {
	sz := in.Cls.Size()
	w64 := sz == 8
	if v, ok := intConst(s.f, in.Args[1]); ok {
		s1 := s.src(in.Args[0], 0, sz)
		d, done := s.dst(in.To, sz)
		s.b.rotrImm(w64, d, s1, uint32(v)&uint32(sz*8-1))
		done()
		return
	}
	s.b.fail("arm64: rotate by a non-constant is not supported")
}

// rem computes a remainder: q = a / b, then r = a - q*b via msub.
func (s *sel) rem(in *ir.Instr, sz int, signed bool) {
	w64 := sz == 8
	a := s.src(in.Args[0], 0, sz)
	b := s.src(in.Args[1], 1, sz)
	q := intScratchRegs[2]
	if signed {
		s.b.sdiv(w64, q, a, b)
	} else {
		s.b.udiv(w64, q, a, b)
	}
	d, done := s.dst(in.To, sz)
	s.b.msub(w64, d, q, b, a) // d = a - q*b
	done()
}
