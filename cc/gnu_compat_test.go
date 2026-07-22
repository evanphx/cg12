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
