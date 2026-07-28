package amd64_test

import (
	"math"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// Execution tests for the float/integer conversions, and in particular for the
// unsigned directions -- OStoui and OUltof -- which x86-64 has no single
// instruction for. Every expected value here is computed by Go itself
// (uint64(f) / float64(u) / math.Float64bits), never written out by hand, so the
// test asserts against the language's own semantics rather than against a
// re-derivation of the sequence under test.

// runConvCases compiles and runs a module built around a single conversion. The
// conversion lives in a one-parameter helper function `conv` (class argCls in,
// resCls out, body building the conversion itself) rather than inline in the
// entry block, so the value genuinely arrives in a register at run time instead
// of being resolved as an immediate operand.
//
// `runtest` then calls conv once per case and folds one bit per mismatching case
// into its return value, so the process exit code is a bitmask naming exactly
// which cases went wrong -- 0 when all of them agree. resCls must be an integer
// class, since the comparison against the expected value has to be exact.
func runConvCases(t *testing.T, argCls, resCls ir.Cls, body func(b *ir.Block, x ir.Ref) ir.Ref, cases func(f *ir.Func) (args, wants []ir.Ref)) int {
	t.Helper()
	m := ir.NewModule()

	conv := m.NewFunc("conv", resCls)
	x := conv.Param("x", argCls)
	cb := conv.Entry()
	cb.Ret(body(cb, x))

	f, e := entry(m)
	args, wants := cases(f)
	require.Equal(t, len(args), len(wants), "one expected value per case")
	require.LessOrEqual(t, len(args), 8, "the exit code carries one bit per case")

	acc := f.Word(0)
	for i := range args {
		got := e.Call(resCls, f.Sym("conv", 0), args[i])
		mismatch := e.Cmp(ir.CmpNe, ir.ClsW, got, wants[i])
		acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, mismatch, f.Word(int64(i))))
	}
	e.Ret(acc)
	return runObj(t, m)
}

// f64ToU64Values spans the range where the signed and unsigned truncations
// differ: everything below 2^63 is handled by a plain signed truncate, while at
// or above it a signed truncate yields the x87 "integer indefinite" value
// 0x8000000000000000 rather than the value asked for.
var f64ToU64Values = []float64{
	0,
	1.5,
	4.2e9,                  // past 2^32, still far below 2^63
	9223372036854774784.0,  // the largest double strictly below 2^63
	9223372036854775808.0,  // exactly 2^63: the boundary itself
	1.5e19,                 // MSB set -- a signed truncate gets this wrong
	18446744073709549568.0, // the largest double below 2^64, i.e. max u64
}

func TestObjFloat64ToUint64(t *testing.T) {
	code := runConvCases(t, ir.ClsD, ir.ClsL,
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Stoui(ir.ClsL, x) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, v := range f64ToU64Values {
				args = append(args, f.Double(v))
				wants = append(wants, f.Long(int64(uint64(v))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = f64ToU64Values[i])")
}

// The single-precision source has its own instruction (cvttss2si) and its own
// bias constant, so it needs its own coverage; the values are chosen to be
// exactly representable as float32.
var f32ToU64Values = []float32{
	0,
	1.5,
	4.2e9,
	9223371487098961920.0,  // the largest float32 strictly below 2^63
	9223372036854775808.0,  // exactly 2^63
	1.5e19,                 // MSB set
	18446742974197923840.0, // the largest float32 below 2^64
}

func TestObjFloat32ToUint64(t *testing.T) {
	code := runConvCases(t, ir.ClsS, ir.ClsL,
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Stoui(ir.ClsL, x) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, v := range f32ToU64Values {
				args = append(args, f.Single(float64(v)))
				wants = append(wants, f.Long(int64(uint64(v))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = f32ToU64Values[i])")
}

// u64ToFloatValues cover both sides of the signed/unsigned split for the reverse
// direction. 2^63+1025 is there specifically because it is the smallest value
// whose correctly rounded double needs the low bit preserved through the halving
// step: dropping it converts to 9223372036854775808 instead of
// 9223372036854777856.
var u64ToFloatValues = []uint64{
	0,
	1,
	1 << 31,
	1 << 62,
	1 << 63,          // exactly 2^63: MSB set, the boundary
	(1 << 63) + 1025, // MSB set and needs the sticky low bit to round correctly
	math.MaxUint64,
}

func TestObjUint64ToFloat64(t *testing.T) {
	code := runConvCases(t, ir.ClsL, ir.ClsL,
		// Compare the result's bit pattern, so the check is exact rather than
		// approximate: a wrongly signed or wrongly rounded double never matches.
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Cast(ir.ClsL, b.Ultof(ir.ClsD, x)) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, u := range u64ToFloatValues {
				args = append(args, f.Long(int64(u)))
				wants = append(wants, f.Long(int64(math.Float64bits(float64(u)))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = u64ToFloatValues[i])")
}

func TestObjUint64ToFloat32(t *testing.T) {
	code := runConvCases(t, ir.ClsL, ir.ClsW,
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Cast(ir.ClsW, b.Ultof(ir.ClsS, x)) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, u := range u64ToFloatValues {
				args = append(args, f.Long(int64(u)))
				wants = append(wants, f.Word(int64(math.Float32bits(float32(u)))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = u64ToFloatValues[i])")
}

// The 32-bit unsigned conversions are a separate path: a u32 result always fits
// in an int64, and a u32 source is always a non-negative int64, so both ride on
// the 64-bit signed instruction -- but only if the width actually used is 64 and
// not the result's own 32.
var f64ToU32Values = []float64{
	0,
	1.5,
	2147483647.0, // 2^31 - 1: the last value a signed 32-bit truncate gets right
	2147483648.0, // 2^31: MSB of the u32 result set
	3000000000.0,
	4294967295.0, // max u32
}

func TestObjFloat64ToUint32(t *testing.T) {
	code := runConvCases(t, ir.ClsD, ir.ClsW,
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Stoui(ir.ClsW, x) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, v := range f64ToU32Values {
				args = append(args, f.Double(v))
				wants = append(wants, f.Word(int64(uint32(v))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = f64ToU32Values[i])")
}

func TestObjFloat32ToUint32(t *testing.T) {
	values := []float32{0, 1.5, 2147483647.0, 2147483648.0, 3000000000.0, 4294967040.0}
	code := runConvCases(t, ir.ClsS, ir.ClsW,
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Stoui(ir.ClsW, x) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, v := range values {
				args = append(args, f.Single(float64(v)))
				wants = append(wants, f.Word(int64(uint32(v))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases")
}

var u32ToFloatValues = []uint32{0, 1, 1 << 30, 1 << 31, 0x80000001, math.MaxUint32}

func TestObjUint32ToFloat64(t *testing.T) {
	code := runConvCases(t, ir.ClsW, ir.ClsL,
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Cast(ir.ClsL, b.Ultof(ir.ClsD, x)) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, u := range u32ToFloatValues {
				args = append(args, f.Word(int64(u)))
				wants = append(wants, f.Long(int64(math.Float64bits(float64(u)))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = u32ToFloatValues[i])")
}

func TestObjUint32ToFloat32(t *testing.T) {
	code := runConvCases(t, ir.ClsW, ir.ClsW,
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Cast(ir.ClsW, b.Ultof(ir.ClsS, x)) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, u := range u32ToFloatValues {
				args = append(args, f.Word(int64(u)))
				wants = append(wants, f.Word(int64(math.Float32bits(float32(u)))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = u32ToFloatValues[i])")
}

// The signed conversions share the selection path with the unsigned ones, so
// splitting them apart has to leave negative values working: these are the cases
// an unsigned bias sequence would break.
func TestObjFloatSignedConversionsStillSigned(t *testing.T) {
	signedValues := []int64{0, -1, 1, -2147483648, -9223372036854775808, 9223372036854775807}
	code := runConvCases(t, ir.ClsL, ir.ClsL,
		// i64 -> double -> i64 loses precision at the extremes, so check the double
		// itself: round-tripping through Go's float64() gives the expected bits.
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Cast(ir.ClsL, b.Sltof(ir.ClsD, x)) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, v := range signedValues {
				args = append(args, f.Long(v))
				wants = append(wants, f.Long(int64(math.Float64bits(float64(v)))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "signed int -> double regressed")

	code = runConvCases(t, ir.ClsD, ir.ClsL,
		func(b *ir.Block, x ir.Ref) ir.Ref { return b.Stosi(ir.ClsL, x) },
		func(f *ir.Func) (args, wants []ir.Ref) {
			for _, v := range []float64{0, -1.5, 1.5, -2147483648.5, -9.2e18} {
				args = append(args, f.Double(v))
				wants = append(wants, f.Long(int64(v)))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "double -> signed int regressed")
}

// The conversions above all read their source out of an argument register. This
// one drives them from immediates in the same block instead, which is the path
// where the operand is materialized into the very scratch registers the bias
// sequences use -- a sequence that clobbered the source or its own temporary
// would pass the tests above and fail here.
func TestObjUnsignedConvertConstantOperands(t *testing.T) {
	big := uint64(1)<<63 + 1025 // a variable, so Go computes the conversions at run time

	m := ir.NewModule()
	f, e := entry(m)

	// double -> u64 on a value with the MSB set, then straight back to a double,
	// which is exact for this value: both directions have to agree.
	toU64 := e.Stoui(ir.ClsL, f.Double(float64(big)))
	backToF64 := e.Cast(ir.ClsL, e.Ultof(ir.ClsD, toU64))
	wrongDown := e.Cmp(ir.CmpNe, ir.ClsW, toU64, f.Long(int64(uint64(float64(big)))))
	wrongUp := e.Cmp(ir.CmpNe, ir.ClsW, backToF64, f.Long(int64(math.Float64bits(float64(big)))))

	// The same round trip through single precision.
	toU64S := e.Stoui(ir.ClsL, f.Single(float64(float32(big))))
	wrongDownS := e.Cmp(ir.CmpNe, ir.ClsW, toU64S, f.Long(int64(uint64(float32(big)))))
	backToF32 := e.Cast(ir.ClsW, e.Ultof(ir.ClsS, f.Long(int64(big))))
	wrongUpS := e.Cmp(ir.CmpNe, ir.ClsW, backToF32, f.Word(int64(math.Float32bits(float32(big)))))

	acc := e.Or(ir.ClsW, e.Shl(ir.ClsW, wrongDown, f.Word(0)), e.Shl(ir.ClsW, wrongUp, f.Word(1)))
	acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, wrongDownS, f.Word(2)))
	acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, wrongUpS, f.Word(3)))
	e.Ret(acc)

	require.Equal(t, 0, runObj(t, m))
}

// A spilled operand is read into the same XMM scratch the bias sequence stages
// its biased copy in, and a spilled result is built in the same GPR scratch the
// commit stores back from -- so both kinds of spill have to be exercised, not just
// the allocated-register case. System V has no callee-saved XMM, so a float
// carried across a call is always spilled, and there are only five callee-saved
// GPRs, so six integers carried across one cannot all stay in registers either.
func TestObjUnsignedConvertSpilledValues(t *testing.T) {
	values := []uint64{
		1,
		1 << 31,
		1 << 62,
		1 << 63,
		(1 << 63) + 1025,
		18446744073709549568, // 2^64 - 2048, the largest exactly representable u64
	}

	m := ir.NewModule()
	nop := m.NewFunc("nop", ir.ClsW)
	nb := nop.Entry()
	nb.Ret(nop.Word(0))

	f, e := entry(m)
	var doubles []ir.Ref
	for _, u := range values {
		doubles = append(doubles, e.Ultof(ir.ClsD, f.Long(int64(u))))
	}
	e.Call(ir.ClsW, f.Sym("nop", 0)) // every double is now spilled
	var back []ir.Ref
	for _, d := range doubles {
		back = append(back, e.Stoui(ir.ClsL, d))
	}
	e.Call(ir.ClsW, f.Sym("nop", 0)) // and now the integer results are under pressure

	acc := f.Word(0)
	for i, u := range values {
		// Not a round trip back to u: converting to a double loses low bits for the
		// larger values, and Go's own uint64(float64(u)) says what should come back.
		want := f.Long(int64(uint64(float64(u))))
		mismatch := e.Cmp(ir.CmpNe, ir.ClsW, back[i], want)
		acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, mismatch, f.Word(int64(i))))
	}
	e.Ret(acc)

	require.Equal(t, 0, runObj(t, m))
}
