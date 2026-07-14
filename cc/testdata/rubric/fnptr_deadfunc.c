#include <stdio.h>

/* A static function referenced only by its address -- selected by a conditional
 * that the optimizer turns into a phi -- must survive dead-function elimination,
 * and the indirect call through the resulting (possibly constant) pointer must
 * work. This mirrors SQLite's `xDestructor ? xDestructor : sqlite3NoopDestructor`
 * followed by an indirect call through the chosen pointer. */

static void noop(int *p) { (void)p; }
static void set42(int *p) { *p = 42; }

typedef void (*hook)(int *);

static hook pick(int useReal) {
	return useReal ? set42 : noop; /* conditional -> phi over two function addresses */
}

int main(void) {
	int x = 0;

	hook h = pick(1);
	(*h)(&x); /* explicit-deref indirect call */
	printf("after set42: x=%d\n", x); /* 42 */

	hook n = pick(0);
	n(&x); /* noop must be defined and callable */
	printf("after noop: x=%d\n", x); /* 42 */
	return 0;
}
