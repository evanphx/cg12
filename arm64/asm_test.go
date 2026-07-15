package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// TestInlineAsmPassthrough compiles C with GNU inline asm to AArch64 assembly and
// runs it: the OAsm template is passed through with its %N operands substituted
// by the registers the allocator assigns. Covers a %w (32-bit) and %x (64-bit)
// operand, a register shift amount, and a value that must survive an asm which
// clobbers the registers the allocator would otherwise use. Checked both raw and
// after the optimizer.
func TestInlineAsmPassthrough(t *testing.T) {
	src := `#include <stdio.h>
int add(int a, int b){
	int r; __asm__("add %w0, %w1, %w2" : "=r"(r) : "r"(a), "r"(b)); return r;
}
long shl(long x, int n){
	long r; __asm__("lsl %x0, %x1, %x2" : "=r"(r) : "r"(x), "r"((long)n)); return r;
}
long keeptest(long k, long a, long b){
	long r;
	__asm__("sub %x0, %x1, %x2" : "=r"(r) : "r"(a), "r"(b) : "x9", "x10", "cc");
	return r + k; /* k must survive the clobber */
}
int addimm(int a){ /* an "i" operand becomes a literal immediate */
	int r; __asm__("add %w0, %w1, %2" : "=r"(r) : "r"(a), "i"(100)); return r;
}
int main(void){
	printf("%d %ld %ld %d\n", add(20, 22), shl(3, 4), keeptest(1000, 50, 8), addimm(23));
	return 0;
}`
	for _, optimize := range []bool{false, true} {
		name := "raw"
		if optimize {
			name = "opt"
		}
		t.Run(name, func(t *testing.T) {
			m, err := cc.Compile("asm.c", src)
			require.NoError(t, err)
			if optimize {
				opt.OptimizeModule(m)
			}
			out, code := buildAndRun(t, m, "")
			require.Equal(t, 0, code)
			require.Equal(t, "42 48 1042 123\n", out) // 20+22, 3<<4, 50-8+1000, 23+100
		})
	}
}

// TestInlineAsmObjectRejected confirms the object emitter, having no assembler
// for arbitrary mnemonics, rejects inline asm rather than emitting wrong code.
func TestInlineAsmObjectRejected(t *testing.T) {
	src := `int f(int a){ int r; __asm__("mov %w0, %w1" : "=r"(r) : "r"(a)); return r; }`
	m, err := cc.Compile("asm.c", src)
	require.NoError(t, err)
	_, err = arm64.CompileObject(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "inline assembly")
}
