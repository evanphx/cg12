#include <stdio.h>
#include <stdbool.h>

// _Bool normalizes any nonzero value to 1, both on assignment and on cast.
bool toggle(bool b) { return !b; }

int main(void) {
	bool a = 5;      // stored as 1
	bool b = 0;
	_Bool c = a && !b;
	printf("vals: %d %d %d\n", a, b, c);
	printf("casts: %d %d %d\n", (bool)3, (bool)0, (bool)-7);
	printf("toggle: %d %d\n", toggle(a), toggle(b));

	// A bool from a float and from a pointer.
	double d = 0.0;
	int x = 0;
	printf("from: %d %d %d\n", (bool)d, (bool)2.5, (bool)&x);

	int count = 0;
	for (bool go = true; go; go = count < 3)
		count++;
	printf("loop: %d\n", count);
	return 0;
}
