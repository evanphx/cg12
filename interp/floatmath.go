package interp

import (
	"math"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// The float math intrinsics: "float.<operation>.<width>", each a single
// instruction on the targets that have one (see arm64/select.go) and each a
// function of its operand alone.
//
// The interpreter has to produce the same bits the hardware does, not merely
// the same number, because these are the operations whose whole point is their
// behaviour at the edges. Two places where the obvious Go spelling would not:
//
//   - NaN. AArch64's FRINT family and FSQRT return their NaN operand with the
//     quiet bit set, preserving the payload and the sign; Go's portable math
//     functions return the operand untouched, and math.Sqrt of a negative
//     number returns a NaN whose payload is Go's own. Both are handled here
//     explicitly rather than left to whatever the host does.
//   - FABS is the sign bit cleared and nothing else. math.Abs is defined the
//     same way, but going through a float64 would leave a signalling NaN's
//     quiet bit to the host's discretion, so the bits are masked directly.
//
// Everything else -- every finite value, both infinities, both zeros -- is the
// ordinary IEEE 754 result, which is what the Go math package specifies and
// what the instruction computes.

// floatMathOp evaluates a float math intrinsic by name. It reports false for a
// name it does not implement, which the caller turns into a trap.
func floatMathOp(name string, a Value) (Value, bool) {
	operation, cls, ok := floatMathParts(name)
	if !ok {
		return Value{}, false
	}

	if operation == "abs" {
		return Value{Cls: cls, Bits: a.Bits &^ signBit(cls)}, true
	}

	// Every remaining operation quiets a NaN operand and passes it through.
	if isNaNBits(cls, a.Bits) {
		return Value{Cls: cls, Bits: a.Bits | quietBit(cls)}, true
	}

	x := a.f64()
	var result float64
	switch operation {
	case "sqrt":
		if x < 0 {
			// The square root of a negative number is invalid, and the
			// instruction answers with the architecture's default NaN rather
			// than with anything derived from the operand.
			return Value{Cls: cls, Bits: defaultNaNBits(cls)}, true
		}
		result = math.Sqrt(x)
	case "roundeven":
		result = math.RoundToEven(x)
	case "roundaway":
		result = math.Round(x)
	case "ceil":
		result = math.Ceil(x)
	case "floor":
		result = math.Floor(x)
	case "trunc":
		result = math.Trunc(x)
	default:
		return Value{}, false
	}
	return floatVal(cls, result), true
}

// floatMathParts splits a "float.<operation>.<width>" name into its operation
// and the float class the width names.
func floatMathParts(name string) (operation string, cls ir.Cls, ok bool) {
	trimmed := strings.TrimPrefix(name, "float.")
	switch {
	case strings.HasSuffix(trimmed, ".s"):
		return strings.TrimSuffix(trimmed, ".s"), ir.ClsS, true
	case strings.HasSuffix(trimmed, ".d"):
		return strings.TrimSuffix(trimmed, ".d"), ir.ClsD, true
	}
	return "", 0, false
}

// signBit, quietBit and defaultNaNBits are the IEEE 754 fields of the class's
// format: the sign, the leading significand bit that distinguishes a quiet NaN
// from a signalling one, and the NaN an invalid operation produces.
func signBit(cls ir.Cls) uint64 {
	if cls == ir.ClsS {
		return 1 << 31
	}
	return 1 << 63
}

func quietBit(cls ir.Cls) uint64 {
	if cls == ir.ClsS {
		return 1 << 22
	}
	return 1 << 51
}

func defaultNaNBits(cls ir.Cls) uint64 {
	if cls == ir.ClsS {
		return 0x7FC00000
	}
	return 0x7FF8000000000000
}

// isNaNBits reports whether the bits are a NaN of the class's format, without
// routing them through a float64 (which would canonicalise a single's payload).
func isNaNBits(cls ir.Cls, bits uint64) bool {
	if cls == ir.ClsS {
		single := uint32(bits)
		return single&0x7F800000 == 0x7F800000 && single&0x007FFFFF != 0
	}
	return bits&0x7FF0000000000000 == 0x7FF0000000000000 && bits&0x000FFFFFFFFFFFFF != 0
}
