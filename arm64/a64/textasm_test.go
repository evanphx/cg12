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

// TestAssembleErrors confirms bad input is reported, never silently dropped.
func TestAssembleErrors(t *testing.T) {
	for _, src := range []string{
		"frobnicate x0, x1",  // unknown mnemonic
		"add x0, x1",         // too few operands
		"add x0, x1, potato", // bad operand
		"b missing",          // undefined label
	} {
		_, err := Assemble(src)
		require.Error(t, err, "expected error for %q", src)
	}
}
