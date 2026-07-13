#include <stdio.h>

// Global pointers initialized with the address of another global become
// relocations in the data image: a bare array decays to its base, &arr[i] and
// &s.field add a constant offset, and a string literal points at interned data.
int table[] = {10, 20, 30, 40};
int *head = table;         // &table[0]
int *third = &table[2];    // &table[0] + 8

struct Cfg { int w, h; const char *name; };
struct Cfg cfg = {80, 24, "box"};
int *cfg_h = &cfg.h;       // &cfg + offsetof(h)

const char *msg = "hello"; // interned string
const char *names[] = {"alpha", "beta", "gamma"}; // array of string relocations

int main(void) {
	printf("array: %d %d\n", *head, *third);
	printf("struct: %d name=%s\n", *cfg_h, cfg.name);
	printf("string: %s\n", msg);
	for (int i = 0; i < 3; i++)
		printf("name[%d]=%s\n", i, names[i]);
	return 0;
}
