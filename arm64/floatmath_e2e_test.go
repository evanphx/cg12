package arm64_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// The float math intrinsics, compiled to machine code and run on the CPU.
//
// These exist because the whole argument for lowering math.Sqrt and its
// neighbours is that the instruction's answer is the specified answer at the
// edges as well as in the middle. Checking the middle proves nothing: a
// software square root gets 2.0 right too. So every case here is an edge -- a
// NaN, a signed zero, an infinity, a negative operand, a tie -- and every
// comparison is on the bits, because -0.0 == 0.0 and NaN != NaN would let a
// wrong answer through a comparison on values.
//
// The expected bits are stated literally rather than computed from the Go math
// package, which on this host lowers to these very instructions and so would
// only ask the hardware to confirm itself.

// floatMathCase is one intrinsic applied to one operand, and the exact result
// the AArch64 instruction must produce.
type floatMathCase struct {
	name     string
	operand  uint64
	expected uint64
}

const (
	positiveZero     = 0x0000000000000000
	negativeZero     = 0x8000000000000000
	positiveInfinity = 0x7FF0000000000000
	negativeInfinity = 0xFFF0000000000000
	// goNaN is the bit pattern math.NaN() returns: a quiet NaN with payload 1.
	goNaN = 0x7FF8000000000001
	// defaultNaN is the NaN AArch64 produces for an invalid operation.
	defaultNaN = 0x7FF8000000000000
	// negativeNaN is a quiet NaN with the sign bit set, which the instructions
	// carry through rather than canonicalise.
	negativeNaN = 0xFFF8000000000000
	// signallingNaN has an exponent of all ones and a zero leading significand
	// bit. A Go program can only make one through math.Float64frombits.
	signallingNaN = 0x7FF0000000000001
	// quietedSignallingNaN is signallingNaN with the quiet bit set, which is
	// what an instruction that processes a NaN operand returns.
	quietedSignallingNaN = 0x7FF8000000000001
)

func bitsOf(x float64) uint64 { return math.Float64bits(x) }

// sqrtCases: Sqrt(+Inf) = +Inf, Sqrt(±0) = ±0, Sqrt(x < 0) = NaN, Sqrt(NaN) = NaN.
var sqrtCases = []floatMathCase{
	{"four", bitsOf(4), bitsOf(2)},
	{"two", bitsOf(2), bitsOf(math.Sqrt2)},
	{"a half", bitsOf(0.5), bitsOf(0.7071067811865476)},
	{"the largest finite value", bitsOf(math.MaxFloat64), bitsOf(1.3407807929942596e154)},
	{"the smallest subnormal", bitsOf(math.SmallestNonzeroFloat64), bitsOf(2.2227587494850775e-162)},

	{"positive zero", positiveZero, positiveZero},
	{"negative zero", negativeZero, negativeZero},
	{"positive infinity", positiveInfinity, positiveInfinity},

	// Every negative operand is invalid and answers with the default NaN.
	{"minus one", bitsOf(-1), defaultNaN},
	{"a small negative", bitsOf(-1e-300), defaultNaN},
	{"the most negative finite value", bitsOf(-math.MaxFloat64), defaultNaN},
	{"negative infinity", negativeInfinity, defaultNaN},
	{"the smallest negative subnormal", bitsOf(-math.SmallestNonzeroFloat64), defaultNaN},

	// A NaN operand comes back quiet, with its payload and sign intact.
	{"Go's NaN", goNaN, goNaN},
	{"a negative NaN", negativeNaN, negativeNaN},
	{"a signalling NaN", signallingNaN, quietedSignallingNaN},
}

// absCases: Abs(±Inf) = +Inf, Abs(NaN) = NaN. FABS clears the sign bit and
// touches nothing else, so it does not even quiet a signalling NaN.
var absCases = []floatMathCase{
	{"minus three", bitsOf(-3), bitsOf(3)},
	{"three", bitsOf(3), bitsOf(3)},
	{"negative zero", negativeZero, positiveZero},
	{"positive zero", positiveZero, positiveZero},
	{"negative infinity", negativeInfinity, positiveInfinity},
	{"positive infinity", positiveInfinity, positiveInfinity},
	{"the most negative finite value", bitsOf(-math.MaxFloat64), bitsOf(math.MaxFloat64)},
	{"the smallest negative subnormal", bitsOf(-math.SmallestNonzeroFloat64), bitsOf(math.SmallestNonzeroFloat64)},
	{"a negative NaN", negativeNaN, defaultNaN},
	{"Go's NaN", goNaN, goNaN},
	{"a signalling NaN", signallingNaN, signallingNaN},
}

// The four roundings share their special cases -- f(±0) = ±0, f(±Inf) = ±Inf,
// f(NaN) = NaN -- and differ only on which way a fraction goes.
var roundingSpecialCases = []floatMathCase{
	{"positive zero", positiveZero, positiveZero},
	{"negative zero", negativeZero, negativeZero},
	{"positive infinity", positiveInfinity, positiveInfinity},
	{"negative infinity", negativeInfinity, negativeInfinity},
	{"Go's NaN", goNaN, goNaN},
	{"a negative NaN", negativeNaN, negativeNaN},
	{"a signalling NaN", signallingNaN, quietedSignallingNaN},
	{"the largest finite value", bitsOf(math.MaxFloat64), bitsOf(math.MaxFloat64)},
}

// floorCases: toward -Inf. Note that a fraction between -1 and 0 floors to -1,
// and a fraction between 0 and 1 floors to +0, not to -0.
var floorCases = append([]floatMathCase{
	{"one and a half", bitsOf(1.5), bitsOf(1)},
	{"minus one and a half", bitsOf(-1.5), bitsOf(-2)},
	{"two and a half", bitsOf(2.5), bitsOf(2)},
	{"a half", bitsOf(0.5), positiveZero},
	{"minus a half", bitsOf(-0.5), bitsOf(-1)},
	{"the smallest subnormal", bitsOf(math.SmallestNonzeroFloat64), positiveZero},
	{"the smallest negative subnormal", bitsOf(-math.SmallestNonzeroFloat64), bitsOf(-1)},
	{"two to the fifty-second", bitsOf(4503599627370496), bitsOf(4503599627370496)},
}, roundingSpecialCases...)

// ceilCases: toward +Inf. A fraction between -1 and 0 ceils to -0, which is
// the case a comparison on values could not tell from +0.
var ceilCases = append([]floatMathCase{
	{"one and a half", bitsOf(1.5), bitsOf(2)},
	{"minus one and a half", bitsOf(-1.5), bitsOf(-1)},
	{"two and a half", bitsOf(2.5), bitsOf(3)},
	{"a half", bitsOf(0.5), bitsOf(1)},
	{"minus a half", bitsOf(-0.5), negativeZero},
	{"the smallest subnormal", bitsOf(math.SmallestNonzeroFloat64), bitsOf(1)},
	{"the smallest negative subnormal", bitsOf(-math.SmallestNonzeroFloat64), negativeZero},
}, roundingSpecialCases...)

// truncCases: toward zero, so the sign of a fraction survives as the sign of
// the zero.
var truncCases = append([]floatMathCase{
	{"one and a half", bitsOf(1.5), bitsOf(1)},
	{"minus one and a half", bitsOf(-1.5), bitsOf(-1)},
	{"two and a half", bitsOf(2.5), bitsOf(2)},
	{"a half", bitsOf(0.5), positiveZero},
	{"minus a half", bitsOf(-0.5), negativeZero},
	{"the smallest subnormal", bitsOf(math.SmallestNonzeroFloat64), positiveZero},
	{"the smallest negative subnormal", bitsOf(-math.SmallestNonzeroFloat64), negativeZero},
}, roundingSpecialCases...)

// roundevenCases: to nearest, ties to even -- math.RoundToEven.
var roundevenCases = append([]floatMathCase{
	{"a half", bitsOf(0.5), positiveZero},
	{"minus a half", bitsOf(-0.5), negativeZero},
	{"one and a half", bitsOf(1.5), bitsOf(2)},
	{"minus one and a half", bitsOf(-1.5), bitsOf(-2)},
	{"two and a half", bitsOf(2.5), bitsOf(2)},
	{"minus two and a half", bitsOf(-2.5), bitsOf(-2)},
	{"three and a half", bitsOf(3.5), bitsOf(4)},
	{"the largest double below a half", bitsOf(0.49999999999999994), positiveZero},
	{"one ulp above a half", bitsOf(0.5000000000000001), bitsOf(1)},
	{"the last representable half", bitsOf(4503599627370495.5), bitsOf(4503599627370496)},
}, roundingSpecialCases...)

// roundawayCases: to nearest, ties away from zero -- math.Round. The tie is
// where it parts company with roundeven.
var roundawayCases = append([]floatMathCase{
	{"a half", bitsOf(0.5), bitsOf(1)},
	{"minus a half", bitsOf(-0.5), bitsOf(-1)},
	{"one and a half", bitsOf(1.5), bitsOf(2)},
	{"minus one and a half", bitsOf(-1.5), bitsOf(-2)},
	{"two and a half", bitsOf(2.5), bitsOf(3)},
	{"minus two and a half", bitsOf(-2.5), bitsOf(-3)},
	{"the largest double below a half", bitsOf(0.49999999999999994), positiveZero},
	{"the largest negative double above minus a half", bitsOf(-0.49999999999999994), negativeZero},
	{"the last representable half", bitsOf(4503599627370495.5), bitsOf(4503599627370496)},
}, roundingSpecialCases...)

// TestE2EFloatMathIntrinsics runs each intrinsic on the CPU and compares the
// bits it produced against the specified answer.
func TestE2EFloatMathIntrinsics(t *testing.T) {
	for _, intrinsic := range []struct {
		name  string
		cases []floatMathCase
	}{
		{"float.sqrt.d", sqrtCases},
		{"float.abs.d", absCases},
		{"float.floor.d", floorCases},
		{"float.ceil.d", ceilCases},
		{"float.trunc.d", truncCases},
		{"float.roundeven.d", roundevenCases},
		{"float.roundaway.d", roundawayCases},
	} {
		t.Run(intrinsic.name, func(t *testing.T) {
			runFloatMathCases(t, intrinsic.name, intrinsic.cases)
		})
	}
}

// runFloatMathCases builds a function that applies the intrinsic to a double
// handed in as raw bits and returns the raw bits of its result, then checks
// every case against it in one program.
func runFloatMathCases(t *testing.T, intrinsic string, cases []floatMathCase) {
	t.Helper()

	m := ir.NewModule()
	f := m.NewFunc("applyintrinsic", ir.ClsL).Export()
	operandBits := f.Param("operand", ir.ClsL)
	entry := f.Entry()
	operand := entry.Cast(ir.ClsD, operandBits)
	result := entry.Intrinsic(intrinsic, ir.ClsD, operand)
	entry.Ret(entry.Cast(ir.ClsL, result))

	var checks strings.Builder
	for i, c := range cases {
		fmt.Fprintf(&checks,
			"\tif (applyintrinsic(0x%016xULL) != 0x%016xULL) return %d;\n",
			c.operand, c.expected, i+1)
	}

	stdout, code := buildAndRun(t, m, `
#include <stdint.h>
extern uint64_t applyintrinsic(uint64_t);
int main(void) {
`+checks.String()+`	return 0;
}`)
	require.Empty(t, stdout)
	if code != 0 {
		failed := cases[code-1]
		t.Fatalf("%s of %#016x produced the wrong bits; the specified answer is %#016x (case %q)",
			intrinsic, failed.operand, failed.expected, failed.name)
	}
}

// TestE2EFloatMathIntrinsicsSingle checks the single-precision arm of each
// intrinsic, which goc does not currently emit -- the Go math package is
// float64 throughout -- but the IR and the backend both offer.
func TestE2EFloatMathIntrinsicsSingle(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("singlemath", ir.ClsS).Export()
	x := f.Param("x", ir.ClsS)
	entry := f.Entry()
	// sqrt(abs(trunc(x))): -4.25 -> -4.0 -> 4.0 -> 2.0.
	truncated := entry.Intrinsic("float.trunc.s", ir.ClsS, x)
	magnitude := entry.Intrinsic("float.abs.s", ir.ClsS, truncated)
	entry.Ret(entry.Intrinsic("float.sqrt.s", ir.ClsS, magnitude))

	_, code := buildAndRun(t, m, `
extern float singlemath(float);
int main(void){ return singlemath(-4.25f) == 2.0f ? 0 : 1; }`)
	require.Equal(t, 0, code)
}
