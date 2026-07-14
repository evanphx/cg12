#include <stdio.h>

/* _Alignof(type) and _Alignof(expr): the type's alignment as an integer
 * constant. Regression for _Alignof(type) reaching codegen unhandled. */

struct mixed {
	char c;
	double d;
};

int main(void) {
	printf("char=%zu short=%zu int=%zu\n",
	       _Alignof(char), _Alignof(short), _Alignof(int));
	printf("long=%zu double=%zu ptr=%zu\n",
	       _Alignof(long), _Alignof(double), _Alignof(void *));
	printf("struct=%zu\n", _Alignof(struct mixed));

	double x;
	long y;
	printf("expr_double=%zu expr_long=%zu\n", _Alignof(x), _Alignof(y));

	/* usable in a constant context */
	char buf[_Alignof(double) * 2];
	printf("buf=%zu\n", sizeof(buf));
	return 0;
}
