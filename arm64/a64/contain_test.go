package a64

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The integer and FP register files share the same 5-bit encoding, so naming a
// d register where an x register belongs does not fail to assemble -- it
// assembles to an instruction on an entirely different register. `mov d0, d1`
// was `mov x0, x1`, which is why a double reaching this through an "r"
// constraint came back as 0.0 rather than as a diagnostic.
//
// The FP encoders exist (Fadd, FmovReg, LdrFP and the rest); this parser does
// not reach them yet, and saying so is the honest answer until it does.
func TestFloatRegistersAreRejectedNotMisEncoded(t *testing.T) {
	for _, src := range []string{
		"mov d0, d1", "mov s0, s1", "add d0, d1, d2", "ldr d0, [x1]",
		"str s0, [x1]", "sub d0, d1, d2", "ldr x0, [d1]",
	} {
		_, err := Assemble(src)
		require.Errorf(t, err, "%q must not assemble to an integer instruction", src)
	}
}

// The integer forms still work.
func TestIntegerFormsStillAssemble(t *testing.T) {
	for _, src := range []string{
		"mov x0, x1", "add x0, x1, x2", "ldr x0, [x1]", "str w0, [x1, #8]",
		"cmp x0, x1", "lsl x0, x1, #3", "ret",
	} {
		_, err := Assemble(src)
		require.NoErrorf(t, err, "%q should assemble", src)
	}
}

// GAS separates statements with ';' on AArch64 too -- the comment markers are
// '//' and '#'. Truncating at ';' silently dropped the rest of the line.
func TestSemicolonSeparatesStatements(t *testing.T) {
	one, err := Assemble("mov x0, x1")
	require.NoError(t, err)
	two, err := Assemble("mov x0, x1; add x0, x0, #1")
	require.NoError(t, err)
	require.Len(t, two, len(one)+4, "the second instruction must be encoded too")

	sep, err := Assemble("mov x0, x1\nadd x0, x0, #1")
	require.NoError(t, err)
	require.Equal(t, sep, two, "';' and a newline must mean the same thing")

	c, err := Assemble("mov x0, x1 // add x0, x0, #1")
	require.NoError(t, err)
	require.Equal(t, one, c, "'//' really is a comment")
}
