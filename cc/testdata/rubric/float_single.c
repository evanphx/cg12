#include <stdio.h>

// Single-precision float: literals, arithmetic, comparisons, and float<->double
// conversions must all keep the 32-bit class distinct from double.
struct Vec3 { float x, y, z; }; // a homogeneous float aggregate (HFA)

struct Vec3 scale(struct Vec3 v, float s) {
	v.x *= s;
	v.y *= s;
	v.z *= s;
	return v;
}

float dot(struct Vec3 a, struct Vec3 b) {
	return a.x * b.x + a.y * b.y + a.z * b.z;
}

int main(void) {
	float a = 3.5f, b = 1.5f;
	float avg = (a + b) / 2.0f;
	printf("avg=%.3f prod=%.3f quot=%.3f\n", avg, a * b, a / b);
	printf("cmp: %d %d\n", a > b, avg < 2.0f);

	double d = a;         // widen float -> double
	float back = d + 0.5; // narrow double -> float
	printf("conv: %.3f %.3f\n", d, back);

	struct Vec3 v = {1.0f, 2.0f, 3.0f};
	struct Vec3 w = scale(v, 2.0f);
	printf("hfa: (%.1f,%.1f,%.1f) dot=%.1f\n", w.x, w.y, w.z, dot(v, w));
	return 0;
}
