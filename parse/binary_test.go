package parse_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBinaryRoundTripParsed parses textual IL, caches it to bytes, reloads it,
// and checks the reloaded module prints identically — parsed units (with
// multi-block control flow, phis, and loops) survive the binary format.
func TestBinaryRoundTripParsed(t *testing.T) {
	const il = `export function w $sumto(w %n) {
@start
	jmp @loop
@loop
	%i =w phi @start 0, @body %i2
	%s =w phi @start 0, @body %s2
	%c =w csgew %i, %n
	jnz %c, @exit, @body
@body
	%s2 =w add %s, %i
	%i2 =w add %i, 1
	jmp @loop
@exit
	ret %s
}`
	m, err := parse.Parse(il)
	require.NoError(t, err)

	data, err := m.MarshalBinary()
	require.NoError(t, err)
	m2, err := ir.DecodeModule(data)
	require.NoError(t, err)

	assert.Equal(t, m.String(), m2.String())
}
