package interp

import (
	"testing"

	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// The register window is packed by liveness: a chain of short-lived temporaries,
// only two of which are ever live at once, compiles into a handful of slots
// rather than one slot per temporary.
func TestBytecodePacksRegisters(t *testing.T) {
	il := `export function w $f(w %x) {
		@s
		%a =w add %x, 1
		%b =w add %a, 1
		%c =w add %b, 1
		%d =w add %c, 1
		%e =w add %d, 1
		%g =w add %e, 1
		%h =w add %g, 1
		%i =w add %h, 1
		ret %i
	}`
	m, err := parse.Parse(il)
	require.NoError(t, err)
	mc, err := New(m)
	require.NoError(t, err)
	f := mc.funcs["f"]
	bf, err := mc.compile(f)
	require.NoError(t, err)

	// 8 temporaries + a parameter + the constant 1: a one-slot-per-value layout
	// would need 10. Liveness packing folds the chain into far fewer.
	require.Less(t, int(bf.frameSize), len(f.Temps),
		"expected the window (%d) to be smaller than the temp count (%d)", bf.frameSize, len(f.Temps))
	require.LessOrEqual(t, int(bf.frameSize), 5, "chain should pack into a small window")

	// Packing must not change the result: both engines agree.
	tw, err := New(m)
	require.NoError(t, err)
	got, err := tw.Call("f", W(10))
	require.NoError(t, err)

	vm, err := New(m, WithBytecode())
	require.NoError(t, err)
	gotVM, err := vm.Call("f", W(10))
	require.NoError(t, err)
	require.Equal(t, got, gotVM)
}
