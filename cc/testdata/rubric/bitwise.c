#include <stdio.h>
int main(void) {
	unsigned x = 0xF0, y = 0x0F;
	printf("%u %u %u %u %u\n", x | y, x & y, x ^ y, x << 1, x >> 4);
	return 0;
}
