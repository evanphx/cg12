package amd64

import (
	"math"

	"github.com/evanphx/cg12/ir"
)

// This file holds the pure decision logic for folding a constant into an x86-64
// instruction's immediate field: whether an operand can be spelled as an
// immediate at all, and which operand of a two-operand instruction it is. It is
// separate from emission for the same reason arm64/imm.go is -- the width rule
// below is the whole correctness content of the optimization, and it is worth
// reading (and testing) without an assembler in the way.
//
// Nothing here emits anything; see xasm_imm.go for the encodings and
// xselect_imm.go for the selection that consumes these answers.

// immFitsALU reports whether v can be the immediate operand of an x86-64 ALU
// instruction at the given operand width, and returns the 32-bit field to encode.
//
// The width rule is the one real hazard in this file. **x86-64 has no 64-bit ALU
// immediate.** Under REX.W the 81 /ext id, 83 /ext ib and 69 /r id forms all carry
// a signed 32-bit (or 8-bit) field that the machine sign-extends to 64 bits before
// operating, so a 64-bit operation can fold only a constant that is already in
// [-2^31, 2^31). A value outside that -- 1<<32, or the 0xffffffff a `& 0xffffffff`
// mask is -- must still be materialized into a register with MOVABS. Folding it
// would not fail to assemble; it would compute with a different number.
//
// A 32-bit operation has no such limit. It reads and writes 32 bits, so the
// immediate field already spans every value the operation can distinguish, and
// int32(v) is exactly the constant the register form would have materialized:
// mc.movImm with w == false emits MOV r32, imm32 of the same truncation. That
// symmetry is deliberate -- this predicate and movImm's own MinInt32/MaxInt32
// test are the same boundary, so a constant is either folded or materialized,
// never rounded.
func immFitsALU(v int64, w bool) (int32, bool) {
	if !w {
		return int32(v), true
	}
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, false
	}
	return int32(v), true
}

// commutativeInt reports whether an integer two-operand op may take its constant
// on either side. x86's immediate forms only ever encode the *second* operand, so
// `2 - x` cannot fold while `x - 2` can; being explicit about which ops may be
// reassociated is what keeps that from being an off-by-one in the operand index.
func commutativeInt(op ir.Op) bool {
	switch op {
	case ir.OAdd, ir.OMul, ir.OAnd, ir.OOr, ir.OXor:
		return true
	}
	return false
}

// immBinOperands splits a two-operand integer instruction into the operand that
// stays in a register and the immediate that folds into the instruction, or
// reports false when the instruction has no foldable constant.
//
// Both operands being constants is not special-cased: the second-operand branch
// claims it, the first operand is materialized into the destination as usual, and
// the fold still saves the scratch register. (opt.Fold normally settles such an
// instruction long before selection; this only has to stay correct when it did
// not run.)
func immBinOperands(f *ir.Func, in *ir.Instr, w bool) (reg ir.Ref, imm int32, ok bool) {
	if c := intConstAMD(f, in.Arg(1)); c != nil {
		if v, fits := immFitsALU(c.Int, w); fits {
			return in.Arg(0), v, true
		}
		return ir.Ref{}, 0, false
	}
	if commutativeInt(in.Op) {
		if c := intConstAMD(f, in.Arg(0)); c != nil {
			if v, fits := immFitsALU(c.Int, w); fits {
				return in.Arg(1), v, true
			}
		}
	}
	return ir.Ref{}, 0, false
}

// cmovClass reports whether a conditional select of this class can be emitted
// branchlessly.
//
// CMOVcc is an integer instruction, so W and L are direct. S and D qualify too,
// because a select is a choice between two bit patterns and not an arithmetic
// operation: moving both arms through general registers and moving the winner
// back is exact for every value including NaNs and both zeros (see selFloat).
// Anything else -- a 128-bit ClsQ select -- has no branchless form here and is
// rewritten into a control-flow diamond instead; see lowerSelects.
func cmovClass(cls ir.Cls) bool {
	switch cls {
	case ir.ClsW, ir.ClsL, ir.ClsS, ir.ClsD:
		return true
	}
	return false
}
