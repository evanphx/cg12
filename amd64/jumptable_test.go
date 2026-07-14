package amd64_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestJumpTableAmd64 exercises a dense switch that lowers to an indexed jump
// table (contiguous, gapped, and negative-based ranges), run under qemu.
func TestJumpTableAmd64(t *testing.T) {
	src := `
int contig(int n){ switch(n){
 case 0:return 100; case 1:return 101; case 2:return 102; case 3:return 103;
 case 4:return 104; case 5:return 105; case 6:return 106; case 7:return 107;
 case 8:return 108; case 9:return 109; default:return -1; } }
int negbase(int n){ switch(n){
 case -4:return 1; case -3:return 2; case -2:return 3; case -1:return 4;
 case 0:return 5; case 1:return 6; case 2:return 7; case 3:return 8; default:return 0; } }
int runtest(void){
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
	require.Equal(t, 0, runC(t, src))
}

// TestComputedGotoAmd64 exercises the &&label / goto *expr GNU extension with a
// label table built at runtime (OBlockAddr materializes each address, JmpBr does
// the indirect branch).
func TestComputedGotoAmd64(t *testing.T) {
	src := `
int run(int n){
	void *tab[3];
	tab[0] = &&a; tab[1] = &&b; tab[2] = &&c;
	int acc = 0;
	if(n < 0 || n > 2) return -1;
	goto *tab[n];
a: acc += 1; goto done;
b: acc += 2; goto done;
c: acc += 4; goto done;
done:
	return acc;
}
int runtest(void){
	if(run(0)!=1) return 1;
	if(run(1)!=2) return 2;
	if(run(2)!=4) return 3;
	if(run(5)!=-1) return 4;
	return 0;
}`
	require.Equal(t, 0, runC(t, src))
}

// TestStaticLabelTableAmd64 exercises a static dispatch table of &&label
// addresses (the threaded-interpreter pattern), which places each block's
// address as a relocation in .data.
func TestStaticLabelTableAmd64(t *testing.T) {
	src := `
int vm(const int *code){
	static void *const tab[] = {&&ADD, &&MUL, &&HALT};
	int pc=0, acc=0;
	goto *tab[code[pc]];
ADD: acc += 5; pc++; goto *tab[code[pc]];
MUL: acc *= 3; pc++; goto *tab[code[pc]];
HALT: return acc;
}
int runtest(void){
	int prog[] = {0,0,1,2};      /* (0+5+5)*3 = 30 */
	if(vm(prog)!=30) return 1;
	int p2[] = {0,2};            /* 0+5 = 5 */
	if(vm(p2)!=5) return 2;
	return 0;
}`
	require.Equal(t, 0, runC(t, src))
}
