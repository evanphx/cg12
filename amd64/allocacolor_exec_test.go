package amd64_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Alloca colouring changes where every local lands, so the assertion that
// matters is not "the frame got smaller" but "the program still computes the
// same answers". These run natively on an x86-64 host through runC, so a slot
// that is reused while still live shows up as a wrong result rather than as a
// layout that merely looks different.
//
// The C front end only emits the lifetime.start/end markers colouring reads when
// CG12_LIFETIMES is set (cc/compile.go), so each test sets it. Without it the
// markers are absent, every alloca keeps a private slot, and these would pass
// without exercising colouring at all -- which is also why the default C path
// and goc are unaffected by this change.

// runCLifetimes is runC with the C front end's lifetime markers enabled, so
// allocaGroups has something to colour.
func runCLifetimes(t *testing.T, src string) int {
	t.Helper()
	t.Setenv("CG12_LIFETIMES", "1")
	return runC(t, src)
}

// TestAllocaColouringDisjointBuffersExecute is the case colouring actually
// collapses: same-shaped buffers in the arms of an if, whose live regions never
// meet. Each arm fills its buffer and reads it back, so a slot handed out while
// another buffer is still live would corrupt the sum.
func TestAllocaColouringDisjointBuffersExecute(t *testing.T) {
	src := `
static int fill(char *p, int n, int seed){
	int s=0;
	for(int i=0;i<n;i++) p[i]=(char)(seed+i);
	for(int i=0;i<n;i++) s+=(unsigned char)p[i];
	return s;
}
int branchy(int c){
	int t=0;
	if(c){ char a[64]; t+=fill(a,64,1); } else { char b[64]; t+=fill(b,64,2); }
	if(c>1){ char d[64]; t+=fill(d,64,3); } else { char e[64]; t+=fill(e,64,4); }
	return t;
}
int runtest(void){
	// seed s over 64 bytes sums to 64*s + 2016, taken mod 256 per byte.
	int expect1=0, expect3=0, expect2=0, expect4=0;
	for(int i=0;i<64;i++){ expect1+=(unsigned char)(1+i); expect2+=(unsigned char)(2+i);
	                       expect3+=(unsigned char)(3+i); expect4+=(unsigned char)(4+i); }
	if(branchy(2)!=expect1+expect3) return 1;
	if(branchy(1)!=expect1+expect4) return 2;
	if(branchy(0)!=expect2+expect4) return 3;
	return 0;
}
`
	require.Equal(t, 0, runCLifetimes(t, src))
}

// TestAllocaColouringNestedLifetimesExecute is the safety direction: an outer
// buffer stays live across an inner scope, so the two must not share. If the
// interference test ever loosened wrongly, the inner writes would land on top of
// the outer buffer and the checksum would change.
func TestAllocaColouringNestedLifetimesExecute(t *testing.T) {
	src := `
static void stamp(char *p, int n, int v){ for(int i=0;i<n;i++) p[i]=(char)(v+i); }
static int sum(char *p, int n){ int s=0; for(int i=0;i<n;i++) s+=(unsigned char)p[i]; return s; }
int nested(int c){
	char outer[64];
	stamp(outer,64,7);
	if(c){ char inner[64]; stamp(inner,64,200); if(sum(inner,64)==0) return -1; }
	{ char other[64]; stamp(other,64,99); if(sum(other,64)==0) return -2; }
	return sum(outer,64);
}
int runtest(void){
	int expect=0; for(int i=0;i<64;i++) expect+=(unsigned char)(7+i);
	if(nested(0)!=expect) return 1;
	if(nested(1)!=expect) return 2;
	return 0;
}
`
	require.Equal(t, 0, runCLifetimes(t, src))
}

// TestAllocaColouringLoopBodiesExecute puts the shared slot inside a loop, where
// the same colour is written and read on every iteration and a live-in from the
// back edge is what the dataflow has to get right.
func TestAllocaColouringLoopBodiesExecute(t *testing.T) {
	src := `
static int check(char *p, int n, int v){
	for(int i=0;i<n;i++) p[i]=(char)(v^i);
	for(int i=0;i<n;i++) if(p[i]!=(char)(v^i)) return 0;
	return 1;
}
int loops(int n){
	int ok=1;
	for(int i=0;i<n;i++){
		{ char a[48]; ok &= check(a,48,i); }
		{ char b[48]; ok &= check(b,48,i+1); }
	}
	{ char c[48]; ok &= check(c,48,5); }
	return ok;
}
int runtest(void){
	if(loops(0)!=1) return 1;
	if(loops(1)!=1) return 2;
	if(loops(9)!=1) return 3;
	return 0;
}
`
	require.Equal(t, 0, runCLifetimes(t, src))
}

// TestAllocaColouringMixedShapesExecute mixes sizes and alignments, including a
// struct with an alignment stronger than its members' -- a group must never mix
// shapes, or a smaller buffer would be handed a slot sized for it while its
// neighbour expects the larger one.
func TestAllocaColouringMixedShapesExecute(t *testing.T) {
	src := `
struct big { long a, b, c, d; };
static long touch(struct big *s, long v){ s->a=v; s->b=v+1; s->c=v+2; s->d=v+3; return s->a+s->b+s->c+s->d; }
int mixed(int c){
	long t=0;
	if(c){ struct big x; t+=touch(&x,10); char s[16]; s[0]=1; s[15]=2; t+=s[0]+s[15]; }
	else  { struct big y; t+=touch(&y,20); char u[16]; u[0]=3; u[15]=4; t+=u[0]+u[15]; }
	{ struct big z; t+=touch(&z,30); }
	return (int)t;
}
int runtest(void){
	if(mixed(1)!=(10+11+12+13)+(1+2)+(30+31+32+33)) return 1;
	if(mixed(0)!=(20+21+22+23)+(3+4)+(30+31+32+33)) return 2;
	return 0;
}
`
	require.Equal(t, 0, runCLifetimes(t, src))
}
