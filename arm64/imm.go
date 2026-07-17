package arm64

import (
	"math"

	"github.com/evanphx/cg12/ir"
)

// This file holds the pure decision logic for folding integer constants into
// AArch64 immediate instruction forms. Both backends — the machine-code emitter
// (mc.go) and the text emitter (emit.go) — use it so they agree on when an
// operand becomes an immediate rather than a materialized register.

// intConst returns the integer value of ref when it is a ConstInt.
func intConst(f *ir.Func, ref ir.Ref) (int64, bool) {
	if ref.Kind != ir.RefConst {
		return 0, false
	}
	c := f.Consts[ref.ID]
	if c.Kind != ir.ConstInt {
		return 0, false
	}
	return c.Int, true
}

// shiftImm reports whether v is a valid immediate shift amount for a size-byte
// operand (0 .. bits-1).
func shiftImm(v int64, size int) (uint32, bool) {
	if v >= 0 && v < int64(size)*8 {
		return uint32(v), true
	}
	return 0, false
}

// addSubImm decides how to fold a constant into an add or sub. It returns the
// unsigned 12-bit immediate, whether it is shifted left by 12, whether the
// operation must flip (a negative constant turns add into sub and vice versa),
// and ok. A value is foldable if it (or its negation) fits 12 bits, optionally
// shifted left by 12.
func addSubImm(v int64) (imm uint32, lsl12, flip, ok bool) {
	n := v
	if n < 0 {
		flip = true
		n = -n
	}
	switch {
	case n <= 0xfff:
		return uint32(n), false, flip, true
	case n&0xfff == 0 && n>>12 <= 0xfff:
		return uint32(n >> 12), true, flip, true
	}
	return 0, false, false, false
}

// rotateMatch reports whether instruction `or` is the rotate idiom
//
//	(x >> c) | (x << (w-c))   -- rotate right by c
//	(x << c) | (x >> (w-c))   -- rotate left by c
//
// over its own width w, and returns the source value x and the right-rotation
// amount. defs maps a temporary id to the instruction that defines it, so the
// two shift operands can be inspected. Only pure single-use shift pairs of a
// common source with complementary constant amounts qualify.
func rotateMatch(f *ir.Func, or *ir.Instr, defOf func(ir.Ref) *ir.Instr) (x ir.Ref, ror uint32, ok bool) {
	if or.Op != ir.OOr {
		return ir.Ref{}, 0, false
	}
	w := int64(or.Cls.Size()) * 8
	if w != 32 && w != 64 {
		return ir.Ref{}, 0, false
	}
	a, b := defOf(or.Args[0]), defOf(or.Args[1])
	if a == nil || b == nil {
		return ir.Ref{}, 0, false
	}
	// Normalize so a is the right shift (OShr) and b is the left shift (OShl).
	if a.Op == ir.OShl && b.Op == ir.OShr {
		a, b = b, a
	}
	if a.Op != ir.OShr || b.Op != ir.OShl {
		return ir.Ref{}, 0, false
	}
	if a.Args[0] != b.Args[0] { // same source value
		return ir.Ref{}, 0, false
	}
	sr, ok1 := intConst(f, a.Args[1])
	sl, ok2 := intConst(f, b.Args[1])
	if !ok1 || !ok2 || sr <= 0 || sl <= 0 || sr+sl != w {
		return ir.Ref{}, 0, false
	}
	return a.Args[0], uint32(sr), true
}

// floatConstBits returns the bit pattern of a float-valued constant, whether
// written as a float (Flt) or as a raw integer bit pattern (Int of float class).
func floatConstBits(c ir.Const) (int64, bool) {
	switch c.Kind {
	case ir.ConstFloat:
		return floatBits(c), true
	case ir.ConstInt:
		return c.Int, true
	}
	return 0, false
}

// floatBits returns the IEEE-754 bit pattern of a floating constant.
func floatBits(c ir.Const) int64 {
	if c.Cls == ir.ClsS {
		return int64(math.Float32bits(float32(c.Flt)))
	}
	return int64(math.Float64bits(c.Flt))
}
