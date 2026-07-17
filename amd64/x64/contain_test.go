package x64

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The register encoding is the same 4 bits at every width, so a mis-sized
// operand does not fail to assemble -- it assembles to an instruction of the
// wrong width. `movb %al, %bl` and `movl %eax, %ebx` were the same bytes, which
// writes four times what was asked. An error is the only safe answer while there
// are no 8- and 16-bit encoders.
func TestMisSizedOperandsAreRejected(t *testing.T) {
	for _, c := range []struct{ src, why string }{
		{"movb %al, %bl", "no byte encoders"},
		{"movw %ax, %bx", "no halfword encoders"},
		{"addb $1, %al", "no byte encoders"},
		{"movq %eax, %rbx", "a 32-bit name in a 64-bit instruction"},
		{"movl %rax, %ebx", "a 64-bit name in a 32-bit instruction"},
		{"addl %rax, %rbx", "64-bit names in a 32-bit instruction"},
	} {
		_, err := Assemble(c.src)
		require.Errorf(t, err, "%s (%s) must not assemble", c.src, c.why)
	}
}

// The forms that do line up still work.
func TestWellSizedOperandsStillAssemble(t *testing.T) {
	for _, src := range []string{
		"movl %eax, %ebx", "movq %rax, %rbx", "addl $1, %eax", "addq %rax, %rbx",
		"movq (%rax), %rbx", "movl %eax, (%rbx)", "leaq 8(%rax), %rbx",
	} {
		_, err := Assemble(src)
		require.NoErrorf(t, err, "%s should assemble", src)
	}
}

// GAS separates statements with ';' on x86-64; it is not a comment. Treating it
// as one silently dropped everything after the first instruction.
func TestSemicolonSeparatesStatements(t *testing.T) {
	one, err := Assemble("movq %rax, %rbx")
	require.NoError(t, err)
	two, err := Assemble("movq %rax, %rbx; movq %rcx, %rdx")
	require.NoError(t, err)
	require.Greater(t, len(two), len(one), "the second instruction must be encoded too")

	sep, err := Assemble("movq %rax, %rbx\nmovq %rcx, %rdx")
	require.NoError(t, err)
	require.Equal(t, sep, two, "';' and a newline must mean the same thing")

	// '#' and '//' really are comments, and still are.
	c, err := Assemble("movq %rax, %rbx # movq %rcx, %rdx")
	require.NoError(t, err)
	require.Equal(t, one, c)
}
