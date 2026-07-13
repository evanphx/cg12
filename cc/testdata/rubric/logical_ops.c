#include <stdio.h>
int main(void) {
	for (int i = 0; i < 4; i++) {
		int a = i & 1, b = (i >> 1) & 1;
		printf("%d%d: %d %d\n", a, b, a && b, a || b);
	}
	return 0;
}
