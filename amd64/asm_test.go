package amd64_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// runAsmSrc compiles C containing inline asm to an x86-64 object, links it
// freestanding with the _start stub, and runs it, returning runtest's exit code.
// The template is assembled by x64.Assemble, so this is also what proves that
// path encodes what the template asked for.
func runAsmSrc(t *testing.T, src string, optimize bool) int {
	t.Helper()
	clang := testenv.Tool(t, "clang")
	testenv.Tool(t, "ld.lld")

	// As in runObjWith: an x86-64 image on an x86-64 host is a host binary, so
	// exec it directly and reserve the emulator for reaching x86-64 from another
	// architecture. Resolving qemu-x86_64 only there is what keeps a native run
	// from skipping this test, while a cross host without it still skips.
	var runner string
	if runtime.GOARCH != "amd64" {
		runner = testenv.Tool(t, "qemu-x86_64")
	}

	m, err := cc.CompileFor(cc.TargetAMD64, "asm.c", src)
	require.NoError(t, err)
	if optimize {
		opt.OptimizeModule(m)
	}
	code, err := amd64.CompileObject(m)
	require.NoError(t, err)

	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.o")
	stubS := filepath.Join(dir, "start.s")
	stubO := filepath.Join(dir, "start.o")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, code, 0o644))
	require.NoError(t, os.WriteFile(stubS, []byte(startStub), 0o644))

	out, err := exec.Command(clang, "--target=x86_64-linux-gnu", "-c", stubS, "-o", stubO).CombinedOutput()
	require.NoErrorf(t, err, "assemble stub: %s", out)
	out, err = exec.Command("ld.lld", "-static", "-nostdlib", "-o", bin, stubO, objPath).CombinedOutput()
	require.NoErrorf(t, err, "link: %s", out)

	var cmd *exec.Cmd
	if runner == "" {
		cmd = exec.Command(bin)
	} else {
		cmd = exec.Command(runner, bin)
	}
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
	require.Equal(t, 0, runAsmSrc(t, src, false))
	require.Equal(t, 0, runAsmSrc(t, src, true))
}

// The object path assembles a template itself, and x64.Assemble knows a subset of
// x86-64. A template reaching past that subset has to be an error naming what it
// could not encode -- silently skipping the instruction would leave the
// surrounding code reading a register nothing ever wrote.
func TestInlineAsmUnsupportedMnemonicErrors(t *testing.T) {
	src := `int f(int a){ int r; __asm__("cpuid\n\tmovl %k1, %k0" : "=r"(r) : "r"(a)); return r; }`
	m, err := cc.CompileFor(cc.TargetAMD64, "asm.c", src)
	require.NoError(t, err)
	_, err = amd64.CompileObject(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "inline assembly")
	require.Contains(t, err.Error(), "cpuid", "the error should name what it could not encode")
}

// syscall is why most inline assembly on this target exists, and cg12 could not
// assemble the word: the encoder was here, and nothing could name it. This is
// the real thing -- write(1, "ok\n", 3) and exit(0), by hand, with no libc.
func TestInlineAsmSyscall(t *testing.T) {
	src := `
static long sys_write(long fd, const char *buf, long n){
	long r;
	__asm__ volatile("syscall"
		: "=a"(r)
		: "a"(1L), "D"(fd), "S"(buf), "d"(n)
		: "rcx", "r11", "memory");   /* the kernel clobbers rcx and r11 */
	return r;
}
int runtest(void){
	const char msg[4]; ((char*)msg)[0]='o'; ((char*)msg)[1]='k'; ((char*)msg)[2]='\n'; ((char*)msg)[3]=0;
	return sys_write(1, msg, 3) == 3 ? 0 : 1;
}`
	require.Equal(t, 0, runAsmSrc(t, src, false))
	require.Equal(t, 0, runAsmSrc(t, src, true))
}

// cdq/cqo and the one-operand divides: the dividend is rdx:rax, so only the
// divisor is named, and the sign-extend that fills rdx is a separate step.
func TestInlineAsmDivide(t *testing.T) {
	src := `
static long sdiv(long a, long b){
	long q;
	__asm__("cqo\n\tidivq %q2" : "=a"(q) : "a"(a), "r"(b) : "rdx","cc");
	return q;
}
int runtest(void){
	if (sdiv(84, 2) != 42) return 1;
	if (sdiv(-84, 2) != -42) return 2;
	return 0;
}`
	require.Equal(t, 0, runAsmSrc(t, src, false))
	require.Equal(t, 0, runAsmSrc(t, src, true))
}
