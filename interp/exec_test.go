package interp_test

import (
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// run parses IL, loads it, and calls one exported function, asserting no error.
func run(t *testing.T, il, fn string, args ...interp.Value) interp.Value {
	t.Helper()
	m, err := parse.Parse(il)
	require.NoError(t, err, "parse")
	mc, err := interp.New(m)
	require.NoError(t, err, "load")
	v, err := mc.Call(fn, args...)
	require.NoError(t, err, "call")
	return v
}

// The scalar corpus, run through the interpreter with no backend or toolchain.
// These are the cg12 corpus cases whose C driver passes scalars and prints a
// scalar (difftest/corpus_test.go): the interpreter drives the exported function
// directly and checks its return value.
func TestScalarCorpus(t *testing.T) {
	add := `export function w $add(w %a, w %b) { @start
		%r =w add %a, %b
		ret %r }`
	require.Equal(t, int64(42), run(t, add, "add", interp.W(17), interp.W(25)).I64())

	fact := `export function l $fact(w %n) { @start
		%nl =l extsw %n
		jmp @loop
	@loop
		%i =l phi @start 1, @body %i1
		%acc =l phi @start 1, @body %acc1
		%done =w csgtl %i, %nl
		jnz %done, @exit, @body
	@body
		%acc1 =l mul %acc, %i
		%i1 =l add %i, 1
		jmp @loop
	@exit
		ret %acc }`
	require.Equal(t, int64(3628800), run(t, fact, "fact", interp.W(10)).I64())

	fib := `export function w $fib(w %n) { @start
		%c =w csltw %n, 2
		jnz %c, @base, @rec
	@base
		ret %n
	@rec
		%n1 =w sub %n, 1
		%a =w call $fib(w %n1)
		%n2 =w sub %n, 2
		%b =w call $fib(w %n2)
		%r =w add %a, %b
		ret %r }`
	require.Equal(t, int64(610), run(t, fib, "fib", interp.W(15)).I64())

	// gcd exercises the parallel-phi swap (%x<-%y, %y<-%r) that a naive
	// sequential transfer would corrupt.
	gcd := `export function w $gcd(w %a, w %b) { @start
		jmp @loop
	@loop
		%x =w phi @start %a, @body %y
		%y =w phi @start %b, @body %r
		%z =w ceqw %y, 0
		jnz %z, @exit, @body
	@body
		%r =w rem %x, %y
		jmp @loop
	@exit
		ret %x }`
	require.Equal(t, int64(21), run(t, gcd, "gcd", interp.W(1071), interp.W(462)).I64())

	darith := `export function d $darith(d %a, d %b, d %c) { @start
		%t =d sub %a, %b
		%r =d div %t, %c
		ret %r }`
	require.InDelta(t, 2.0, run(t, darith, "darith", interp.D(10), interp.D(4), interp.D(3)).F64(), 1e-12)
}

// Shift semantics: the amount masks by the class width (opt/fold.go:201), so a
// shift "beyond word width" wraps the amount rather than zeroing.
func TestShiftMasking(t *testing.T) {
	il := `export function l $f(l %x, w %n) { @start
		%nl =l extuw %n
		%r =l shl %x, %nl
		ret %r }`
	// shl by 64 masks to 0: x unchanged.
	require.Equal(t, int64(7), run(t, il, "f", interp.L(7), interp.W(64)).I64())
	// shl by 65 masks to 1: x*2.
	require.Equal(t, int64(14), run(t, il, "f", interp.L(7), interp.W(65)).I64())

	// Word shift masks to 31.
	ilw := `export function w $g(w %x, w %n) { @start
		%r =w shl %x, %n
		ret %r }`
	require.Equal(t, int64(1), run(t, ilw, "g", interp.W(1), interp.W(32)).I64()) // mask 32->0
}

// Division: signed truncates toward zero; MinInt/-1 does not trap; div-by-zero
// traps by default.
func TestDivision(t *testing.T) {
	sdiv := `export function w $d(w %a, w %b) { @start
		%r =w div %a, %b
		ret %r }`
	require.Equal(t, int64(-3), run(t, sdiv, "d", interp.W(-17), interp.W(5)).I64()) // trunc toward zero

	// MinInt32 / -1 = MinInt32 (overflow wraps, no trap).
	require.Equal(t, int64(-2147483648), run(t, sdiv, "d", interp.W(-2147483648), interp.W(-1)).I64())

	// div by zero traps.
	m, err := parse.Parse(sdiv)
	require.NoError(t, err)
	mc, err := interp.New(m)
	require.NoError(t, err)
	_, err = mc.Call("d", interp.W(5), interp.W(0))
	require.Error(t, err)
	require.Contains(t, err.Error(), "divide by zero")

	// ... unless WithDivZeroYieldsZero (arm64 sdiv behaviour).
	mc2, err := interp.New(m, interp.WithDivZeroYieldsZero())
	require.NoError(t, err)
	v, err := mc2.Call("d", interp.W(5), interp.W(0))
	require.NoError(t, err)
	require.Equal(t, int64(0), v.I64())
}

// Word arithmetic wraps and stays sign-extended.
func TestWordWrap(t *testing.T) {
	il := `export function w $f(w %x) { @start
		%r =w add %x, 1
		ret %r }`
	require.Equal(t, int64(-2147483648), run(t, il, "f", interp.W(2147483647)).I64())
}

// A lowered module is refused at load.
func TestRefusesLowered(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsW)
	f.Entry().Ret(f.Word(1))
	require.NoError(t, f.MarkLowered("arm64"))
	_, err := interp.New(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "lowered for arm64")
}
