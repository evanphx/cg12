#include <stdio.h>

/* Accessing a member (or element) directly on a struct-returning call --
 * f().member, f().arr[i] -- where the returned struct is an unnamed temporary.
 * Regression for genAddr rejecting an aggregate rvalue as "not an lvalue".
 * Covers register-returned (small) and sret (large) structs. */

struct point {
	int x, y;
};
struct point mkpoint(int a) { return (struct point){a, a * 2}; }

struct big {
	long a, b, c, d;
};
struct big mkbig(void) { return (struct big){1, 2, 3, 4}; }

struct vec {
	int v[4];
};
struct vec mkvec(void) { return (struct vec){{10, 20, 30, 40}}; }

int main(void) {
	printf("x=%d y=%d\n", mkpoint(5).x, mkpoint(5).y);       /* 5 10 */
	printf("big=%ld\n", mkbig().a + mkbig().d);              /* 5 */
	printf("vec=%d\n", mkvec().v[2] + mkvec().v[0]);         /* 40 */
	printf("chain=%d\n", mkpoint(mkpoint(3).y).x);           /* mkpoint(6).x = 6 */
	return 0;
}
