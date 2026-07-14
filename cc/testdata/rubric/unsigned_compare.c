#include <stdio.h>

/* The usual arithmetic conversions on a mixed signed/unsigned comparison: at
 * equal rank the signed operand converts to unsigned, so -1 compares greater
 * than a small unsigned, not less. Regression for a compare that used a signed
 * predicate for -1 < 1u. Wider-signed-vs-narrower-unsigned stays signed. */

int main(void) {
	unsigned u = 1;
	unsigned long ul = 1;
	long l = -1;
	int i = -1;

	printf("i<u=%d i>u=%d\n", i < u, i > u);       /* 0 1 : -1 -> UINT_MAX */
	printf("i<=u=%d i==u=%d\n", i <= u, (unsigned)i == u - 1 + 4294967295u); /* 0 ... */
	printf("i<ul=%d\n", i < ul);                   /* 0 : -1 -> ULONG_MAX  */
	printf("l<u=%d\n", l < u);                     /* 1 : long holds uint, signed */
	printf("neg1==umax=%d\n", i == 4294967295u);   /* 1 : both unsigned     */
	printf("both_signed=%d\n", i < 1);             /* 1 : plain signed      */
	printf("both_unsigned=%d\n", u < 2u);          /* 1                     */

	/* an unsigned loop bound with a signed index -- a classic footgun */
	int seen = 0;
	unsigned n = 3;
	for (int k = 0; k < (int)n; k++) seen++;
	printf("loop=%d\n", seen); /* 3 */
	return 0;
}
