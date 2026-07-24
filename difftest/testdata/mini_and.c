/* The && fast-path shape that motivates jump threading.
 *
 * gcc threads the materialized `(a & 1) && (b & 1)` boolean through the
 * consumer branch so each predecessor jumps straight to its target and the
 * booleans die (~5 instructions for f). cg12 today materializes the boolean
 * into a phi and re-branches on it (~20 instructions). This file is both a
 * differential case (compiled by cg12 and gcc, outputs compared) and the
 * objdump target for the instruction-count acceptance check on `f`.
 */
#include <stdio.h>

/* slow path -- deliberately distinguishable from the constant fast path. */
long slow(long a, long b) { return a * 100 + b; }

long f(long a, long b) {
	if ((a & 1) && (b & 1))
		return slow(a, b);
	return 20;
}

int main(void) {
	long sum = 0;
	for (long a = 0; a < 40; a++)
		for (long b = 0; b < 40; b++)
			sum += f(a, b);
	printf("%ld\n", sum);
	return 0;
}
