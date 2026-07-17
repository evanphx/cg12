package cc_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// `__asm__ volatile("" ::: "memory")` emits no instructions. That is the whole
// point of it: it exists to stop the optimizer moving memory accesses across it,
// which is how seqlocks, lock-free code, and Linux's barrier() work. A barrier
// that optimizes away is worse than no barrier, because the source says it is
// there.
func TestMemoryBarrierBlocksLoadElimination(t *testing.T) {
	load := func(src string) int {
		m, err := cc.Compile("b.c", src)
		require.NoError(t, err)
		opt.OptimizeModule(m)
		return strings.Count(m.String(), "loaduw $g")
	}

	// Without a barrier the second read is redundant and folding it is correct.
	require.Equal(t, 1, load(`
int g;
int f(void){ int a = g; int b = g; return a + b; }`),
		"without a barrier the two reads fold to one")

	// With one in between, both reads must survive.
	require.Equal(t, 2, load(`
int g;
int f(void){ int a = g; __asm__ volatile("" ::: "memory"); int b = g; return a + b; }`),
		"the barrier must stop the second read folding into the first")
}

// The barrier costs no instructions -- it only constrains the compiler.
func TestMemoryBarrierEmitsNoCode(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
int g = 21;
int main(void){
  int a = g;
  __asm__ volatile("" ::: "memory");
  g = 21;
  __asm__ volatile("" ::: "memory");
  int b = g;
  printf("%d\n", a + b);
  return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "42\n", out)
}
