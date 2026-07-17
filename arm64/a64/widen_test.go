package a64

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The encoders for these have been in this package all along -- validated
// byte-for-byte against the reference assembler by TestEncodingsMatchAssembler
// -- and the parser simply could not reach them. `mov d0, d1` was refused, so a
// double could not go through inline asm at all.
//
// Each line is assembled by us and by the reference assembler, and the bytes
// must agree. That is the same question the encoders answer, asked of the
// parser: it is no use reaching an encoder with the wrong operands.
var widenedCases = []string{
	// Floating point.
	"fadd s0, s1, s2", "fadd d0, d1, d2",
	"fsub s3, s4, s5", "fsub d3, d4, d5",
	"fmul s6, s7, s8", "fmul d6, d7, d8",
	"fdiv s0, s1, s2", "fdiv d0, d1, d2",
	"fneg s0, s1", "fneg d2, d3",
	"fcmp s0, s1", "fcmp d2, d3",
	"fcvt d0, s1", "fcvt s2, d3",
	"fmov s0, s1", "fmov d4, d5",
	"fmov s0, w1", "fmov d2, x3",
	"fmov w0, s1", "fmov x2, d3",
	"scvtf s0, w1", "scvtf d2, x3", "scvtf d0, w1", "scvtf s2, x3",
	"ucvtf s4, w5", "ucvtf d6, x7",
	"fcvtzs w0, s1", "fcvtzs x2, d3", "fcvtzu w4, s5", "fcvtzu x6, d7",

	// Integer forms the parser could not reach either.
	"br x1", "blr x2",
	"clz w1, w2", "clz x3, x4",
	"sxtb w0, w1", "sxtb x2, w3", "sxth w4, w5", "sxtw x6, w7",
	"uxtb w0, w1", "uxth w2, w3",
	"madd x1, x2, x3, x4", "msub w5, w6, w7, w8",
	"extr w1, w2, w3, #5", "extr x1, x2, x3, #40",
	"ror w9, w10, #7", "ror x11, x12, #40",
	"csel x1, x2, x3, eq", "csel w4, w5, w6, lt",
	"cset w1, ne", "cset x2, ge",
	"mrs x0, tpidr_el0",
	"ldp x29, x30, [sp, #16]", "stp x0, x1, [x2, #16]", "stp w3, w4, [x5, #8]",
}

func TestWidenedParsingMatchesAssembler(t *testing.T) {
	as, objcopy := tools(t)
	text := assemble(t, as, objcopy, widenedCases)
	require.Len(t, text, 4*len(widenedCases), "one word per instruction")

	for i, src := range widenedCases {
		got, err := Assemble(src)
		require.NoErrorf(t, err, "we cannot assemble %q", src)
		require.Lenf(t, got, 4, "%q is one instruction", src)
		want := binary.LittleEndian.Uint32(text[4*i:])
		assert.Equalf(t, want, binary.LittleEndian.Uint32(got),
			"%s: assembler=%#08x ours=%#08x (%s)", src, want, binary.LittleEndian.Uint32(got), Disasm(binary.LittleEndian.Uint32(got)))
	}
}

// Reaching an encoder is not enough: the operands have to mean what they say.
// The register files share an encoding, so a mismatched pair does not fail to
// assemble, it assembles onto something else.
func TestWidenedFormsRejectBadOperands(t *testing.T) {
	for _, c := range []struct{ src, why string }{
		{"fadd s0, d1, s2", "mixed widths"},
		{"fadd x0, x1, x2", "general registers in an FP instruction"},
		{"fcvt s0, s1", "fcvt between the same width converts nothing"},
		{"fmov x0, x1", "neither operand is an FP register"},
		{"fmov d0, w1", "mismatched widths across the register files"},
		{"sxtb w0, x1", "an extend reads a w register"},
		{"sxtw w0, w1", "sxtw widens to an x"},
		{"csel x1, x2, x3, zz", "not a condition"},
		{"cset w1, nope", "not a condition"},
		{"mrs x0, ttbr0_el1", "a system register we do not encode"},
		{"mrs w0, tpidr_el0", "a system register read gives an x"},
		{"stp x0, w1, [x2, #16]", "mismatched widths"},
	} {
		_, err := Assemble(c.src)
		require.Errorf(t, err, "%q (%s) must not assemble", c.src, c.why)
	}
}
