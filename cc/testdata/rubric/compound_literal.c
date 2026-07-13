#include <stdio.h>

// A compound literal (T){...} is an unnamed object: it can be read, passed by
// value, indexed, and have its address taken like any other lvalue.
struct Point { int x, y; };

int dist2(struct Point a, struct Point b) {
	int dx = a.x - b.x, dy = a.y - b.y;
	return dx * dx + dy * dy;
}

int sum(const int *a, int n) {
	int s = 0;
	for (int i = 0; i < n; i++)
		s += a[i];
	return s;
}

int main(void) {
	// Struct compound literal used as an initializer.
	struct Point o = (struct Point){3, 4};
	printf("struct: %d %d\n", o.x, o.y);

	// Passed by value directly as arguments.
	printf("dist2: %d\n", dist2((struct Point){0, 0}, (struct Point){3, 4}));

	// Array compound literal decays to a pointer when passed.
	printf("array-sum: %d\n", sum((int[]){10, 20, 30, 40}, 4));

	// Indexed in place.
	printf("index: %d\n", (int[]){7, 8, 9}[1]);

	// Address-of yields a pointer to the unnamed object.
	struct Point *p = &(struct Point){5, 6};
	printf("addr: %d %d\n", p->x, p->y);
	return 0;
}
