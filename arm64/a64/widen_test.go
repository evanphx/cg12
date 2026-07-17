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

	// System and synchronization, none of which the parser could name. Without
	// the exclusives there is no way to write a compare-and-swap, which is what a
	// lock-free atomic is built from.
	"svc #0", "svc #128",
	"dmb sy", "dmb ish", "dmb ishst", "dmb ld", "dmb st",
	"dsb sy", "dsb ish", "isb",
	"ldxr w0, [x1]", "ldxr x2, [x3]",
	"ldaxr w4, [x5]", "ldaxr x6, [x7]",
	"stxr w0, w1, [x2]", "stxr w3, x4, [x5]",
	"stlxr w6, w7, [x8]", "stlxr w9, x10, [x11]",

	// Register-offset addressing, which only the base+immediate forms could parse.
	"ldr w1, [x2, w3, sxtw #2]", "ldr w4, [x5, x6, lsl #2]",
	"ldr x7, [x8, x9, lsl #3]", "ldr x0, [x1, x2]",
	"str w1, [x2, w3, sxtw #2]", "str x3, [x4, x5, lsl #3]",
	"ldrb w1, [x2, w3, sxtw]", "ldrb w4, [x5, x6]",
	"strb w1, [x2, w3, uxtw]",
	"ldrh w4, [x5, w6, uxtw #1]", "ldrsw x1, [x2, w3, sxtw #2]",

	// System-register transfer, beyond the single tpidr_el0 the parser knew.
	"mrs x0, fpcr", "mrs x1, fpsr", "mrs x2, nzcv",
	"mrs x3, cntvct_el0", "mrs x4, ctr_el0",
	"msr fpcr, x5", "msr nzcv, x6", "msr tpidr_el0, x7",
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
		{"dmb foo", "not a barrier option"},
		{"stxr x0, x1, [x2]", "the status register must be 32-bit"},
		{"ldxr w0, [x1, #8]", "an exclusive access takes no offset"},
		{"svc x0", "svc takes an immediate, not a register"},
		{"isb ish", "only the sy domain is encoded for isb"},
		{"ldr w0, [x1, w2, sxtw #1]", "shift #1 does not match a 4-byte word access"},
		{"ldr x0, [x1, w2]", "a w index needs an extend"},
		{"ldr w0, [x1, x2, ror #2]", "ror is not an index extend"},
	} {
		_, err := Assemble(c.src)
		require.Errorf(t, err, "%q (%s) must not assemble", c.src, c.why)
	}
}
