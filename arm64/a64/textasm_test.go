package a64

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// word decodes the n-th 32-bit little-endian instruction word.
func word(t *testing.T, b []byte, n int) uint32 {
	t.Helper()
	require.GreaterOrEqual(t, len(b), (n+1)*4)
	return binary.LittleEndian.Uint32(b[n*4:])
}

// TestAssembleEncodings assembles single instructions and checks each against
// the direct encoder, confirming the text assembler and the compiler's
// machine-code path share one encoding.
func TestAssembleEncodings(t *testing.T) {
	cases := []struct {
		asm  string
		want uint32
	}{
		{"add x0, x1, x2", AddReg(true, 0, 1, 2)},
		{"add w3, w4, #5", AddImm(false, 3, 4, 5)},
		{"sub x9, x10, x11", SubReg(true, 9, 10, 11)},
		{"mul x0, x1, x2", Mul(true, 0, 1, 2)},
		{"udiv w0, w1, w2", Udiv(false, 0, 1, 2)},
		{"and x5, x6, x7", AndReg(true, 5, 6, 7)},
		{"orr w0, w1, w2", OrrReg(false, 0, 1, 2)},
		{"eor x0, x1, x2", EorReg(true, 0, 1, 2)},
		{"lsl x0, x1, #3", LslImm(true, 0, 1, 3)},
		{"lsr w0, w1, w2", Lsrv(false, 0, 1, 2)},
		{"neg x0, x1", NegReg(true, 0, 1)},
		{"mvn w0, w1", MvnReg(false, 0, 1)},
		{"mov x0, x1", MovReg(true, 0, 1)},
		{"mov w0, #42", Movz(false, 0, 42, 0)},
		{"cmp x0, x1", CmpReg(true, 0, 1)},
		{"cmp w0, #7", CmpImm(false, 0, 7)},
		{"ldr x0, [x1]", LdrImm(true, 0, 1, 0)},
		{"ldr w0, [x1, #8]", LdrImm(false, 0, 1, 8)},
		{"str x2, [x3, #16]", StrImm(true, 2, 3, 16)},
		{"ldrb w0, [x1]", LdrbImm(0, 1, 0)},
		{"ret", Ret(30)},
		{"ret x5", Ret(5)},
		{"brk #0", Brk(0)},
		{"nop", 0xd503201f},
		{"hint #0", 0xd503201f},  // hint #0 is nop
		{"hint #8", 0xd503211f},  // pacia1716
		{"hint #12", 0xd503219f}, // autia1716
		{"casal w0, w1, [x2]", uint32(0x88e0fc41)},
		{"casal x3, x4, [x5]", uint32(0xc8e3fca4)},
		{"swpalb w6, w7, [x8]", uint32(0x38e68107)},
		{"swpal w9, w10, [x11]", uint32(0xb8e9816a)},
		{"swpal x12, x13, [x14]", uint32(0xf8ec81cd)},
		{"ldaddal w15, w16, [x17]", uint32(0xb8ef0230)},
		{"ldaddal x18, x19, [x20]", uint32(0xf8f20293)},
		{"ldclralb w21, w22, [x23]", uint32(0x38f512f6)},
		{"ldclral w24, w25, [x26]", uint32(0xb8f81359)},
		{"ldsetalb w3, w4, [x5]", uint32(0x38e330a4)},
		{"ldsetal x9, x10, [x11]", uint32(0xf8e9316a)},
		{"ldarb w12, [x13]", uint32(0x08dffdac)},
		{"stlrb w14, [x15]", uint32(0x089ffdee)},
		{"ldaxrb w16, [x17]", uint32(0x085ffe30)},
		{"stlxrb w18, w19, [x20]", uint32(0x0812fe93)},
	}
	for _, c := range cases {
		t.Run(c.asm, func(t *testing.T) {
			b, err := Assemble(c.asm)
			require.NoError(t, err)
			require.Equal(t, c.want, word(t, b, 0), "encoding of %q", c.asm)
		})
	}
}

// TestAssembleBranches checks that labels and forward/backward branches resolve.
func TestAssembleBranches(t *testing.T) {
	b, err := Assemble(`
		// count down x0 to zero
	loop:
		sub x0, x0, #1
		cbnz x0, loop
		ret
	`)
	require.NoError(t, err)
	require.Len(t, b, 3*4)
	require.Equal(t, SubImm(true, 0, 0, 1), word(t, b, 0))
	// cbnz branches back one instruction: byte offset -4.
	require.Equal(t, Cbnz(true, 0, -4), word(t, b, 1))
	require.Equal(t, Ret(30), word(t, b, 2))
}

func TestAssembleRawInstructionWord(t *testing.T) {
	b, err := Assemble(".inst 0xd503201f")
	require.NoError(t, err)
	require.Len(t, b, 4)
	require.Equal(t, uint32(0xd503201f), word(t, b, 0))
}

func TestTextAssemblerCPUIdentificationRegisters(t *testing.T) {
	program, err := AssembleProgram(`
mrs x0, id_aa64isar0_el1
mrs x1, id_aa64pfr0_el1
mrs x2, midr_el1
`)
	require.NoError(t, err)

	code, relocations, err := program.Link()
	require.NoError(t, err)
	require.Empty(t, relocations)
	require.Equal(t, []byte{
		0x00, 0x06, 0x38, 0xd5,
		0x01, 0x04, 0x38, 0xd5,
		0x02, 0x00, 0x38, 0xd5,
	}, code)
}

func TestTextAssemblerDITSequence(t *testing.T) {
	program, err := AssembleProgram(`
mrs x0, dit
msr dit, #1
msr dit, #0
ubfx x1, x0, #24, #1
dsb nsh
isb sy
`)
	require.NoError(t, err)

	code, relocations, err := program.Link()
	require.NoError(t, err)
	require.Empty(t, relocations)
	require.Equal(t, []byte{
		0xa0, 0x42, 0x3b, 0xd5,
		0x5f, 0x41, 0x03, 0xd5,
		0x5f, 0x40, 0x03, 0xd5,
		0x01, 0x60, 0x58, 0xd3,
		0x9f, 0x37, 0x03, 0xd5,
		0xdf, 0x3f, 0x03, 0xd5,
	}, code)
}

// TestAssembleErrors confirms bad input is reported, never silently dropped.
func TestAssembleErrors(t *testing.T) {
	for _, src := range []string{
		"frobnicate x0, x1",  // unknown mnemonic
		"add x0, x1",         // too few operands
		"add x0, x1, potato", // bad operand
		"b missing",          // undefined label (Bytes resolves locally only)
	} {
		_, err := Assemble(src)
		require.Error(t, err, "expected error for %q", src)
	}
}

// TestLinkExternalBranch confirms Program.Link keeps an undefined bl as a CALL26
// relocation (rather than erroring like Bytes) and exports .globl labels, so a
// hand-written function can be assembled into a linkable object.
func TestLinkExternalBranch(t *testing.T) {
	p, err := AssembleProgram(`
		.globl f
	f:
		bl external
		ret
	`)
	require.NoError(t, err)
	code, relocs, err := p.Link()
	require.NoError(t, err)
	require.Len(t, code, 2*4)
	require.Len(t, relocs, 1)
	require.Equal(t, "external", relocs[0].Sym)
	require.Equal(t, RelCall26, relocs[0].Kind)
	require.Equal(t, 0, relocs[0].Offset) // the bl is the first word
	require.True(t, p.IsGlobal("f"))

	// A conditional or missing local branch still cannot be a relocation.
	_, err = AssembleProgram("b.eq gone\nret")
	require.NoError(t, err)
}
