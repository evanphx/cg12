package x64

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAssembleEncodings assembles single AT&T instructions and checks each
// against the direct encoder, confirming the text assembler and the compiler's
// machine-code path share one encoding.
func TestAssembleEncodings(t *testing.T) {
	cases := []struct {
		asm  string
		want []byte
	}{
		{"addq %rsi, %rax", AddReg(true, RAX, RSI)},
		{"subl %ecx, %edx", SubReg(false, RDX, RCX)},
		{"andq %r8, %r9", AndReg(true, R9, R8)},
		{"xorl %eax, %eax", XorReg(false, RAX, RAX)},
		{"addq $5, %rax", AddImm(true, RAX, 5)},
		{"cmpl $7, %edi", CmpImm(false, RDI, 7)},
		{"imulq %rbx, %rcx", Imul(true, RCX, RBX)},
		{"negq %rax", Neg(true, RAX)},
		{"notl %edx", Not(false, RDX)},
		{"shlq $3, %rax", ShlImm(true, RAX, 3)},
		{"sarl %cl, %edx", SarCL(false, RDX)},
		{"movq %rsi, %rax", MovReg(true, RAX, RSI)},
		{"movq $42, %rax", MovImm64(RAX, 42)},
		{"movl $7, %ecx", MovImm32(false, RCX, 7)},
		{"movq (%rbp), %rax", Load(true, RAX, At(RBP, 0))},
		{"movl -8(%rbp), %eax", Load(false, RAX, At(RBP, -8))},
		{"movq %rax, 16(%rsp)", Store(64, RAX, At(RSP, 16))},
		{"leaq 8(%rdi), %rax", Lea(true, RAX, At(RDI, 8))},
		{"ret", Ret()},
		{"nop", []byte{0x90}},
	}
	for _, c := range cases {
		t.Run(c.asm, func(t *testing.T) {
			b, err := Assemble(c.asm)
			require.NoError(t, err)
			require.Equal(t, c.want, b, "encoding of %q", c.asm)
		})
	}
}

// TestAssembleBranches checks that labels and jumps resolve.
func TestAssembleBranches(t *testing.T) {
	b, err := Assemble(`
	loop:
		subq $1, %rax
		jne loop
		ret
	`)
	require.NoError(t, err)
	// subq $1,%rax (4 bytes) + jne rel32 (6) + ret (1).
	require.Greater(t, len(b), 8)
	require.Equal(t, byte(0xc3), b[len(b)-1]) // ret
}

// TestAssembleErrors confirms bad input is reported.
func TestAssembleErrors(t *testing.T) {
	for _, src := range []string{
		"frobnicate %rax",   // unknown mnemonic
		"addq %rax",         // too few operands
		"addq %rax, potato", // bad destination
		"jmp missing",       // undefined label
	} {
		_, err := Assemble(src)
		require.Error(t, err, "expected error for %q", src)
	}
}
