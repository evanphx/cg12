package cc_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Regressions found by compiling and running miniruby with cg12. Each is a
// distinct codegen or front-end bug that a real interpreter tripped over.

// Inline `mov %0, sp` must capture the stack pointer, not the zero register (SP
// and XZR share register number 31, so the assembler has to pick the add form).
// Ruby samples the stack pointer this way to detect overflow; a zeroed sample
// made every method call look like a stack overflow.
func TestInlineAsmMovSP(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
int main(void){
	void *sp;
	__asm__ __volatile__("mov\t%0, sp" : "=r"(sp));
	// A real stack pointer is a large non-zero address; zero would mean the move
	// read the zero register instead.
	printf("%s\n", sp != (void*)0 ? "sp-ok" : "sp-zero");
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "sp-ok\n", out)
}

// An enum bit field is unsigned when the enum has no negative enumerator (the GCC
// rule), so a value that sets the field's top bit reads back whole rather than
// sign-extended. Ruby packs its method type (VM_METHOD_TYPE_OPTIMIZED == 9) into a
// 4-bit enum field; sign-extending it to -7 sent method dispatch down the wrong arm.
func TestEnumBitfieldUnsigned(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
enum kind { K0, K1, K2, K3, K4, K5, K6, K7, K8, K9 };
struct s { enum kind k : 4; int other : 4; };
int main(void){
	struct s v = { K9, 0 };
	printf("%d\n", (int)v.k);   // 9, not -7
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "9\n", out)
}

// A static struct whose initializer takes its own address (a list head that
// points to itself, `static T x = { &x }`) must land in initialized data with a
// relocation, not be zeroed into .bss. A NULL self-pointer made an "empty" list
// look non-empty and its walk dereference garbage.
func TestSelfReferentialStaticInit(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
struct node { struct node *next, *prev; };
static struct node head = { &head, &head };
int main(void){
	printf("%s\n", (head.next == &head && head.prev == &head) ? "self" : "broken");
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "self\n", out)
}

// A block-scope `extern` names a global defined elsewhere; it has no local
// storage. cg12 had given it a stack slot, so `char **envp = environ;` read
// uninitialized stack instead of the real environ.
func TestBlockScopeExtern(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
long the_global = 1234567890123;
int main(void){
	extern long the_global;   // block-scope extern names the global, not a new local
	long v = the_global;      // reads the global's value, not uninitialized stack
	printf("%ld\n", v);
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "1234567890123\n", out)
}

// A constant ?: in a static pointer initializer picks its arm at compile time:
// Ruby's generated builtin tables use `i < 0 ? NULL : "name"` to give real entries
// a name and the sentinel a null one. Dropping it left every name NULL, so the
// lookup walked past the sentinel and dereferenced a NULL name.
func TestTernaryInStaticInitializer(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
struct e { int i; const char *name; };
static const struct e tab[] = {
	{ 0, 0 < 0 ? (const char*)0 : "zero" },
	{ 1, 1 < 0 ? (const char*)0 : "one" },
	{ -1, -1 < 0 ? (const char*)0 : "never" },
};
int main(void){
	printf("%s %s %s\n", tab[0].name, tab[1].name, tab[2].name ? tab[2].name : "(null)");
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "zero one (null)\n", out)
}

// An 8-byte __atomic_exchange_n must return the full 64-bit previous value, not a
// sign-extended low 32 bits. Ruby's finalizer walks a zombie list it takes with an
// atomic exchange; a truncated head pointer crashed it at process exit.
func TestAtomicExchange64(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
int main(void){
	unsigned long slot = 0xdeadbeef00c0ffeeUL;
	unsigned long old = __atomic_exchange_n(&slot, 0, __ATOMIC_SEQ_CST);
	printf("%016lx %016lx\n", old, slot);   // full 64-bit old value, slot now 0
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "deadbeef00c0ffee 0000000000000000\n", out)
}

// A 64-bit rotate's result must stay 64-bit when it feeds a wider expression, not
// be typed as int and truncated. Ruby's flonum encoding rotates the bits of a
// double, then masks; truncation turned every float into NaN.
func TestRotate64InExpression(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
int main(void){
	// The & forces the rotate result through a 64-bit expression; a 32-bit-typed
	// (truncated, sign-extended) result would print ffffffffabcdef01.
	unsigned long r = __builtin_rotateleft64(0x123456789abcdef0UL, 4) & ~0UL;
	unsigned long back = __builtin_rotateright64(r, 4);
	printf("%016lx %016lx\n", r, back);
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "23456789abcdef01 123456789abcdef0\n", out)
}
