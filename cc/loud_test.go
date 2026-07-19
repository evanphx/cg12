package cc_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// A construct cg12 cannot fold must be a diagnostic, not a default. Taking 0 for
// an unfoldable case label, or emitting nothing for a goto, produces a program
// that compiles, runs, and does something the source never asked for -- which is
// worse than not compiling.
func TestUnfoldableConstructsAreDiagnosed(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{
			// collectLabels pre-allocates a block for every label reachable from a
			// goto, so a name still missing here is one that does not exist -- and
			// emitting nothing for a jump is not a failed jump, it is a fall-through
			// to whatever follows. The parser catches an undefined label in ordinary
			// code, so this is the case it does not see: a jump INTO a statement
			// expression, which gcc rejects as "jump into statement expression" and
			// modernc accepts.
			name: "goto into a statement expression",
			src: `int f(int a){
				int x = ({ int t = a; inner: t; });
				if (x < 5) goto inner;
				return x;
			}`,
			want: "no such label",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := cc.Compile("x.c", c.src)
			require.Error(t, err, "must not compile silently")
			require.Contains(t, err.Error(), c.want)
		})
	}
}

// The constructs that do fold still work, including the ones next to the paths
// above: a goto forward to a label defined later, and case labels from constant
// expressions and enum constants.
func TestFoldableConstructsStillWork(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
enum { TWO = 2, TEN = 10 };
static int classify(int n){
	switch(n){
	case 1:        return 100;
	case TWO:      return 200;        /* an enum constant */
	case 3 + 4:    return 700;        /* a constant expression */
	case TEN ... TEN + 2: return 999; /* a GNU range */
	default:       return 0;
	}
}
int main(void){
	int i = 0;
	goto forward;                     /* a label defined later */
back:
	printf("%d %d %d %d %d %zu\n", classify(1), classify(2), classify(7),
	       classify(11), classify(50), sizeof(int));
	return 0;
forward:
	i = 1;
	if (i) goto back;
	return 1;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "100 200 700 999 0 4\n", out)
}

// long double means a different thing on each target, and cg12 implements one of
// them: AArch64's 128-bit IEEE quad, lowered to the libgcc soft-float helpers
// (__addtf3, __eqtf2, ...). x86-64's is an 80-bit x87 extended value.
//
// Compiling this C for amd64 emitted calls to the quad helpers regardless. That
// failed loudly enough by luck -- x86-64 does not provide them, so the image did
// not load (exit 127) -- but only by luck: the danger is a build that DOES supply
// __addtf3, which computes IEEE binary128. The program would then run, compute in
// a format libc does not share, and disagree the moment a long double reached
// printf. A diagnostic at compile time is the honest answer until amd64 has x87.
func TestLongDoubleIsRefusedForAmd64(t *testing.T) {
	for _, src := range []string{
		`int f(void){ long double a = 1.5L, b = 2.25L; return (a+b) == 3.75L; }`, // arithmetic
		`long double g(long double x){ return x; }`,                              // across the ABI
		`long double gv = 1.0L;`,                                                 // a global
	} {
		_, err := cc.CompileFor(cc.TargetAMD64, "p.c", src)
		require.Error(t, err, "amd64's long double is not the quad cg12 implements")
		require.Contains(t, err.Error(), "long double")

		_, err = cc.CompileFor(cc.TargetARM64, "p.c", src)
		require.NoError(t, err, "arm64's long double IS the quad cg12 implements")
	}
}

// The explicit __atomic_* builtins now lower to the atomic intrinsics (see
// TestExplicitAtomicsRun), but an _Atomic-qualified OBJECT still is not supported:
// the language makes every access to it atomic, and the general expression path
// does not yet route ordinary loads/stores/read-modify-writes through the atomic
// ops. Letting it through would compile `g++` on an _Atomic int to an ordinary
// non-atomic read-modify-write -- code that runs, passes any single-threaded test,
// and races under contention. An error is the honest answer until the access paths
// learn the qualifier.
func TestAtomicObjectsAreRefused(t *testing.T) {
	for _, src := range []string{
		`_Atomic int g; void f(void){ g++; }`,                             // an atomic object
		`void f(void){ _Atomic int x = 0; x++; }`,                         // a local one
		`struct S { _Atomic int a; }; int f(struct S *s){ return s->a; }`, // read via a member
		`struct S { _Atomic int a; }; void f(struct S *s){ s->a = 1; }`,   // written via a member
	} {
		_, err := cc.Compile("a.c", src)
		require.Error(t, err, "an atomic object that compiles to ordinary memory access is worse than one that fails")
		require.Contains(t, err.Error(), "not supported")
	}
}

// The refusal must not spread past what it is for. volatile is a different
// promise and cg12 keeps it (see #104 and cc/volatile_test.go); an ordinary
// member of a struct that merely contains an atomic is an ordinary load; and a
// plain int is a plain int. Refusing any of these would reject programs cg12
// compiles correctly.
func TestNonAtomicsStillCompile(t *testing.T) {
	for _, src := range []string{
		`int g; void f(void){ g++; }`,
		`volatile int g; void f(void){ g++; }`,
		`struct S { _Atomic int a; int b; }; int f(struct S *s){ return s->b; }`,
	} {
		_, err := cc.Compile("a.c", src)
		require.NoError(t, err)
	}
}
