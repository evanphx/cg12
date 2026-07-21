package arm64_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// TestJumpTableArm64 exercises a dense switch that lowers to an indexed jump
// table -- both a contiguous range and one based at a negative case -- end to
// end on arm64: the table is built, indexed, and branched through by running it.
func TestJumpTableArm64(t *testing.T) {
	src := `
int contig(int n){ switch(n){
 case 0:return 100; case 1:return 101; case 2:return 102; case 3:return 103;
 case 4:return 104; case 5:return 105; case 6:return 106; case 7:return 107;
 case 8:return 108; case 9:return 109; default:return -1; } }
int negbase(int n){ switch(n){
 case -4:return 1; case -3:return 2; case -2:return 3; case -1:return 4;
 case 0:return 5; case 1:return 6; case 2:return 7; case 3:return 8; default:return 0; } }
int check(void){
	if(contig(0)!=100) return 1;
	if(contig(9)!=109) return 2;
	if(contig(5)!=105) return 3;
	if(contig(-1)!=-1) return 4;
	if(contig(10)!=-1) return 5;
	if(negbase(-4)!=1) return 6;
	if(negbase(0)!=5) return 7;
	if(negbase(3)!=8) return 8;
	if(negbase(-5)!=0) return 9;
	if(negbase(4)!=0) return 10;
	return 0;
}`
	main := `extern int check(void); int main(void){ return check(); }`

	m, err := cc.Compile("switch.c", src)
	require.NoError(t, err)
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)
}

// A dense switch inside a loop still lowers to a jump table after the optimizer
// promotes the loop's variables: mem2reg gives the loop's merge block (the
// switch's implicit default, since no case falls through to it) phis for every
// loop-carried value, and the jump-table pass must not treat those as a reason to
// fall back to a comparison tree when the value range has no gaps routing to it.
// This is the shape of a promoted interpreter's dispatch loop.
func TestJumpTableSurvivesPromotedDefaultPhi(t *testing.T) {
	src := `
int run(const unsigned char *c, int n){
    int s = 0;
    for (int i = 0; i < n; i++) {
        int v = 0;
        switch (c[i]) {
        case 0: v=1; break; case 1: v=2; break; case 2: v=3;  break;
        case 3: v=4; break; case 4: v=5; break; case 5: v=6;  break;
        case 6: v=7; break; case 7: v=8; break; case 8: v=9;  break;
        case 9: v=10; break;
        }
        s += v;
    }
    return s;
}`
	main := `extern int run(const unsigned char*, int);
int main(void){
    unsigned char code[10] = {0,1,2,3,4,5,6,7,8,9};
    return run(code, 10) == 55 ? 0 : 1;   /* 1+2+...+10 */
}`

	// End to end: the promoted loop dispatches and accumulates correctly.
	m, err := cc.Compile("run.c", src)
	require.NoError(t, err)
	opt.Run(m, opt.DefaultPipeline())
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)

	// And the dispatch really is an indexed jump table, not a comparison tree.
	m2, err := cc.Compile("run.c", src)
	require.NoError(t, err)
	opt.Run(m2, opt.DefaultPipeline())
	o, err := arm64.CompileToObject(m2)
	require.NoError(t, err)
	require.True(t, strings.Contains(arm64.Disassemble(o), "br x"),
		"dense switch with a promoted default phi should use an indexed branch")
}
