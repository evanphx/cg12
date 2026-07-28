package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// runC compiles a C program whose runtest() returns a word and runs it,
// returning the exit code (0 = success). Building and running the image is
// runObj's job -- including whether an emulator is involved, which on an x86-64
// host it is not.
func runC(t *testing.T, src string) int {
	t.Helper()
	// Ahead of the frontend, which probes the host's C compiler itself and fails
	// rather than skipping when there is none to probe.
	freestanding.Require(t)

	m, err := cc.CompileFor(cc.TargetAMD64, "x.c", src)
	require.NoError(t, err)
	return runObj(t, m)
}

// runCOpt is runC with the optimizer run first, so the test exercises the
// optimized code path (mem2reg promotion, phi coalescing, and the like).
func runCOpt(t *testing.T, src string) int {
	t.Helper()
	freestanding.Require(t)

	m, err := cc.CompileFor(cc.TargetAMD64, "x.c", src)
	require.NoError(t, err)
	opt.Run(m, opt.DefaultPipeline())
	return runObj(t, m)
}

// TestSwitchDispatch exercises the switch lowering end-to-end on amd64: a small
// switch (linear chain) and a larger one (binary search), both with gaps,
// fallthrough, and default, probed across and past their ranges.
func TestSwitchDispatch(t *testing.T) {
	src := `
int small(int n){ switch(n){ case 1:return 10; case 2:return 20; case 5:return 50; default:return -1; } }
int big(int n){
	switch(n){
	case 1: return 101; case 2: return 102; case 3: return 103; case 5: return 105;
	case 8: return 108; case 13: return 113; case 21: return 121; case 34: return 134;
	default: return -1;
	}
}
int fall(int c){ int w=0; switch(c){ case 1: w+=1; case 2: w+=2; break; case 3: w+=4; break; } return w; }

int runtest(void){
	if(small(1)!=10) return 1;
	if(small(2)!=20) return 2;
	if(small(5)!=50) return 3;
	if(small(3)!=-1) return 4;
	if(big(1)!=101) return 5;
	if(big(8)!=108) return 6;
	if(big(34)!=134) return 7;
	if(big(4)!=-1) return 8;
	if(big(100)!=-1) return 9;
	if(fall(1)!=3) return 10;  /* 1 falls into 2 */
	if(fall(2)!=2) return 11;
	if(fall(3)!=4) return 12;
	if(fall(9)!=0) return 13;
	return 0;
}`
	require.Equal(t, 0, runC(t, src))
}
