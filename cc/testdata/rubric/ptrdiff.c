#include <stdio.h>

/* Pointer arithmetic scales by the pointee's size in every form: p+n, n+p,
 * a[i], i[a], p-n, p+=n, and p-q (which divides the byte distance by the
 * element size). Regression for a bug where cg12 emitted the raw byte
 * difference for p - q, and for one where n + p / i[a] used the integer as the
 * base. Both corrupted any element-indexed access, e.g. SQLite's
 * (int)(pOp - p->aOp). */

struct wide { long a; int b; char c[13]; }; /* padded to 24 bytes */

long diff_wide(struct wide *p, struct wide *q) { return p - q; }
int diff_int(int *p, int *q) { return (int)(p - q); }
long diff_char(char *p, char *q) { return p - q; }
long diff_double(double *p, double *q) { return p - q; }

int main(void) {
	struct wide w[10];
	int a[10];
	char ac[10];
	double ad[10];

	printf("sizeof=%zu\n", sizeof(struct wide));
	printf("wide=%ld\n", diff_wide(&w[7], &w[2]));       /* 5 */
	printf("int=%d\n", diff_int(&a[9], &a[3]));          /* 6 */
	printf("char=%ld\n", diff_char(&ac[8], &ac[1]));     /* 7 */
	printf("double=%ld\n", diff_double(&ad[9], &ad[1])); /* 8 */
	printf("neg=%ld\n", diff_wide(&w[1], &w[6]));        /* -5 */

	int *p = a;
	a[3] = 30;
	a[5] = 50;
	printf("p+n=%d n+p=%d\n", *(p + 3), *(3 + p)); /* 30 30 */
	printf("aidx=%d idxa=%d\n", a[5], 5[a]);       /* 50 50 */
	p += 5;
	printf("padd=%d pdiff=%ld\n", *p, p - a); /* 50 5 */
	p -= 2;
	printf("psub=%d\n", *p); /* 30 */
	return 0;
}
