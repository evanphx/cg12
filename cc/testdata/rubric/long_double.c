#include <stdio.h>

// long double is a 128-bit IEEE quad on AArch64. It has no hardware
// arithmetic, so cg12 lowers every operation to a libgcc soft-float call and
// passes the value in a SIMD register per the AAPCS. Full quad precision is
// preserved: results match those computed by gcc exactly.

long double g_pi = 3.14159265358979323846L; // exact quad constant in .data

struct Vec { long double x, y; }; // a struct of two quads (an HFA)

long double dot(struct Vec a, struct Vec b) {
	return a.x * b.x + a.y * b.y;
}

long double factorial(int n) {
	long double r = 1.0L;
	for (int i = 2; i <= n; i++)
		r *= (long double)i; // compound assignment, int -> quad conversion
	return r;
}

int main(void) {
	// Arithmetic and a division that needs more than double precision.
	long double a = 1.5L, b = 2.5L;
	printf("arith: %.4Lf %.4Lf %.4Lf %.4Lf\n", a + b, a - b, a * b, b / a);
	printf("pi=%.18Lf third=%.20Lf\n", g_pi, 1.0L / 3.0L);

	// Comparisons and unary minus.
	printf("cmp: %d %d %d %d\n", a < b, a == a, b > a, a >= b);
	printf("neg: %.2Lf abs=%.2Lf\n", -a, (a - b) < 0 ? -(a - b) : (a - b));

	// Conversions to and from long double.
	int i = -42;
	unsigned u = 3000000000U;
	long l = 9000000000L;
	printf("toLD: %.1Lf %.1Lf %.1Lf\n", (long double)i, (long double)u, (long double)l);
	double d = 2.718281828459045;
	printf("fromLD: %d %ld %.6f\n", (int)g_pi, (long)(g_pi * 100), (double)(g_pi + (long double)d));

	// A struct of quads passed by value, and a big exact result.
	struct Vec v1 = {1.0L, 2.0L}, v2 = {3.0L, 4.0L};
	printf("dot=%.2Lf 20!=%.0Lf\n", dot(v1, v2), factorial(20));

	// Arrays and a running sum.
	long double arr[3] = {1.5L, 2.5L, 3.5L};
	long double s = 0.0L;
	for (int k = 0; k < 3; k++)
		s += arr[k];
	printf("arrsum=%.2Lf\n", s);
	return 0;
}
