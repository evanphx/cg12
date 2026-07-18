package main

import (
	"testing"

	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// decodeModule accepts a unit as either text IL or the binary form, which is what
// lets the tools pipe binary while still taking text input.
func TestDecodeModuleAutodetect(t *testing.T) {
	const src = `export function l $f(l %a, l %b) { @s %r =l add %a, %b  ret %r }`

	m, err := parse.Parse(src)
	require.NoError(t, err)
	bin, err := m.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, binMagic, string(bin[:len(binMagic)]), "binary unit starts with the magic")

	fromText, err := decodeModule([]byte(src))
	require.NoError(t, err)
	fromBin, err := decodeModule(bin)
	require.NoError(t, err)

	// Both routes yield the same module.
	require.Equal(t, fromText.String(), fromBin.String())
	require.Contains(t, fromText.String(), "function l $f")
}
