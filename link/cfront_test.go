package link_test

// C constructs whose correctness only running shows, checked per backend. Each
// lives here rather than in difftest/ because difftest compares against the host
// gcc and so only ever runs arm64: the bugs below were worse on amd64, and one of
// them was invisible on arm64 entirely.

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
)

// C lets a goto jump into a block, past a declaration -- SQLite's VDBE
// interpreter does exactly this. The declaration's storage still exists on the
// jumped-to path, so this program returns 42; gcc agrees.
//
// In the IR the alloca lands in the block holding the declaration, and the use
// lands in the label's block, which is reachable without it. The alloca does not
// dominate its use, so the temporary holding the address is read on a path that
// never wrote it, and the backend's `lea`/`add` for it never ran.
const gotoPastDeclSrc = `
int probe(int x) {
	if (x) goto later;
	{
		int arr[4];
	later:
		arr[0] = 7;
		arr[1] = 35;
		return arr[0] + arr[1];
	}
}
int main(void) { return probe(1); }
`

func gotoPastDeclModule(t *testing.T, target cc.Target) *ir.Module {
	t.Helper()
	m, err := cc.CompileFor(target, "p.c", gotoPastDeclSrc)
	require.NoError(t, err)
	return m
}

// The arm64 backend hoisted allocas to the entry block from the start, so this
// has always passed there. It is here to hold the pair together: the answer must
// be the same on both, and it is the comparison that makes the amd64 result below
// mean something rather than look like a quirk of one target.
func TestArm64GotoPastDeclaration(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(gotoPastDeclModule(t, cc.TargetARM64)))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 42, runExe(t, exe), "the storage exists on the jumped-to path")
}

// The same program on amd64, which had no hoisting pass: it segfaulted, because
// the register that should hold arr's address was never loaded and the first
// store went wherever it happened to point.
//
// The pass was target-independent all along and lived in one backend, so the
// other inherited nothing. That is the failure this pair exists to prevent
// recurring -- a third backend would have inherited nothing too.
func TestAmd64GotoPastDeclaration(t *testing.T) {
	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(gotoPastDeclModule(t, cc.TargetAMD64)))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 42, runX86Static(t, exe), "the storage exists on the jumped-to path")
}

// va_copy on amd64, which segfaulted: modernc defines the builtin as a macro
// expanding to `dst = src`, and models a va_list as an 8-byte pointer, so the
// assignment copied 8 bytes where the state is longer. The destination kept the
// source's address rather than its state, and va_arg through it dereferenced
// that as a register-save area.
//
// The differential harness in difftest/ covers this against gcc, but only on
// arm64 -- where the same bug merely returned a wrong answer. Only the amd64 run
// showed it as a crash, and only this test runs it.
func TestAmd64VaCopy(t *testing.T) {
	m, err := cc.CompileFor(cc.TargetAMD64, "v.c", `
#include <stdarg.h>
static int sum(int n, ...) {
	va_list a, b;
	va_start(a, n);
	va_copy(b, a);
	int x = 0, y = 0;
	for (int i = 0; i < n; i++) x += va_arg(a, int);
	for (int i = 0; i < n; i++) y += va_arg(b, int);
	va_end(a); va_end(b);
	return x + y;
}
int main(void){ return sum(4, 1, 2, 3, 4) == 20 ? 0 : 1; }`)
	require.NoError(t, err)

	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 0, runX86Static(t, exe), "a copied va_list walks the same arguments")
}

// A module lowered for one target must not be lowered for another.
//
// Lowering rewrites a function in place and pins the target's physical registers
// into its temporaries. Nothing recorded that, so compiling one module for arm64
// and then for amd64 built an amd64 program around AArch64's register
// assignment. It did not fail: `add(40, 2)` came back as something that is not
// 42, from an image that linked and ran.
//
// The answer must stay an error rather than become a wrong number.
func TestModuleCannotBeLoweredForTwoTargets(t *testing.T) {
	src := `int add(int a, int b){ return a + b; }
	        int main(void){ return add(40, 2) == 42 ? 0 : 1; }`
	m, err := cc.CompileFor(cc.TargetARM64, "p.c", src)
	require.NoError(t, err)

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m), "the first target lowers it")

	l2 := link.NewWith(amd64.Backend{})
	err = l2.AddModule(m)
	require.Error(t, err, "the second target must refuse the already-lowered module")
	require.Contains(t, err.Error(), "already lowered for arm64")
}

// Lowering the same module again for the SAME target stays allowed: the passes
// are idempotent on already-lowered IR, and compiling one module into two images
// of one architecture is a reasonable thing to do. The marker exists to catch a
// second TARGET, not a second call.
func TestModuleCanBeLoweredTwiceForOneTarget(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	// Not add(a,b): phis, a loop, a by-value aggregate and calls, so "idempotent"
	// means something.
	src := `
	struct P { long a, b, c; };
	static long acc(struct P p, int n){
		long t = 0;
		for (int i = 0; i < n; i++) t += (i & 1) ? p.a + i : p.b - i;
		return t + p.c;
	}
	int main(void){ struct P p = {10, 20, 3}; return acc(p, 6) == 96 ? 0 : 1; }`
	m, err := cc.CompileFor(cc.TargetARM64, "p.c", src)
	require.NoError(t, err)

	for _, pass := range []string{"first", "second"} {
		l := link.NewWith(arm64.Backend{})
		require.NoErrorf(t, l.AddModule(m), "%s lowering for the same target", pass)
		exe, err := l.LinkExecutable("main")
		require.NoErrorf(t, err, "%s link", pass)
		require.Equalf(t, 0, runExe(t, exe), "%s image computes the same answer", pass)
	}
}

// Floating-point inline asm on amd64, which did not compile at all.
//
// Three things were missing and each hid the next: cc rejected "x" (x86's
// spelling of "an SSE register"), the x64 assembler could not name a single SSE
// instruction, and the register allocator spilled every FP operand of every asm.
// The last is the interesting one: an asm clobbers like a call, its inputs are
// deliberately extended one position past it so an output cannot reuse their
// register, and the two together made an operand look like a value living ACROSS
// the asm -- which must be callee-saved, and System V has no callee-saved XMM.
//
// This runs the result, because the assembler agreeing with llvm-mc (which
// amd64/x64 checks) says the bytes are the instruction, not that the operands
// reached it in the right registers.
func TestAmd64FloatInlineAsmRuns(t *testing.T) {
	m, err := cc.CompileFor(cc.TargetAMD64, "f.c", `
double dadd(double a, double b){ double r=a; __asm__("addsd %1, %0" : "+x"(r) : "x"(b)); return r; }
float  fmul(float a, float b){ float r=a; __asm__("mulss %1, %0" : "+x"(r) : "x"(b)); return r; }
double dmov(double a){ double r; __asm__("movsd %1, %0" : "=x"(r) : "x"(a)); return r; }
double dzero(void){ double r; __asm__("xorpd %0, %0" : "=x"(r)); return r; }
// Enough live doubles that the operands cannot all be handed a register by luck.
double busy(double a, double b, double c, double d){
	double r = a; __asm__("addsd %1, %0" : "+x"(r) : "x"(b));
	double s = c; __asm__("mulsd %1, %0" : "+x"(s) : "x"(d));
	return r + s;
}
int main(void){
	if (dadd(1.5, 2.25) != 3.75) return 1;
	if (fmul(2.0f, 4.0f) != 8.0f) return 2;
	if (dmov(7.5) != 7.5) return 3;
	if (dzero() != 0.0) return 4;
	if (busy(1.0, 2.0, 3.0, 4.0) != 15.0) return 5; /* (1+2) + (3*4) */
	return 0;
}`)
	require.NoError(t, err)
	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 0, runX86Static(t, exe), "each template computed what it says")
}

// The operand relief must not reach the clobber list. An asm's operand does not
// CROSS the asm, so it need not be callee-saved -- but a template that says it
// writes a register may write it before the instruction that reads the operand,
// so the operand must still not be sitting there.
//
// Three details make this test test that, each learned by watching an earlier
// draft pass when the rule was deliberately broken:
//
//   - xmm8 is the FIRST entry in floatAllocOrder, so it is what the allocator
//     hands out unless the clobber list stops it. Clobbering one further down
//     passes either way: nothing was going to be put there.
//   - the operand must be one the TEMPLATE reads. A "+x" preload is copied into
//     the output register before the template runs, so a clobber cannot reach it
//     -- the draft that used one proved nothing.
//   - the template ZEROES the register rather than moving an operand into it.
//     "movsd %1, %%xmm8" is a no-op exactly when %1 is already in xmm8, which is
//     the case being tested.
//
// So: if the clobber is ignored, b lands in xmm8, xorpd zeroes it, and r comes
// back 0 rather than 2.25.
func TestAmd64FloatInlineAsmHonoursClobbers(t *testing.T) {
	m, err := cc.CompileFor(cc.TargetAMD64, "f.c", `
double f(double b){
	double r;
	__asm__("xorpd %%xmm8, %%xmm8\n\tmovsd %1, %0" : "=x"(r) : "x"(b) : "xmm8");
	return r;
}
int main(void){ return f(2.25) == 2.25 ? 0 : 1; }`)
	require.NoError(t, err)
	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 0, runX86Static(t, exe), "b was not placed in the clobbered register")
}

// Inline asm that references a symbol -- adrp/add for a global's address, or a
// bl to another function -- carries relocations against its own bytes, and the
// backend must re-anchor them into the function's text. It used to assemble the
// template and throw the relocations away, so `adrp x0, g` reached a zero page
// and the following load dereferenced nothing.
func TestArm64AsmSymbolReference(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	// helper() is reached by a bl in inline asm; g by adrp+add. Both are symbol
	// references that only work if the relocation survived.
	m, err := cc.CompileFor(cc.TargetARM64, "a.c", `
char g = 35;
int helper(void){ return 7; }
int main(void){
	char *p;
	__asm__("adrp %0, g\n\tadd %0, %0, :lo12:g" : "=r"(p));
	int h;
	__asm__("bl helper\n\tmov %w0, w0" : "=r"(h) :: "x30");
	return *p + h;   /* 35 + 7 = 42 */
}`)
	require.NoError(t, err)
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 42, runExe(t, exe), "the symbol references in the templates resolved")
}
