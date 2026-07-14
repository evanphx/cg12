#include <stdio.h>

/* C99 _Complex float/double: a complex value is modelled as a two-element
 * {real, imag} aggregate, ABI-compatible with how the target passes a complex
 * (a homogeneous float pair in SIMD registers). Arithmetic lowers to component
 * operations. Covers the a + b*I idiom (whose result modernc mis-types as real,
 * recovered structurally), literal imaginary constants, mixed real/complex
 * arithmetic, compound assignment, __real__/__imag__, conversions, comparison,
 * and passing/returning a complex by value. Component access uses __real__ /
 * __imag__ (operators) rather than creal/cimag so no libm link is needed. */

#define I (__extension__ 1.0iF)

static void show(const char *tag, double _Complex z) {
	printf("%s=%.1f%+.1fi\n", tag, __real__ z, __imag__ z);
}

double _Complex sq(double _Complex z) { return z * z; }

float _Complex fscale(float _Complex z, float k) { return z * k + 1.0f * I; }

int main(void) {
	double _Complex z = 2.0 + 3.0 * I; /* the standard idiom */
	double _Complex w = 1.0 - 1.0i;    /* literal imaginary */

	show("z", z); /* 2.0+3.0i */
	show("add", z + w);
	show("sub", z - w);
	show("mul", z * w);
	show("div", z / w);
	show("sq", sq(z)); /* -5+12i */

	double _Complex a = z;
	a += w;       /* 3 + 2i */
	a *= 2.0;     /* 6 + 4i */
	a -= 1.0 * I; /* 6 + 3i */
	show("cmp", a);

	printf("parts re=%.1f im=%.1f\n", __real__ z, __imag__ z);

	double _Complex rc = 5.0; /* real -> complex */
	show("rc", rc);
	printf("toreal=%.1f\n", (double)(z * w)); /* real part of 5+1i = 5 */

	printf("eq=%d ne=%d\n", z == z, z != w);

	float _Complex fz = fscale(2.0f + 1.0f * I, 3.0f); /* 6 + (3+1)i = 6+4i */
	printf("fz=%.1f%+.1fi\n", (double)__real__ fz, (double)__imag__ fz);

	return 0;
}
