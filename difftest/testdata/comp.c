/* The fast-path sentinel shape (as in Ruby's vm.c opcode handlers).
 *
 * `r` starts as the Qundef sentinel; a fast path may overwrite it; a single
 * `r == Qundef` test funnels the unhandled cases to the slow path. Because `r`
 * is used past that branch (it is returned), threading the sentinel test needs
 * real SSA reconstruction -- the value is available via the block's old
 * dominator but not via a bypassing predecessor. This is the Stage-3 target;
 * Stage 2 leaves it unchanged. Compiled by both cg12 and gcc and diffed.
 */
#include <stdio.h>

#define Qundef ((long)0x14)

long fix_plus(long a, long b) { return (((a >> 1) + (b >> 1)) << 1) | 1; }
long flo_plus(long a, long b) { return a + b + 2; }
long slow(long a, long b)     { return a * 100 + b; }

static int isflo(long x) { return (x & 3) == 2; }

long handler(long a, long b) {
	long r = Qundef;
	if ((a & 1) && (b & 1))
		r = fix_plus(a, b);
	else if (isflo(a) && isflo(b))
		r = flo_plus(a, b);
	if (r == Qundef)
		r = slow(a, b);
	return r;
}

int main(void) {
	long sum = 0;
	for (long a = 0; a < 40; a++)
		for (long b = 0; b < 40; b++)
			sum += handler(a, b);
	printf("%ld\n", sum);
	return 0;
}
