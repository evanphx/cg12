package interp_test

import (
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/stretchr/testify/require"
)

// Stores and loads round-trip through memory, with the load op fixing width and
// signedness.
func TestMemoryRoundTrip(t *testing.T) {
	sb := `export function w $f(w %x) { @start
		%p =l alloc4 4
		storeb %x, %p
		%r =w loadsb %p
		ret %r }`
	require.Equal(t, int64(-1), run(t, sb, "f", interp.W(255)).I64()) // signed byte
	require.Equal(t, int64(127), run(t, sb, "f", interp.W(127)).I64())

	ub := `export function w $f(w %x) { @start
		%p =l alloc4 4
		storeb %x, %p
		%r =w loadub %p
		ret %r }`
	require.Equal(t, int64(255), run(t, ub, "f", interp.W(255)).I64()) // zero-extended

	rl := `export function l $f(l %x) { @start
		%p =l alloc8 8
		storel %x, %p
		%r =l loadl %p
		ret %r }`
	require.Equal(t, int64(0x123456789A), run(t, rl, "f", interp.L(0x123456789A)).I64())

	// Halfword and word widths.
	rh := `export function w $f(w %x) { @start
		%p =l alloc4 4
		storeh %x, %p
		%r =w loaduh %p
		ret %r }`
	require.Equal(t, int64(0xBEEF), run(t, rh, "f", interp.W(0x1BEEF)).I64()) // low 16 bits

	// A double round-trips bit-for-bit.
	rd := `export function d $f(d %x) { @start
		%p =l alloc8 8
		stored %x, %p
		%r =d loadd %p
		ret %r }`
	require.Equal(t, 3.14159, run(t, rd, "f", interp.D(3.14159)).F64())
}

// A global initializer is laid out in memory and read back through pointer
// arithmetic on the symbol's address.
func TestGlobalLoadBack(t *testing.T) {
	il := `data $arr = align 8 { l 10, l 20, l 30 }
	export function l $get(w %i) { @start
		%o =l extsw %i
		%o8 =l mul %o, 8
		%p =l add $arr, %o8
		%r =l loadl %p
		ret %r }`
	require.Equal(t, int64(10), run(t, il, "get", interp.W(0)).I64())
	require.Equal(t, int64(20), run(t, il, "get", interp.W(1)).I64())
	require.Equal(t, int64(30), run(t, il, "get", interp.W(2)).I64())
}

// A datum that holds the address of another global (a Sym item) relocates to a
// real pointer, so following it reaches the target.
func TestGlobalPointerInit(t *testing.T) {
	il := `data $target = { w 42 }
	data $ptr = align 8 { l $target }
	export function w $deref() { @start
		%pp =l loadl $ptr
		%r =w loaduw %pp
		ret %r }`
	require.Equal(t, int64(42), run(t, il, "deref").I64())
}
