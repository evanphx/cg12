package cc_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// cg12 builds an aggregate's type from its members' types, but a bitfield has no
// field type it can mirror -- its storage is packed bits, not a member of some
// class. So a struct with a bitfield becomes an opaque aggregate of the C size:
// enough for the ABI to classify it (GP registers when small, memory when large)
// and to copy its bytes, while an individual bitfield is still reached through a
// pointer at its own offset. This checks a bitfield struct survives crossing the
// ABI by value, in both register and memory classes, with its bits intact.
func TestBitfieldStructByValueWorks(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
struct Small { unsigned a:1, b:3, c:4; int x; };   // 8 bytes: two GP registers
struct Big   { long p, q; unsigned f:5; };         // 24 bytes: passed by reference
int  usesmall(struct Small s){ return s.a + s.b*10 + s.c*100 + s.x*1000; }
long usebig(struct Big b){ return b.p + b.q + b.f; }
int main(void){
	struct Small s = { 1, 5, 9, 7 };     // a=1 b=5 c=9 x=7 -> 7951
	struct Big   g = { 100, 200, 17 };   // 100+200+17 -> 317
	printf("%d %ld\n", usesmall(s), usebig(g));
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "7951 317\n", out)
}

// A memory-value argument (here an __int128) passed to a narrower scalar parameter
// is coerced to that scalar and travels as an ordinary value -- it must not be
// tagged as a by-value aggregate of the scalar's type (which has no aggregate).
func TestWideArgToScalarParam(t *testing.T) {
	_, code := compileAndRun(t, `
long low64(long x){ return x; }
int main(void){
	unsigned __int128 big = ((unsigned __int128)123 << 64) | 456;
	return low64(big) == 456 ? 0 : 1;   // truncated to the low 64 bits
}`)
	require.Equal(t, 0, code)
}

// Bitfields themselves are fine: member access uses the type checker's offsets,
// and the storage its size. Only crossing the ABI by value consults the
// aggregate type, so only that is refused.
func TestBitfieldsWorkWhenTheyDoNotCrossTheABI(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
struct W { unsigned a:1, b:3, c:4; };
static struct W g;
int main(void){
  struct W w;
  w.a = 1; w.b = 5; w.c = 9;
  g = w;                        /* whole-struct copy: not an ABI crossing */
  struct W *p = &g;             /* by pointer: likewise */
  printf("%u %u %u %zu\n", p->a, p->b, p->c, sizeof(struct W));
  return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "1 5 9 4\n", out, "bitfields pack as C says, and sizeof agrees")
}

// A struct whose layout cg12 does reproduce passes by value as before.
func TestOrdinaryStructsStillPassByValue(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
struct P { char c; int i; };
struct D { double x, y; };
static int usep(struct P p){ return p.c + p.i; }
static double used(struct D d){ return d.x + d.y; }
int main(void){
  struct P p = {2, 40};
  struct D d = {1.5, 2.5};
  printf("%d %g\n", usep(p), used(d));
  return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "42 4\n", out)
}

// A packed struct is now laid out with the packed offsets (see packed_test.go
// for the numbers), so access through a pointer or a value compiles. What is
// still refused is passing one by value across the ABI: cg12's aggregate types
// carry the natural layout and cannot describe a packed struct to the classifier.
func TestPackedStructByValueRefused(t *testing.T) {
	for _, src := range []string{
		`struct P { char c; int i; } __attribute__((packed));
		 int f(struct P *p){ return p->i; }`, // through a pointer
		`struct P { char c; int i; } __attribute__((packed)) pv;
		 int f(void){ return pv.i; }`, // through a value
	} {
		_, err := cc.Compile("p.c", src)
		require.NoError(t, err, "packed access uses the packed offsets and compiles")
	}
	_, err := cc.Compile("p.c", `struct P { char c; int i; } __attribute__((packed));
	                             int f(struct P p){ return p.i; }`)
	require.Error(t, err, "a packed struct cannot cross the ABI by value")
	require.Contains(t, err.Error(), "packed")
}

// The refusal must not catch an ordinary struct: the attribute is the trigger,
// not the shape. Every C program has structs like this one.
func TestUnpackedStructStillCompiles(t *testing.T) {
	_, err := cc.Compile("p.c", `struct P { char c; int i; };
	                             int f(struct P *p){ return p->i; }`)
	require.NoError(t, err)
}
