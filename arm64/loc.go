package arm64

import "github.com/evanphx/cg12/ir"

// A loc is where a value is: in a register, in a spill slot, or not anywhere
// yet because it is a constant. Selection works in these rather than in
// registers alone, so that a move can be described before it is known whether
// either end has to go through memory.

// location kinds for parallel moves.
type loc struct {
	reg  Reg  // valid when !mem && !imm
	mem  bool // spilled: use slot offset
	slot int
	imm  bool // immediate constant
	val  int64
	size int
}

type movePairLoc struct{ dst, src loc }

func sameLoc(a, b loc) bool {
	if a.reg == b.reg && !a.mem && !b.mem && !a.imm && !b.imm {
		return true
	}
	if a.mem && b.mem && a.slot == b.slot {
		return true
	}
	return false
}

// srcReadsDst reports whether reading src touches the destination location dst.

func srcReadsDst(src, dst loc) bool {
	if src.imm {
		return false
	}
	if !src.mem && !dst.mem {
		return src.reg == dst.reg
	}
	if src.mem && dst.mem {
		return src.slot == dst.slot
	}
	return false
}

// parallelMove selects the simultaneous-move ordering once, through the shared sel.

// locOf is where a value lives: a register, a spill slot, or an immediate. It
// reports false for an operand that is not a movable value -- a symbol's address
// has to be materialized before it can be moved, so it has no location of its
// own, and the caller decides how to complain.
func locOf(f *ir.Func, ref ir.Ref) (loc, bool) {
	switch ref.Kind {
	case ir.RefTemp:
		t := f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return loc{reg: Reg(t.Reg), size: t.Cls.Size()}, true
		}
		return loc{mem: true, slot: t.Slot, size: t.Cls.Size()}, true
	case ir.RefConst:
		switch c := f.Consts[ref.ID]; c.Kind {
		case ir.ConstFloat:
			// Carry the bit pattern; the move goes via a GPR into the SIMD register.
			return loc{imm: true, val: floatBits(c), size: c.Cls.Size()}, true
		case ir.ConstInt:
			return loc{imm: true, val: c.Int, size: c.Cls.Size()}, true
		}
	}
	return loc{}, false
}

// isConstSym reports whether ref is a symbol's address, which is materialized
// rather than moved.
func isConstSym(f *ir.Func, ref ir.Ref) bool {
	return ref.Kind == ir.RefConst && f.Consts[ref.ID].Kind == ir.ConstSym
}
