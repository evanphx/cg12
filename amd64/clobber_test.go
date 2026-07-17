package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// An OAsm clobbers like a call, which covers the caller-saved set. GNU asm also
// permits clobbering a CALLEE-saved register, which is precisely where the
// allocator puts a value that has to survive the asm -- and the clobber list,
// which says so, was parsed and thrown away. On x86-64 this is the cpuid idiom:
// cpuid writes rbx, which is callee-saved, so every use of it declares : "rbx".
func TestCalleeSavedClobbersAreHonoured(t *testing.T) {
	m, err := cc.CompileFor(cc.TargetAMD64, "c.c", `
long f(long a, long b, long c, long d){
	long r;
	__asm__("movq $99, %%rbx\n\tmovq $99, %%r12\n\tmovq $99, %%r13\n\tmovq $99, %%r14\n\tmovq $9, %q0"
		: "=r"(r) : : "rbx","r12","r13","r14");
	return r + a + b + c + d;   /* every one must survive */
}
int runtest(void){ return f(1,2,3,4) == 19 ? 0 : 1; }`)
	require.NoError(t, err)
	require.Equal(t, 0, runObj(t, m),
		"the values live across the asm must not be in the registers it writes")
}

// The caller-saved case, which worked before and must keep working.
func TestCallerSavedClobbersStillWork(t *testing.T) {
	m, err := cc.CompileFor(cc.TargetAMD64, "c.c", `
long f(long a, long b){
	long r;
	__asm__("movq $99, %%r10\n\tmovq %q1, %q0\n\tsubq %q2, %q0"
		: "=r"(r) : "r"(a), "r"(b) : "r10","cc");
	return r;
}
int runtest(void){ return f(50, 8) == 42 ? 0 : 1; }`)
	require.NoError(t, err)
	require.Equal(t, 0, runObj(t, m))
}

// A clobber the backend cannot resolve to a register is an error.
func TestUnknownClobberIsRefused(t *testing.T) {
	for _, name := range []string{"not_a_register", "x19", "r99"} {
		m, err := cc.CompileFor(cc.TargetAMD64, "c.c", `
int f(int a){ int r; __asm__("movl %k1, %k0" : "=r"(r) : "r"(a) : "`+name+`"); return r; }`)
		require.NoError(t, err)
		_, err = amd64.CompileObject(m)
		require.Errorf(t, err, "clobbering %q must not compile", name)
		require.Contains(t, err.Error(), name)
	}
}

// Any of a register's names clobbers the one register: %rbx, %ebx and %bl are
// windows onto the same thing.
func TestClobberNamesAtAnyWidth(t *testing.T) {
	for _, name := range []string{"rbx", "ebx", "bl", "memory", "cc"} {
		m, err := cc.CompileFor(cc.TargetAMD64, "c.c", `
int f(int a){ int r; __asm__("movl %k1, %k0" : "=r"(r) : "r"(a) : "`+name+`"); return r; }`)
		require.NoError(t, err)
		_, err = amd64.CompileObject(m)
		require.NoErrorf(t, err, "clobbering %q should compile", name)
	}
}
