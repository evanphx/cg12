package arm64_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// Many address-taken locals used across a cold call put their frame-slot addresses
// under pressure. Rather than spill and reload each address, the allocator
// rematerialises it (recompute x29+offset at each use). The values must round-trip
// correctly -- this is the pattern (a stack slot per local live across a rarely-taken
// path) that dominates an interpreter's spill traffic.
func TestRematAddressesUnderPressure(t *testing.T) {
	const n = 20
	var src strings.Builder
	src.WriteString("long ext(long *p){ return *p; }\n")
	src.WriteString("long f(long *in){\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "  long v%d = in[%d];\n", i, i)
	}
	src.WriteString("  long *ps[") // take every address, forcing an alloca per local
	fmt.Fprintf(&src, "%d] = {", n)
	for i := 0; i < n; i++ {
		if i > 0 {
			src.WriteString(", ")
		}
		fmt.Fprintf(&src, "&v%d", i)
	}
	src.WriteString("};\n")
	src.WriteString("  long s = 0;\n")
	src.WriteString("  for (int i = 0; i < ") // load through the addresses
	fmt.Fprintf(&src, "%d; i++) s += *ps[i];\n", n)
	src.WriteString("  if (in[0] == 424242) s += ext(ps[0]) + ext(ps[19]);\n") // cold path
	src.WriteString("  return s;\n}\n")

	m, err := cc.Compile("c.c", src.String())
	require.NoError(t, err)

	main := `extern long f(long*);
int runtest(void){
  long in[20]; for (int i=0;i<20;i++) in[i] = i*i - 3;   /* never 424242 */
  long want = 0; for (int i=0;i<20;i++) want += in[i];
  return f(in) == want ? 0 : 1;
}
int main(void){ return runtest(); }`

	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code, "rematerialised addresses must recompute correctly")
}
