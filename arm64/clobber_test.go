package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// An OAsm clobbers like a call, which covers the caller-saved set -- so a
// template that writes x9 or x10 has always worked. GNU asm also permits
// clobbering a CALLEE-saved register, and that is precisely where the allocator
// puts a value that has to survive the asm. The clobber list is what says so,
// and it was parsed and thrown away: this program crashed.
func TestCalleeSavedClobbersAreHonoured(t *testing.T) {
	m, err := cc.Compile("c.c", `
long f(long a, long b, long c, long d, long e){
	long r;
	__asm__("mov x19, #99\n\tmov x20, #99\n\tmov x21, #99\n\tmov x22, #99\n\tmov x23, #99\n\tmov %x0, #9"
		: "=r"(r) : : "x19","x20","x21","x22","x23");
	return r + a + b + c + d + e;   /* every one must survive */
}
int runtest(void){ return f(1,2,3,4,5) == 24 ? 0 : 1; }`)
	require.NoError(t, err)

	_, code := buildAndRun(t, m, `
extern int runtest(void);
int main(void){ return runtest(); }`)
	require.Equal(t, 0, code, "the values live across the asm must not be in the registers it writes")
}

// The caller-saved case, which worked before and must keep working.
func TestCallerSavedClobbersStillWork(t *testing.T) {
	m, err := cc.Compile("c.c", `
long f(long a, long b){
	long r;
	__asm__("mov x9, #99\n\tmov x10, #99\n\tsub %x0, %x1, %x2"
		: "=r"(r) : "r"(a), "r"(b) : "x9","x10","cc");
	return r;
}
int runtest(void){ return f(50, 8) == 42 ? 0 : 1; }`)
	require.NoError(t, err)
	_, code := buildAndRun(t, m, `
extern int runtest(void);
int main(void){ return runtest(); }`)
	require.Equal(t, 0, code)
}

// A clobber the backend cannot resolve to a register is an error. Keeping only
// the promises we happen to recognise, and passing silently over the rest, keeps
// exactly the ones that needed no keeping.
func TestUnknownClobberIsRefused(t *testing.T) {
	for _, name := range []string{"not_a_register", "x99", "rbx"} {
		m, err := cc.Compile("c.c", `
int f(int a){ int r; __asm__("mov %w0, %w1" : "=r"(r) : "r"(a) : "`+name+`"); return r; }`)
		require.NoError(t, err)
		_, err = arm64.CompileObject(m)
		require.Errorf(t, err, "clobbering %q must not compile", name)
		require.Contains(t, err.Error(), name)
	}
}

// "memory" and "cc" name no register: the memory clobber is already implied by
// an OAsm being an unknown memory effect, and cg12 does not allocate the flags.
func TestMemoryAndCcClobbersAreAccepted(t *testing.T) {
	m, err := cc.Compile("c.c", `
int g;
int f(int a){ int r; __asm__("mov %w0, %w1" : "=r"(r) : "r"(a) : "memory","cc"); return r + g; }`)
	require.NoError(t, err)
	_, err = arm64.CompileObject(m)
	require.NoError(t, err)
}

func TestInlineAsmMaterializationAvoidsClobberedScratchRegisters(t *testing.T) {
	m, err := cc.Compile("c.c", `
long f(void) {
	long result;
	__asm__("mov %x0, %x1" : "=r"(result) : "r"(123456789L) : "x15", "x16", "x17", "x27");
	return result;
}`)
	require.NoError(t, err)
	_, err = arm64.CompileObject(m)
	require.NoError(t, err)
}

func TestInlineAsmReportsWhenEveryScratchRegisterIsClobbered(t *testing.T) {
	m, err := cc.Compile("c.c", `
long f(void) {
	long result;
	__asm__("mov %x0, %x1" : "=r"(result) : "r"(123456789L) : "x14", "x15", "x16", "x17", "x27");
	return result;
}`)
	require.NoError(t, err)
	_, err = arm64.CompileObject(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-clobbered integer scratch")
}
