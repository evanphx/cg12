package cc_test

import (
	"testing"

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
