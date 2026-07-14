#include <stdio.h>

/* A static pointer initialized with the address of a string literal -- even
 * through a cast, and even for a function-local static array -- must reference
 * the literal's data symbol, not be zeroed. Regression for a bug where cg12
 * emitted zero bytes for such slots, so SQLite's trimFunc dereferenced a NULL
 * azOne[0] and crashed. */

static const char *const words[] = {"alpha", "beta", "gamma"};

int main(void) {
	static const unsigned lenOne[] = {1};
	static unsigned char *const azOne[] = {(unsigned char *)" "};

	printf("lenOne=%u azOne=%c\n", lenOne[0], azOne[0][0]); /* 1 (space) */
	printf("azOne_ok=%d\n", azOne[0][0] == ' ');            /* 1 */

	for (int i = 0; i < 3; i++) {
		printf("words[%d]=%s\n", i, words[i]);
	}
	return 0;
}
