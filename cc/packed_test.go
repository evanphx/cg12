package cc_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// A __attribute__((packed)) struct is laid out with no inter-member padding and
// alignment 1. cg12 reads member offsets from the type checker, so packed access
// through a pointer must land on the packed offsets, not the natural ones. The
// expected numbers are GCC's for the same source.
func TestPackedStructLayout(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stddef.h>
#include <stdio.h>
struct A { char c; int i; } __attribute__((packed));
struct __attribute__((packed)) C { char c; int i; };  // leading-attribute spelling
struct B { short s; char c; long l; int i; } __attribute__((packed));
struct D { char c; int i; };                           // not packed (control)
struct F { int i; char c; } __attribute__((packed, aligned(4)));
int main(void){
	printf("A %zu %zu %zu %zu\n", offsetof(struct A,c), offsetof(struct A,i), sizeof(struct A), _Alignof(struct A));
	printf("C %zu %zu %zu %zu\n", offsetof(struct C,c), offsetof(struct C,i), sizeof(struct C), _Alignof(struct C));
	printf("B %zu %zu %zu %zu %zu\n", offsetof(struct B,s), offsetof(struct B,c), offsetof(struct B,l), offsetof(struct B,i), sizeof(struct B));
	printf("D %zu %zu %zu %zu\n", offsetof(struct D,c), offsetof(struct D,i), sizeof(struct D), _Alignof(struct D));
	printf("F %zu %zu %zu %zu\n", offsetof(struct F,i), offsetof(struct F,c), sizeof(struct F), _Alignof(struct F));
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t,
		"A 0 1 5 1\n"+
			"C 0 1 5 1\n"+
			"B 0 2 3 11 15\n"+
			"D 0 4 8 4\n"+
			"F 0 4 8 4\n", out)
}

// Packed bitfields are laid out as a little-endian bit stream. Reading and
// writing them back through cg12 must agree with GCC (the expected values are
// GCC's for the same source), and a non-bitfield member interleaved with them
// lands on the flushed byte boundary.
func TestPackedBitfieldAccess(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
#include <string.h>
struct C { char c; unsigned a:5, b:5; int i; } __attribute__((packed));
struct A { unsigned a:3, b:5; } __attribute__((packed));
struct E { unsigned a:3; int i; unsigned b:4; } __attribute__((packed));
int main(void){
	printf("sizeC=%zu sizeA=%zu sizeE=%zu\n", sizeof(struct C), sizeof(struct A), sizeof(struct E));
	struct C s; memset(&s,0,sizeof s);
	s.c=0x7F; s.a=0x15; s.b=0x0A; s.i=0x11223344;
	printf("C c=%d a=%u b=%u i=%x\n", s.c, s.a, s.b, s.i);
	struct A t; memset(&t,0,sizeof t); t.a=6; t.b=25;
	printf("A a=%u b=%u\n", t.a, t.b);
	struct E e; memset(&e,0,sizeof e); e.a=5; e.i=-7; e.b=9;
	printf("E a=%u i=%d b=%u\n", e.a, e.i, e.b);
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t,
		"sizeC=7 sizeA=1 sizeE=6\n"+
			"C c=127 a=21 b=10 i=11223344\n"+
			"A a=6 b=25\n"+
			"E a=5 i=-7 b=9\n", out)
}

// A packed bitfield whose access unit would overrun the struct is refused rather
// than compiled to an out-of-bounds load: `b:40` needs a 6-byte span, which only
// an 8-byte unit covers, past the end of a 6-byte struct.
func TestPackedWideBitfieldRefused(t *testing.T) {
	_, err := cc.Compile("p.c", `struct W { char c:3; long l:40; } __attribute__((packed));
	                             long f(struct W *p){ return p->l; }`)
	require.Error(t, err, "a packed bitfield needing an out-of-bounds unit is refused")
	require.Contains(t, err.Error(), "packed")
}

// Beyond offsets: writing a packed struct's members must place the bytes where
// the packed layout says, so raw bytes read back at the packed offsets.
func TestPackedStructAccess(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
struct A { char c; int i; } __attribute__((packed));
int main(void){
	struct A a;
	a.c = 0x11;
	a.i = 0x22334455;              // stored little-endian across bytes 1..4
	unsigned char *p = (unsigned char*)&a;
	printf("%02x %02x%02x%02x%02x\n", p[0], p[4], p[3], p[2], p[1]);
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "11 22334455\n", out)
}
