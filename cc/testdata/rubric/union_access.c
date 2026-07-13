#include <stdio.h>

// A union overlays its members at offset zero.
union Value {
	int i;
	float f;
	unsigned char bytes[4];
};

int main(void) {
	union Value v;
	v.i = 0x01020304;
	printf("bytes: %d %d %d %d\n", v.bytes[0], v.bytes[1], v.bytes[2], v.bytes[3]);

	v.f = 1.5f;
	printf("float: %.1f raw=%u\n", v.f, v.i);

	// Reinterpret an int's bytes through the union.
	union Value w;
	w.i = 65;
	printf("as-char: %c\n", w.bytes[0]);
	return 0;
}
