package amd64_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPackedStructByValueAmd64 passes and returns packed structs by value on the
// System V ABI. The classifier sees the packed offsets, so a member that straddles
// an eightbyte boundary lands in the eightbytes it covers: struct M's double
// spans both, giving an INTEGER first eightbyte (it also holds the char) and an
// SSE second one. runtest returns 0 only when every result matches.
func TestPackedStructByValueAmd64(t *testing.T) {
	src := `
struct A { char c; int i; } __attribute__((packed));     // 5 bytes, one GP register
struct B { char c; long l; } __attribute__((packed));    // 9 bytes, two GP registers
struct M { char c; double d; } __attribute__((packed));  // 9 bytes: eightbyte 0 INTEGER, 1 SSE
long usea(struct A a){ return a.c + a.i; }
long useb(struct B b){ return b.c + b.l; }
double usem(struct M m){ return m.c + m.d; }
struct A mka(char c, int i){ struct A a = { c, i }; return a; }
int runtest(void){
	struct A a = { 3, 1000 };
	struct B b = { 7, 1000000000L };
	struct M m = { 2, 3.5 };
	struct A r = mka(9, 40);
	if (usea(a) != 1003) return 1;
	if (useb(b) != 1000000007L) return 2;
	if (usem(m) != 5.5) return 3;
	if (r.c + r.i != 49) return 4;
	return 0;
}`
	require.Equal(t, 0, runC(t, src))
}

// TestOddSizeAggByValueAmd64 guards the general fix behind the packed work: an
// eightbyte with a 3/5/6/7-byte tail (here a plain char array, no packing) must
// move all its bytes through the argument register, not just the largest
// power-of-two chunk. Before the fix a 5-byte struct dropped its fifth byte.
func TestOddSizeAggByValueAmd64(t *testing.T) {
	src := `
struct S5 { unsigned char b[5]; };
struct S7 { unsigned char b[7]; };
long sum5(struct S5 s){ return s.b[0]+s.b[1]+s.b[2]+s.b[3]+s.b[4]; }
long sum7(struct S7 s){ return s.b[0]+s.b[1]+s.b[2]+s.b[3]+s.b[4]+s.b[5]+s.b[6]; }
int runtest(void){
	struct S5 a = {{10, 20, 30, 40, 50}};       // fifth byte 50 must survive
	struct S7 b = {{1, 2, 4, 8, 16, 32, 64}};   // 127
	if (sum5(a) != 150) return 1;
	if (sum7(b) != 127) return 2;
	return 0;
}`
	require.Equal(t, 0, runC(t, src))
}
