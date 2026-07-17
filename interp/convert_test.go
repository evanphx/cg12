package interp_test

import (
	"math"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/stretchr/testify/require"
)

// Float->int conversion is round-toward-zero and saturating (AArch64 fcvtz*),
// not Go's undefined out-of-range conversion. These are the interpreter's most
// error-prone semantics, so they are pinned with explicit vectors including
// ±Inf, NaN, and the class boundaries.
func TestFloatToIntSaturation(t *testing.T) {
	d2iw := `export function w $f(d %x) { @start %r =w dtosi %x  ret %r }`
	inf := math.Inf(1)
	for _, c := range []struct {
		in   float64
		want int64 // signed 32-bit result
	}{
		{3.9, 3}, {-3.9, -3}, {0, 0}, {-0.0, 0},
		{math.NaN(), 0},
		{inf, math.MaxInt32}, {-inf, math.MinInt32},
		{1e18, math.MaxInt32}, {-1e18, math.MinInt32},
		{2147483647.9, math.MaxInt32}, // just below 2^31: truncates in range
		{2147483648.0, math.MaxInt32}, // 2^31: saturates
		{-2147483648.0, math.MinInt32},
	} {
		got := run(t, d2iw, "f", interp.D(c.in)).I64()
		require.Equalf(t, c.want, got, "dtosi w %v", c.in)
	}

	d2il := `export function l $f(d %x) { @start %r =l dtosi %x  ret %r }`
	for _, c := range []struct {
		in   float64
		want int64
	}{
		{math.NaN(), 0}, {inf, math.MaxInt64}, {-inf, math.MinInt64},
		{1e30, math.MaxInt64}, {-1e30, math.MinInt64},
		{9.0e18, 9000000000000000000},
	} {
		require.Equalf(t, c.want, run(t, d2il, "f", interp.D(c.in)).I64(), "dtosi l %v", c.in)
	}

	d2uw := `export function w $f(d %x) { @start %r =w dtoui %x  ret %r }`
	for _, c := range []struct {
		in   float64
		want uint64 // unsigned 32-bit result
	}{
		{3.9, 3}, {-1.5, 0}, {-0.5, 0}, {math.NaN(), 0}, {-inf, 0},
		{inf, math.MaxUint32}, {1e18, math.MaxUint32},
		{4294967295.0, math.MaxUint32}, {4294967296.0, math.MaxUint32},
	} {
		require.Equalf(t, c.want, run(t, d2uw, "f", interp.D(c.in)).U64(), "dtoui w %v", c.in)
	}

	d2ul := `export function l $f(d %x) { @start %r =l dtoui %x  ret %r }`
	require.Equal(t, uint64(math.MaxUint64), run(t, d2ul, "f", interp.D(1e30)).U64())
	require.Equal(t, uint64(0), run(t, d2ul, "f", interp.D(-5)).U64())
}

// Int->float rounds to nearest even (scvtf/ucvtf). The unsigned path must treat
// a large word as unsigned, not as a negative signed value.
func TestIntToFloat(t *testing.T) {
	s2d := `export function d $f(l %x) { @start %r =d sltof %x  ret %r }`
	require.Equal(t, -3.0, run(t, s2d, "f", interp.L(-3)).F64())

	u2d := `export function d $f(l %x) { @start %r =d ultof %x  ret %r }`
	// 0xFFFFFFFFFFFFFFFF as unsigned is 1.8e19, not -1.0.
	require.InDelta(t, 1.8446744073709552e19, run(t, u2d, "f", interp.L(-1)).F64(), 1e4)
}

// Float compares honour IEEE ordered/unordered NaN semantics.
func TestFloatCompareNaN(t *testing.T) {
	nan, one := math.NaN(), 1.0
	pred := func(mn string) string {
		return `export function w $f(d %a, d %b) { @start %r =w ` + mn + ` %a, %b  ret %r }`
	}
	// ordered eq: false if either NaN.
	require.Equal(t, int64(0), run(t, pred("ceqd"), "f", interp.D(nan), interp.D(one)).I64())
	require.Equal(t, int64(1), run(t, pred("ceqd"), "f", interp.D(one), interp.D(one)).I64())
	// unordered ne: true if either NaN.
	require.Equal(t, int64(1), run(t, pred("cned"), "f", interp.D(nan), interp.D(one)).I64())
	// ordered lt/le/gt/ge: false if either NaN.
	require.Equal(t, int64(0), run(t, pred("cltd"), "f", interp.D(nan), interp.D(one)).I64())
	require.Equal(t, int64(0), run(t, pred("cged"), "f", interp.D(nan), interp.D(one)).I64())
	// ordered predicate: true iff neither NaN.
	require.Equal(t, int64(0), run(t, pred("cod"), "f", interp.D(nan), interp.D(one)).I64())
	require.Equal(t, int64(1), run(t, pred("cod"), "f", interp.D(one), interp.D(one)).I64())
	// unordered predicate: true iff some NaN.
	require.Equal(t, int64(1), run(t, pred("cuod"), "f", interp.D(nan), interp.D(one)).I64())
	require.Equal(t, int64(0), run(t, pred("cuod"), "f", interp.D(one), interp.D(one)).I64())
}

// float32 arithmetic rounds per operation (no double rounding through float64).
func TestSinglePrecisionRounding(t *testing.T) {
	// (1 + 2^-24) as float32 rounds to 1.0; adding it to itself stays 1.0-ish.
	il := `export function s $f(s %a, s %b) { @start %r =s add %a, %b  ret %r }`
	got := run(t, il, "f", interp.S(1<<23), interp.S(1)).F32()
	require.Equal(t, float32(1<<23)+1, got)
}
