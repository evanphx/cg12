#include <stdio.h>

struct Point { int x, y; };
struct Box { int cells[6]; }; // 24 bytes: returned/passed via memory

struct Point make(int x, int y) {
	struct Point p;
	p.x = x;
	p.y = y;
	return p;
}

struct Point add(struct Point a, struct Point b) {
	return make(a.x + b.x, a.y + b.y);
}

int manhattan(struct Point p) {
	int ax = p.x < 0 ? -p.x : p.x;
	int ay = p.y < 0 ? -p.y : p.y;
	return ax + ay;
}

struct Box fill(int base) {
	struct Box b;
	for (int i = 0; i < 6; i++)
		b.cells[i] = base + i;
	return b;
}

int box_sum(struct Box b) {
	int s = 0;
	for (int i = 0; i < 6; i++)
		s += b.cells[i];
	return s;
}

int main(void) {
	struct Point a = make(3, 4);
	struct Point b = make(-1, 2);
	struct Point c = add(a, b);
	printf("add: (%d,%d)\n", c.x, c.y);
	printf("manhattan: %d %d\n", manhattan(a), manhattan(b));

	// Struct assignment copies by value.
	struct Point d = c;
	d.x = 100;
	printf("copy: c=(%d,%d) d=(%d,%d)\n", c.x, c.y, d.x, d.y);

	// A large (memory-class) struct returned and passed by value.
	struct Box box = fill(10);
	printf("box: first=%d last=%d sum=%d\n", box.cells[0], box.cells[5], box_sum(box));
	return 0;
}
