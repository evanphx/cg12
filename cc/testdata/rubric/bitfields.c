#include <stdio.h>

// Bit fields pack several values into one storage unit. Reads extract and
// sign-extend; writes splice into the unit without disturbing its neighbours.
struct Flags {
	unsigned a : 3;
	unsigned b : 5;
	unsigned c : 1;
	int d : 20; // signed: negative values must sign-extend on read
};

// Three fields sharing a 16-bit unit (a classic RGB565-style packing).
struct Pixel {
	unsigned r : 5, g : 6, b : 5;
};

struct Flags global = {5, 20, 1, -3}; // exercises the constant bit-packed image

int main(void) {
	printf("global: %u %u %u %d sizeof=%lu\n",
	       global.a, global.b, global.c, global.d, sizeof(struct Flags));

	struct Flags f = {1, 2, 0, 7};
	printf("init: %u %u %u %d\n", f.a, f.b, f.c, f.d);

	f.a = 7;      // max for 3 bits
	f.b += 10;    // compound assignment: read-modify-write
	f.c = 1;
	f.d = -100;   // negative into a signed field
	printf("write: %u %u %u %d\n", f.a, f.b, f.c, f.d);

	f.a++;        // wraps 7 -> 0 within 3 bits
	--f.b;
	printf("incdec: %u %u\n", f.a, f.b);

	struct Pixel p = {31, 63, 16};
	p.g -= 3;
	printf("pixel: %u %u %u\n", p.r, p.g, p.b);
	return 0;
}
