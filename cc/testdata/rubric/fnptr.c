#include <stdio.h>

/* Calling through a function pointer must work whether the pointer is called
 * directly (fp(x)) or with an explicit dereference ((*fp)(x)). The dereference
 * of a function pointer is a no-op: it yields a function designator that decays
 * right back to the same address. Regression for a bug where cg12 emitted an
 * extra load for (*fp)(...), calling the code bytes as an address. This is how
 * SQLite invokes scalar functions: (*pCtx->pFunc->xSFunc)(...). */

static int add(int a, int b) { return a + b; }
static int mul(int a, int b) { return a * b; }

struct ops {
	int (*fn)(int, int);
};

int main(void) {
	int (*fp)(int, int) = add;

	printf("plain=%d\n", fp(3, 4));      /* 7 */
	printf("deref=%d\n", (*fp)(3, 4));   /* 7 */
	printf("deref2=%d\n", (**fp)(3, 4)); /* 7 */

	struct ops o = {mul};
	printf("field=%d\n", o.fn(5, 6));       /* 30 */
	printf("field_deref=%d\n", (*o.fn)(5, 6)); /* 30 */

	int (*tbl[2])(int, int) = {add, mul};
	printf("tbl0=%d\n", (*tbl[0])(10, 20)); /* 30 */
	printf("tbl1=%d\n", (*tbl[1])(10, 20)); /* 200 */
	return 0;
}
