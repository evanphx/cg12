package cc_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// A pile of GNU C that real code (Ruby's headers among them) leans on: the
// __builtin_unreachable/assume idiom in a void ?:, __builtin_choose_expr,
// __builtin_isinf(_sign), and the rotate builtins. Each result is checked so the
// lowering is exercised, not merely compiled.
func TestGNUBuiltins(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
static int classify(int x){
	// assume-style void ?: with __builtin_unreachable in the untaken arm
	(x >= 0 ? (void)0 : __builtin_unreachable());
	return x;
}
int main(void){
	printf("classify=%d\n", classify(7));
	printf("choose=%d\n", __builtin_choose_expr(1, 42, 0.0));   // picks the int arm
	double inf = 1.0/0.0, ninf = -1.0/0.0;
	printf("isinf=%d,%d,%d\n", __builtin_isinf(inf), __builtin_isinf(ninf), __builtin_isinf(3.0));
	printf("isinf_sign=%d,%d,%d\n", __builtin_isinf_sign(inf), __builtin_isinf_sign(ninf), __builtin_isinf_sign(3.0));
	unsigned long v = 0x0123456789abcdefUL;
	printf("rotl64=%lx rotr64=%lx\n", __builtin_rotateleft64(v, 8), __builtin_rotateright64(v, 8));
	unsigned int w = 0x12345678u;
	printf("rotl32=%x rotr32=%x\n", __builtin_rotateleft32(w, 4), __builtin_rotateright32(w, 4));
	printf("rot0=%lx\n", __builtin_rotateleft64(v, 0));   // a zero rotate is a no-op, not UB
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t,
		"classify=7\n"+
			"choose=42\n"+
			"isinf=1,1,0\n"+
			"isinf_sign=1,-1,0\n"+
			"rotl64=23456789abcdef01 rotr64=ef0123456789abcd\n"+
			"rotl32=23456781 rotr32=81234567\n"+
			"rot0=123456789abcdef\n", out)
}

// __builtin_assume_aligned evaluates to its pointer argument unchanged. Without a
// prototype it defaulted to returning int, so the enclosing cast to a pointer
// sign-extended it from 32 bits -- truncating any address with high bits set (a
// stack pointer, a >4GB heap). Ruby's st_hash dereferences a pointer through it
// and crashed. Deref through it and check the value survives.
func TestAssumeAlignedPointer(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
int main(void){
	unsigned long buf[3] = {0x1122334455667788UL, 0xdeadbeefcafebabeUL, 7};
	unsigned long *p = (unsigned long*)__builtin_assume_aligned(buf, 8);
	printf("p0=%lx\n", p[0]);
	printf("p1=%lx\n", p[1]);
	// The exact idiom st.c uses: cast the builtin result and dereference it.
	printf("d=%lx\n", *(unsigned long*)__builtin_assume_aligned(buf + 2, 8));
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "p0=1122334455667788\np1=deadbeefcafebabe\nd=7\n", out)
}

// The pointer-authentication signing idiom a pac-ret build uses to sign a
// coroutine's initial return address (coroutine/arm64/Context.h): a hint #8
// (PACIA1716) with the operands pinned to x17/x16 by GCC local register
// variables. cg12 recognizes it and lowers it to a PACIA. The signature lives in
// the pointer's high bits; the low (address) bits must survive, and a different
// modifier must change the result -- confirming both operands are wired through.
func TestPtrauthSignIdiom(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
static inline void *sign(void *addr, void *modifier) {
	register void *r17 __asm("r17") = addr;
	register void *r16 __asm("r16") = modifier;
	asm ("hint #8;" : "+r"(r17) : "r"(r16));
	return r17;
}
int main(void){
	int x = 0;
	void *p = &x;
	unsigned long mask = 0x0000ffffffffffffUL; // low 48 bits: the address itself
	void *s1 = sign(p, (void*)0x1111);
	void *s2 = sign(p, (void*)0x2222);
	printf("low=%d\n", ((unsigned long)s1 & mask) == ((unsigned long)p & mask));
	// If the CPU implements PAC the two signatures differ; if it treats the hint
	// as a nop both equal p. Either way they agree with each other iff PAC is off,
	// so "s1==s2 implies signatures==p" holds in both worlds.
	printf("mod=%d\n", (s1 == s2) == ((unsigned long)s1 == (unsigned long)p));
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "low=1\nmod=1\n", out)
}

// More builtins real code reaches for: popcount, the _p overflow predicates, and
// __builtin_memcpy routed to libc.
func TestMoreBuiltins(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
#include <string.h>
int main(void){
	printf("popcount=%d,%d,%d\n", __builtin_popcount(0xffu), __builtin_popcountl(0x10101ul), __builtin_popcountll(0ull));
	printf("mulov=%d,%d\n", __builtin_mul_overflow_p(1<<20, 1<<20, (int)0), __builtin_mul_overflow_p(3, 4, (int)0));
	char dst[8] = {0};
	__builtin_memcpy(dst, "hello", 5);
	printf("copied=%s\n", dst);
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t,
		"popcount=8,3,0\n"+
			"mulov=1,0\n"+
			"copied=hello\n", out)
}

// A __attribute__((alias)) function (Ruby's RUBY_ALIAS_FUNCTION) resolves to its
// target's code; calling either name runs the same function.
func TestFunctionAlias(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
int real_add(int a, int b){ return a + b; }
int alias_add(int a, int b) __attribute__((alias("real_add")));
int main(void){
	printf("%d %d\n", real_add(2, 3), alias_add(2, 3));
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "5 5\n", out)
}

// A variable declared after a return, reached only through a later goto label,
// must still get its storage (cg12 once skipped the declaration as dead code, so
// the label's code referenced an undefined symbol). Ruby's numeric.c does this.
func TestVariableDeclaredAfterReturn(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
static int f(int x){
	if (x > 0) goto late;
	return -1;

	int v;
late:
	v = x * 10;
	return v + 1;
}
int main(void){ printf("%d\n", f(5)); return 0; }`)
	require.Equal(t, 0, code)
	require.Equal(t, "51\n", out)
}

// A function returning a function pointer, where a parameter name repeats between
// the function's own parameter list and the returned pointer's signature. The two
// live in different prototype scopes; GCC accepts it and so must cg12 (this once
// failed with a spurious "redeclaration of 'argc'").
func TestFuncReturningFuncPointerParamScope(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
static int add1(int argc, int x){ return argc + x; }
static int (*pick(int argc))(int argc, int x){ (void)argc; return add1; }
int main(void){
	int (*f)(int, int) = pick(10);
	printf("%d\n", f(3, 4));
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "7\n", out)
}

// A compile-time double built from a ?: whose taken arm is an integer, and a
// division by a cast of a large unsigned long -- the shape of Ruby's
// dbl_reduce_scale, which the type checker leaves unfolded. cg12 folds it.
func TestFloatConstFold(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
static const double scale =
	(1.0 / (double)(53 > 31 ? (1ul<<31) : 1.0)
	     / (double)(1ul << (53 % 31)));
int main(void){ printf("%.17g\n", scale); return 0; }`)
	require.Equal(t, 0, code)
	// 1 / 2^31 / 2^22 = 2^-53
	require.Equal(t, "1.1102230246251565e-16\n", out)
}

// A thread-local referenced with no local declaration in scope (as an inlined
// header function reaches one) must still use the TLS ABI, or it fails to link
// against the real thread-local definition. Here cg12 reads a _Thread_local the
// primary source only declares extern.
func TestThreadLocalExternReference(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
extern _Thread_local int tls_counter;
_Thread_local int tls_counter;
static int bump(void){ return ++tls_counter; }
int main(void){
	printf("%d %d %d\n", bump(), bump(), bump());
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "1 2 3\n", out)
}

// Ruby textually #includes .c/.inc files that define external symbols (its
// generated instruction tables, vm_insnhelper.c). Those definitions must be
// emitted even though they are not in the primary source file. cc.CompileWith
// with an include dir exercises the path that reaches them.
func TestIncludedFileExternalDefinition(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "table.inc"), []byte(`
const int shared_table[3] = { 10, 20, 30 };
int shared_get(int i){ return shared_table[i]; }
`), 0o644))
	mod, err := cc.CompileWith("m.c", `
#include "table.inc"
int use(void){ return shared_get(1) + shared_table[2]; }
`, cc.Options{Target: cc.TargetARM64, IncludeDirs: []string{dir}})
	require.NoError(t, err)
	// The included definitions are present in the module, not just referenced.
	var haveData, haveFunc bool
	for _, d := range mod.Data {
		if d.Name == "shared_table" {
			haveData = true
		}
	}
	for _, f := range mod.Funcs {
		if f.Name == "shared_get" {
			haveFunc = true
		}
	}
	require.True(t, haveData, "external data from an included file is emitted")
	require.True(t, haveFunc, "external function from an included file is emitted")
}
