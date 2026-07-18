package interp_test

import (
	"bytes"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// A variadic function iterates its trailing arguments through va_start/va_arg.
func TestVariadicSum(t *testing.T) {
	il := `export function w $sum(w %n, ...) { @start
		%ap =l alloc8 8
		vastart %ap
		jmp @loop
	@loop
		%i =w phi @start 0, @body %i1
		%t =w phi @start 0, @body %t1
		%done =w csgew %i, %n
		jnz %done, @exit, @body
	@body
		%v =w vaarg %ap
		%t1 =w add %t, %v
		%i1 =w add %i, 1
		jmp @loop
	@exit
		ret %t }`
	require.Equal(t, int64(60),
		run(t, il, "sum", interp.W(3), interp.W(10), interp.W(20), interp.W(30)).I64())
}

// printf renders the C-subset of conversions from the argument list.
func TestPrintf(t *testing.T) {
	il := `data $fmt = { b "n=%d s=%s c=%c x=%x", b 0 }
	data $str = { b "hi", b 0 }
	export function w $go() { @start
		%r =w call $printf(l $fmt, w 42, l $str, w 65, w 255, ...)
		ret %r }`
	m, err := parse.Parse(il)
	require.NoError(t, err)
	var buf bytes.Buffer
	mc, err := interp.New(m, interp.WithStdout(&buf))
	require.NoError(t, err)
	n, err := mc.Call("go")
	require.NoError(t, err)
	require.Equal(t, "n=42 s=hi c=A x=ff", buf.String())
	require.Equal(t, int64(len(buf.String())), n.I64())
}

// printf field width and zero-padding.
func TestPrintfWidth(t *testing.T) {
	il := `data $fmt = { b "[%5d][%-5d][%05d]", b 0 }
	export function w $go() { @start
		%r =w call $printf(l $fmt, w 42, w 42, w 42, ...)
		ret %r }`
	m, _ := parse.Parse(il)
	var buf bytes.Buffer
	mc, _ := interp.New(m, interp.WithStdout(&buf))
	_, err := mc.Call("go")
	require.NoError(t, err)
	require.Equal(t, "[   42][42   ][00042]", buf.String())
}
