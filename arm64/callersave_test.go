package arm64_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// A cold call inside a loop with many values live across it exercises caller-save:
// because the call is rarely taken, colouring puts the live-across values in
// caller-saved registers, and insertCallerSaves must save/restore them around the
// call so the values survive. The call also has several register arguments, so the
// saves must land before the OArg run (not inside it) to keep the call sequence
// contiguous -- the exact hazard that broke an earlier attempt.
func TestCallerSaveColdCallManyLive(t *testing.T) {
	const n = 12
	var src strings.Builder
	src.WriteString("long ext(long a, long b, long c, long d){ return a+b+c+d; }\n")
	src.WriteString("long f(long *p, int reps){\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "  long a%d = p[%d];\n", i, i)
	}
	src.WriteString("  long s = 0;\n")
	src.WriteString("  for (int i = 0; i < reps; i++) {\n")
	src.WriteString("    s += p[i & 7];\n")
	src.WriteString("    if (p[i & 7] == 424242) s += ext(a0+a1+a2, a3+a4+a5, a6+a7+a8, a9+a10+a11);\n")
	src.WriteString("  }\n")
	src.WriteString("  return s")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, " + a%d", i)
	}
	src.WriteString(";\n}\n")

	m, err := cc.Compile("c.c", src.String())
	require.NoError(t, err)

	main := `extern long f(long*, int);
int runtest(void){
  long p[16]; for (int i=0;i<16;i++) p[i] = i*3 - 5;   /* never equals 424242 */
  long s = 0; for (int i=0;i<20;i++) s += p[i & 7];
  long sa = 0; for (int i=0;i<12;i++) sa += p[i];
  return f(p, 20) == (s + sa) ? 0 : 1;
}
int main(void){ return runtest(); }`

	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code, "values live across the cold call must be saved and restored")
}

// The same value pattern but with the call taken every iteration (hot): colouring
// should prefer callee-saved registers (no per-iteration save/restore), and the
// result must still be correct. Together with the cold case this covers both arms of
// the callee-vs-caller-saved decision.
func TestCallerSaveHotCall(t *testing.T) {
	m, err := cc.Compile("c.c", `
long ext(long x){ return x & 15; }
long f(long a, long b, long c, long d, long e, long g, int reps){
	long s = 0;
	for (int i = 0; i < reps; i++)
		s += ext(i) + a + b + c + d + e + g;   /* call every iteration; a..g live across */
	return s;
}`)
	require.NoError(t, err)
	main := `extern long f(long,long,long,long,long,long,int);
int runtest(void){
  long reps = 100, s = 0;
  for (int i=0;i<reps;i++) s += (i & 15) + 1+2+3+4+5+6;
  return f(1,2,3,4,5,6,reps) == s ? 0 : 1;
}
int main(void){ return runtest(); }`
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)
}
