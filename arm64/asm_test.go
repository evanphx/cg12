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
int sumdiff(int a, int b){ /* two "=&r" outputs, kept distinct from the inputs */
	int s, d;
	__asm__("add %w0, %w2, %w3\n\tsub %w1, %w2, %w3" : "=&r"(s), "=&r"(d) : "r"(a), "r"(b));
	return s * 1000 + d;
}
int memops(int start){ /* "m" input and "=m" output: read, increment in memory, read */
	int cell = start, out;
	__asm__("ldr %w0, %2\n\tadd %w0, %w0, #5\n\tstr %w0, %1" : "=&r"(out), "=m"(cell) : "m"(cell));
	return cell * 1000 + out; /* cell updated in place, out = start+5 */
}
int accum(int x, int y){ /* "+r": read-write register, preloaded and stored back */
	__asm__("add %w0, %w0, %w1" : "+r"(x) : "r"(y));
	return x;
}
int main(void){
	printf("%d %ld %ld %d %d %d %d\n", add(20, 22), shl(3, 4), keeptest(1000, 50, 8),
		addimm(23), sumdiff(20, 8), memops(37), accum(40, 2));
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
			require.Equal(t, "42 48 1042 123 28012 42042 42\n", out) // +,<<,sub+k,+imm,sumdiff,memops,+r
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
