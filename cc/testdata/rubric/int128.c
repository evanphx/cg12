#include <stdio.h>

/* __int128 / unsigned __int128 (GCC extension): a 128-bit integer is modelled
 * as a {lo, hi} pair of 64-bit halves, passed and returned in a register pair.
 * Add, subtract, bitwise, negation, comparison, and int<->128 conversions are
 * lowered to 64-bit half operations; multiply, divide, remainder, shifts, and
 * float conversions defer to the libgcc __*ti3 helpers. Values are printed as
 * their two halves so no 128-bit printf support is needed. */

typedef __int128 i128;
typedef unsigned __int128 u128;

static void show(const char *tag, i128 v) {
	printf("%s=%ld:%lu\n", tag, (long)(v >> 64), (unsigned long)v);
}
static void showu(const char *tag, u128 v) {
	printf("%s=%lu:%lu\n", tag, (unsigned long)(v >> 64), (unsigned long)v);
}

i128 fib128(int n) { /* grows past 64 bits, exercising add by value */
	i128 a = 0, b = 1;
	for (int i = 0; i < n; i++) {
		i128 t = a + b;
		a = b;
		b = t;
	}
	return a;
}

int main(void) {
	i128 big = (i128)1000000000000LL * 1000000000000LL; /* 10^24 */
	show("mul", big);
	show("add", big + 5);
	show("sub", big - 1);
	show("div", big / 7);
	show("mod", big % 7);
	show("neg", -big);

	u128 u = ((u128)1 << 100); /* 2^100 */
	showu("shl", u);
	showu("shr", big >> 40);

	u128 a = ((u128)0xAAAAAAAAAAAAAAAAULL << 64) | 0x5555555555555555ULL;
	u128 b = ((u128)0x0FULL << 64) | 0xF0F0F0F0F0F0F0F0ULL;
	showu("and", a & b);
	showu("or", a | b);
	showu("xor", a ^ b);
	showu("cpl", ~a);

	i128 c = 100;
	c += (i128)1 << 64; /* set the high half */
	c *= 3;
	c <<= 4;
	show("compound", c);

	printf("cmp=%d %d %d %d\n", big > big + 1, (i128)-1 < 0, a == a, (u128)-1 > 0);

	printf("fib180 hi=%ld\n", (long)(fib128(180) >> 64));

	double df = (double)big;
	printf("tofloat=%g\n", df);
	i128 fromf = (i128)1e30;
	show("fromfloat", fromf);

	return 0;
}
