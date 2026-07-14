#include <stdio.h>

/* Mixed floating/integer comparisons compare in the common type given by the
 * usual arithmetic conversions: a floating operand always dominates an integer
 * one, so the integer widens to the float type. Regression for a bug where a
 * double compared against a long truncated the double to long (3.5 == 3L came
 * out true). This is how SQLite decides whether a REAL is losslessly an
 * integer: pMem->u.r == ix. */

int main(void) {
	double d = 3.5;
	long l = 3;
	int i = 3;
	float f = 3.5f;

	printf("d==l:%d d==i:%d d==f:%d\n", d == l, d == i, d == f); /* 0 0 1 */
	printf("l==d:%d i==d:%d\n", l == d, i == d);                 /* 0 0 */
	printf("d<l:%d d<i:%d\n", d < l, d < i);                     /* 0 0 */
	printf("d>l:%d d>i:%d\n", d > l, d > i);                     /* 1 1 */

	double e = 3.0;
	printf("e==l:%d e==i:%d\n", e == l, e == i); /* 1 1 */

	long big = 9007199254740993L; /* 2^53 + 1, not exactly a double */
	double bd = (double)big;
	printf("big==bd:%d\n", big == bd); /* 0 */
	return 0;
}
