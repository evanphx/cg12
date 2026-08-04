package interp_test

import (
	"fmt"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// The float math intrinsics in the interpreter. The interpreter is the
// target-independent oracle the backends are differentially tested against, so
// its answer for these has to be the instruction's answer down to the bits --
// including at the two places where the obvious Go spelling would drift: the
// NaN a negative square root produces, and the quiet bit an integral rounding
// sets on a signalling NaN operand.
//
// The expectations here are the same facts arm64/floatmath_e2e_test.go checks
// on the CPU, restated for the interpreter, so a divergence between the two
// shows up as a failure in one of them rather than as a difftest mystery.

const (
	positiveZero     = 0x0000000000000000
	negativeZero     = 0x8000000000000000
	positiveInfinity = 0x7FF0000000000000
	negativeInfinity = 0xFFF0000000000000
	goNaN            = 0x7FF8000000000001
	defaultNaN       = 0x7FF8000000000000
	negativeNaN      = 0xFFF8000000000000
	signallingNaN    = 0x7FF0000000000001
	quietedNaN       = 0x7FF8000000000001

	one              = 0x3FF0000000000000
	two              = 0x4000000000000000
	three            = 0x4008000000000000
	four             = 0x4010000000000000
	minusOne         = 0xBFF0000000000000
	minusTwo         = 0xC000000000000000
	minusThree       = 0xC008000000000000
	half             = 0x3FE0000000000000
	minusHalf        = 0xBFE0000000000000
	oneAndAHalf      = 0x3FF8000000000000
	minusOneAndAHalf = 0xBFF8000000000000
	twoAndAHalf      = 0x4004000000000000
	minusTwoAndAHalf = 0xC004000000000000
)

func TestFloatMathIntrinsics(t *testing.T) {
	cases := []struct {
		intrinsic string
		operand   uint64
		want      uint64
	}{
		// Sqrt(+Inf) = +Inf, Sqrt(±0) = ±0, Sqrt(x < 0) = NaN, Sqrt(NaN) = NaN.
		{"float.sqrt.d", four, two},
		{"float.sqrt.d", positiveZero, positiveZero},
		{"float.sqrt.d", negativeZero, negativeZero},
		{"float.sqrt.d", positiveInfinity, positiveInfinity},
		{"float.sqrt.d", minusOne, defaultNaN},
		{"float.sqrt.d", negativeInfinity, defaultNaN},
		{"float.sqrt.d", goNaN, goNaN},
		{"float.sqrt.d", negativeNaN, negativeNaN},
		{"float.sqrt.d", signallingNaN, quietedNaN},

		// Abs is the sign bit cleared and nothing else, so a signalling NaN
		// stays signalling.
		{"float.abs.d", minusThree, three},
		{"float.abs.d", negativeZero, positiveZero},
		{"float.abs.d", negativeInfinity, positiveInfinity},
		{"float.abs.d", negativeNaN, defaultNaN},
		{"float.abs.d", signallingNaN, signallingNaN},

		// Floor: toward -Inf.
		{"float.floor.d", oneAndAHalf, one},
		{"float.floor.d", minusOneAndAHalf, minusTwo},
		{"float.floor.d", half, positiveZero},
		{"float.floor.d", minusHalf, minusOne},
		{"float.floor.d", negativeZero, negativeZero},
		{"float.floor.d", negativeInfinity, negativeInfinity},
		{"float.floor.d", signallingNaN, quietedNaN},

		// Ceil: toward +Inf, and a fraction just below zero ceils to -0.
		{"float.ceil.d", oneAndAHalf, two},
		{"float.ceil.d", minusOneAndAHalf, minusOne},
		{"float.ceil.d", half, one},
		{"float.ceil.d", minusHalf, negativeZero},
		{"float.ceil.d", positiveZero, positiveZero},
		{"float.ceil.d", positiveInfinity, positiveInfinity},
		{"float.ceil.d", signallingNaN, quietedNaN},

		// Trunc: toward zero, so the sign survives into the zero.
		{"float.trunc.d", oneAndAHalf, one},
		{"float.trunc.d", minusOneAndAHalf, minusOne},
		{"float.trunc.d", half, positiveZero},
		{"float.trunc.d", minusHalf, negativeZero},
		{"float.trunc.d", negativeInfinity, negativeInfinity},
		{"float.trunc.d", goNaN, goNaN},

		// Roundeven: ties to even.
		{"float.roundeven.d", half, positiveZero},
		{"float.roundeven.d", minusHalf, negativeZero},
		{"float.roundeven.d", oneAndAHalf, two},
		{"float.roundeven.d", twoAndAHalf, two},
		{"float.roundeven.d", minusTwoAndAHalf, minusTwo},
		{"float.roundeven.d", negativeZero, negativeZero},
		{"float.roundeven.d", signallingNaN, quietedNaN},

		// Roundaway: ties away from zero, which is where it parts company with
		// roundeven.
		{"float.roundaway.d", half, one},
		{"float.roundaway.d", minusHalf, minusOne},
		{"float.roundaway.d", oneAndAHalf, two},
		{"float.roundaway.d", twoAndAHalf, three},
		{"float.roundaway.d", minusTwoAndAHalf, minusThree},
		{"float.roundaway.d", positiveZero, positiveZero},
		{"float.roundaway.d", signallingNaN, quietedNaN},
	}

	for _, c := range cases {
		name := fmt.Sprintf("%s/%#016x", c.intrinsic, c.operand)
		t.Run(name, func(t *testing.T) {
			// The operand arrives and the result leaves as raw bits, so no
			// float64 conversion can quietly canonicalise a NaN on the way.
			il := fmt.Sprintf(`export function l $f(l %%bits) {
				@start
				%%x =d cast %%bits
				%%r =d intrinsic %s %%x
				%%out =l cast %%r
				ret %%out
			}`, c.intrinsic)

			operand := interp.Value{Cls: ir.ClsL, Bits: c.operand}
			tw := run(t, il, "f", operand)
			bc := runBC(t, il, "f", operand)
			require.Equal(t, tw, bc, "tree-walker vs bytecode")
			require.Equalf(t, c.want, tw.U64(),
				"%s of %#016x = %#016x, want %#016x", c.intrinsic, c.operand, tw.U64(), c.want)
		})
	}
}

// TestFloatMathIntrinsicsSingle checks the single-precision arm, whose NaN
// fields sit in different bit positions.
func TestFloatMathIntrinsicsSingle(t *testing.T) {
	cases := []struct {
		intrinsic string
		operand   uint32
		want      uint32
	}{
		{"float.sqrt.s", 0x40800000, 0x40000000},      // sqrt(4) = 2
		{"float.sqrt.s", 0xBF800000, 0x7FC00000},      // sqrt(-1) = the default NaN
		{"float.sqrt.s", 0x80000000, 0x80000000},      // sqrt(-0) = -0
		{"float.abs.s", 0xC0400000, 0x40400000},       // abs(-3) = 3
		{"float.abs.s", 0xFF800000, 0x7F800000},       // abs(-Inf) = +Inf
		{"float.floor.s", 0xBFC00000, 0xC0000000},     // floor(-1.5) = -2
		{"float.ceil.s", 0xBF000000, 0x80000000},      // ceil(-0.5) = -0
		{"float.trunc.s", 0xBF000000, 0x80000000},     // trunc(-0.5) = -0
		{"float.roundeven.s", 0x40200000, 0x40000000}, // roundeven(2.5) = 2
		{"float.roundaway.s", 0x40200000, 0x40400000}, // round(2.5) = 3
		// A signalling NaN comes back quiet from a rounding, and unchanged from abs.
		{"float.roundeven.s", 0x7F800001, 0x7FC00001},
		{"float.abs.s", 0x7F800001, 0x7F800001},
	}

	for _, c := range cases {
		name := fmt.Sprintf("%s/%#08x", c.intrinsic, c.operand)
		t.Run(name, func(t *testing.T) {
			il := fmt.Sprintf(`export function w $f(w %%bits) {
				@start
				%%x =s cast %%bits
				%%r =s intrinsic %s %%x
				%%out =w cast %%r
				ret %%out
			}`, c.intrinsic)

			operand := interp.Value{Cls: ir.ClsW, Bits: uint64(int64(int32(c.operand)))}
			tw := run(t, il, "f", operand)
			bc := runBC(t, il, "f", operand)
			require.Equal(t, tw, bc, "tree-walker vs bytecode")
			require.Equalf(t, c.want, uint32(tw.U64()),
				"%s of %#08x = %#08x, want %#08x", c.intrinsic, c.operand, uint32(tw.U64()), c.want)
		})
	}
}
