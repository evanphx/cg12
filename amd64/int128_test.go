package amd64_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInt128 exercises __int128 on amd64 (run under qemu, freestanding). It
// covers the operations lowered inline -- add, subtract, bitwise, negation,
// comparison, and widening/narrowing conversions -- which also verify that a
// 16-byte integer value is passed and returned per the System V x86-64 ABI (a
// pair of general-purpose registers), carrying the full 128 bits. Multiply,
// divide, and shift use libgcc helpers unavailable in this freestanding link;
// they are covered natively on arm64 by the rubric program. Values are built by
// widening a long (no shift) and inspected by truncation or comparison.
func TestInt128(t *testing.T) {
	src := `
typedef __int128 i128;
typedef unsigned __int128 u128;

i128 dbl(i128 a, i128 b){ return a + b - (b - a); }  /* 2a, passes/returns 128-bit */
i128 widen(long x){ return (i128)x; }                /* sign-extend into hi */
long trunc(i128 v){ return (long)v; }

int runtest(void){
	i128 m1 = widen(-1);   /* hi=-1 lo=-1 : the value -1 */
	i128 p1 = widen(1);    /* hi=0  lo=1  */

	/* carry across the 64-bit boundary: -1 + 1 == 0 in both halves */
	if(m1 + p1 != 0) return 1;

	/* borrow across the boundary: 0 - 1 == -1 */
	if(widen(0) - p1 != m1) return 2;

	if(widen(5) + widen(3) != widen(8)) return 3;
	if(trunc(widen(5) + widen(3)) != 8) return 4;

	/* bitwise on low bits (hi=0 for small positives) */
	if(trunc(widen(0xFF) & widen(0x0F)) != 0x0F) return 5;
	if(trunc(widen(0xF0) | widen(0x0F)) != 0xFF) return 6;
	if(trunc(widen(0xFF) ^ widen(0x0F)) != 0xF0) return 7;
	if(~widen(0) != m1) return 8;              /* ~0 == -1 across both halves */

	/* negation and its two's-complement carry */
	if(-widen(5) != widen(-5)) return 9;
	if(-m1 != p1) return 10;                   /* -(-1) == 1 */

	/* comparison uses both halves (signed vs unsigned high compare) */
	if(!(widen(5) > widen(3))) return 11;
	if(!(m1 < p1)) return 12;                  /* signed: -1 < 1 */
	if(!((u128)m1 > (u128)p1)) return 13;      /* unsigned: 2^128-1 > 1 */
	if(widen(7) != widen(7)) return 14;

	/* conversions */
	if(trunc(m1) != -1) return 15;             /* narrow keeps low half */
	if(widen(-1) != m1) return 16;             /* widen sign-extends */
	if((long)(u128)widen(2) != 2) return 17;

	/* ABI: a 128-bit value with a nonzero high half survives arg + return */
	if(dbl(m1, p1) != widen(-2)) return 18;    /* 2*(-1) == -2 */
	if(trunc(dbl(widen(21), widen(0))) != 42) return 19;
	return 0;
}`
	require.Equal(t, 0, runC(t, src))
}
