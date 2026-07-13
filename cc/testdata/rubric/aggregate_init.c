#include <stdio.h>

struct Point { int x, y; };

// Global aggregates: a plain array, a designated/sparse array, a designated
// struct, an array of structs, a string pointer, and a char array from a string.
int table[4] = {10, 20, 30, 40};
int sparse[6] = {[5] = 99, [1] = 7};
struct Point origin = {.y = 2, .x = 1};
struct Point line[2] = {{7, 8}, {.x = 9}};
const char *greeting = "hello";
char tag[8] = "hi";

int main(void) {
	// Local brace initializers: full, partial (rest zero-filled), and nested.
	int local[3] = {1, 2, 3};
	int part[4] = {5};
	struct Point pts[2] = {{7, 8}, {.x = 9}};

	printf("table: %d %d %d %d\n", table[0], table[1], table[2], table[3]);
	printf("sparse: %d %d %d\n", sparse[1], sparse[5], sparse[0]);
	printf("origin: %d %d\n", origin.x, origin.y);
	printf("line: %d %d %d %d\n", line[0].x, line[0].y, line[1].x, line[1].y);
	printf("strings: %s %s\n", greeting, tag);
	printf("local: %d %d %d\n", local[0], local[1], local[2]);
	printf("part: %d %d %d %d\n", part[0], part[1], part[2], part[3]);
	printf("pts: %d %d %d %d\n", pts[0].x, pts[0].y, pts[1].x, pts[1].y);
	return 0;
}
