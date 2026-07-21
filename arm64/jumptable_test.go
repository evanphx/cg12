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

// A sparse switch lowers to a three-way binary-search tree: each internal node
// tests the pivot for equality (hit) and ordering (descend) with one comparison
// feeding two conditional branches -- the cmp; b.eq; b.<lt> flag-reuse idiom.
func TestSwitchTreeThreeWayFlagReuse(t *testing.T) {
	src := `int classify(int n){ switch(n){
	 case 3:return 1;   case 17:return 2;   case 42:return 3;   case 100:return 4;
	 case 250:return 5; case 999:return 6;  case 5000:return 7; case 40000:return 8;
	 case 90000:return 9; default:return 0; } }`
	main := `extern int classify(int);
int main(void){
	int in[]={3,17,42,100,250,999,5000,40000,90000,7,-1};
	int want[]={1,2,3,4,5,6,7,8,9,0,0};
	for(int i=0;i<11;i++) if(classify(in[i])!=want[i]) return i+1;
	return 0;
}`
	// End to end: the tree dispatches every case (and the gaps) correctly.
	m, err := cc.Compile("sw.c", src)
	require.NoError(t, err)
	opt.Run(m, opt.DefaultPipeline())
	_, code := buildAndRun(t, m, main)
	require.Equal(t, 0, code)

	// A pivot's equality branch is immediately followed by an ordering branch,
	// with no second comparison between them -- the two share one cmp's flags.
	m2, err := cc.Compile("sw.c", src)
	require.NoError(t, err)
	opt.Run(m2, opt.DefaultPipeline())
	o, err := arm64.CompileToObject(m2)
	require.NoError(t, err)
	// A three-way node feeds two conditional branches from one comparison, so the
	// conditional branches outnumber the comparisons -- impossible if every branch
	// had its own cmp.
	cmps, condBranches := 0, 0
	for _, l := range strings.Split(arm64.Disassemble(o), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "cmp ") || strings.HasPrefix(l, "fcmp ") {
			cmps++
		}
		if isCondBranch(l) {
			condBranches++
		}
	}
	require.Greater(t, condBranches, cmps,
		"a three-way node shares one cmp between its equality and ordering branches")
}

func isCondBranch(line string) bool {
	for _, c := range []string{"b.eq", "b.ne", "b.lt", "b.le", "b.gt", "b.ge",
		"b.cc", "b.cs", "b.lo", "b.hi", "b.ls", "b.mi", "b.pl"} {
		if strings.HasPrefix(line, c) {
			return true
		}
	}
	return false
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
