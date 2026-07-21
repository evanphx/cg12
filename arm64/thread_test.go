package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// ThreadJumps (run during lowering) collapses empty forwarding blocks but must
// never bypass or drop a block whose address is taken by a computed goto -- an
// indirect branch reaches it with no explicit edge. This runs a computed-goto
// dispatch loop end to end to guard that invariant.
func TestThreadJumpsPreservesComputedGoto(t *testing.T) {
	src := `
long cg(int n){
    static void * const tab[] = {&&L0, &&L1, &&L2};
    long s = 0; int i = 0;
    goto *tab[0];
 L0: s += 1; i++; if (i >= n) return s; goto *tab[i%3];
 L1: s += 2; i++; if (i >= n) return s; goto *tab[i%3];
 L2: s += 3; i++; if (i >= n) return s; goto *tab[i%3];
}`
	main := `extern long cg(int);
int main(void){
    return (cg(6) == 12 && cg(1) == 1 && cg(3) == 6) ? 0 : 1; /* 1,2,3 cycling */
}`
	m, err := cc.Compile("cg.c", src)
	require.NoError(t, err)
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)
}
