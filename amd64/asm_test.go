package amd64_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// runAsmText compiles C to amd64 assembly text (the pass-through path for inline
// asm), assembles it with clang, links it freestanding with the _start stub, and
// runs it under qemu-x86_64, returning runtest's exit code.
func runAsmText(t *testing.T, src string, optimize bool) int {
	t.Helper()
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang not available")
	}
	if _, err := exec.LookPath("ld.lld"); err != nil {
		t.Skip("ld.lld not available")
	}
	qemu, err := exec.LookPath("qemu-x86_64")
	if err != nil {
		t.Skip("qemu-x86_64 not available")
	}

	m, err := cc.Compile("asm.c", src)
	require.NoError(t, err)
	if optimize {
		opt.OptimizeModule(m)
	}
	asm, err := amd64.CompileModule(m)
	require.NoError(t, err)

	dir := t.TempDir()
	asmPath := filepath.Join(dir, "test.s")
	objPath := filepath.Join(dir, "test.o")
	stubS := filepath.Join(dir, "start.s")
	stubO := filepath.Join(dir, "start.o")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(asmPath, []byte(asm), 0o644))
	require.NoError(t, os.WriteFile(stubS, []byte(startStub), 0o644))

	out, err := exec.Command(clang, "--target=x86_64-linux-gnu", "-c", asmPath, "-o", objPath).CombinedOutput()
	require.NoErrorf(t, err, "assemble failed: %s\n--- asm ---\n%s", out, asm)
	out, err = exec.Command(clang, "--target=x86_64-linux-gnu", "-c", stubS, "-o", stubO).CombinedOutput()
	require.NoErrorf(t, err, "assemble stub: %s", out)
	out, err = exec.Command("ld.lld", "-static", "-nostdlib", "-o", bin, stubO, objPath).CombinedOutput()
	require.NoErrorf(t, err, "link: %s", out)

	cmd := exec.Command(qemu, bin)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("run: %v", err)
	}
	return 0
}

// TestInlineAsmPassthrough compiles C with GNU inline asm to x86-64 assembly and
// runs it: the OAsm template is passed through with its %N operands substituted
// by the registers the allocator assigns. Covers %k (32-bit) and %q (64-bit)
// operands, a multi-instruction template, and a value that must survive an asm
// which clobbers the scratch registers. Checked both raw and optimized.
func TestInlineAsmPassthrough(t *testing.T) {
	src := `
int add(int a, int b){
	int r; __asm__("movl %k1, %k0\n\taddl %k2, %k0" : "=r"(r) : "r"(a), "r"(b)); return r;
}
long shl(long x){
	long r; __asm__("movq %q1, %q0\n\tshlq $1, %q0" : "=r"(r) : "r"(x)); return r;
}
long keeptest(long k, long a, long b){
	long r;
	__asm__("movq %q1, %q0\n\tsubq %q2, %q0" : "=r"(r) : "r"(a), "r"(b) : "r10", "r11", "cc");
	return r + k; /* k must survive the clobber */
}
int addimm(int a){ /* an "i" operand becomes a literal immediate */
	int r; __asm__("movl %k1, %k0\n\taddl %2, %k0" : "=r"(r) : "r"(a), "i"(100)); return r;
}
int sumdiff(int a, int b){ /* two "=&r" outputs, kept distinct from the inputs */
	int s, d;
	__asm__("movl %k2, %k0\n\taddl %k3, %k0\n\tmovl %k2, %k1\n\tsubl %k3, %k1"
		: "=&r"(s), "=&r"(d) : "r"(a), "r"(b));
	return s * 1000 + d;
}
int memops(int start){ /* "m" input and "=m" output: read, add 5 in memory, read */
	int cell = start, out;
	__asm__("movl %2, %k0\n\taddl $5, %k0\n\tmovl %k0, %1" : "=&r"(out), "=m"(cell) : "m"(cell));
	return cell * 1000 + out; /* cell updated in place, out = start+5 */
}
int accum(int x, int y){ /* "+r": read-write register, preloaded and stored back */
	__asm__("addl %k1, %k0" : "+r"(x) : "r"(y));
	return x;
}
int runtest(void){
	if(add(20, 22) != 42) return 1;
	if(shl(21) != 42) return 2;
	if(keeptest(1000, 50, 8) != 1042) return 3; /* 50-8+1000 */
	if(addimm(23) != 123) return 4;
	if(sumdiff(20, 8) != 28012) return 5; /* 28*1000 + 12 */
	if(memops(37) != 42042) return 6;    /* 42*1000 + 42 */
	if(accum(40, 2) != 42) return 7;
	return 0;
}`
	require.Equal(t, 0, runAsmText(t, src, false))
	require.Equal(t, 0, runAsmText(t, src, true))
}

// TestInlineAsmObjectRejected confirms the object emitter rejects inline asm
// rather than emitting wrong code.
func TestInlineAsmObjectRejected(t *testing.T) {
	src := `int f(int a){ int r; __asm__("movl %k1, %k0" : "=r"(r) : "r"(a)); return r; }`
	m, err := cc.Compile("asm.c", src)
	require.NoError(t, err)
	_, err = amd64.CompileObject(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "inline assembly")
}
