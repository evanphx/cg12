#include <stdio.h>

// _Generic selects an expression by the controlling operand's type; a statement
// expression ({ ... }) yields its final expression; and the comma operator
// evaluates left to right, yielding the last operand.
#define type_tag(x) _Generic((x), \
	int: "int", double: "double", char *: "str", default: "other")

#define MAX(a, b) ({ __typeof__(a) _x = (a); __typeof__(b) _y = (b); _x > _y ? _x : _y; })

int grade(int s) { // nested ternary
	return s >= 90 ? 4 : s >= 80 ? 3 : s >= 70 ? 2 : s >= 60 ? 1 : 0;
}

int main(void) {
	int i = 0;
	double d = 0;
	char *s = 0;
	long l = 0;
	printf("generic: %s %s %s %s\n", type_tag(i), type_tag(d), type_tag(s), type_tag(l));

	printf("stmtexpr: %d %d\n", MAX(3, 7), MAX(9, 2));

	int a = 0, b = 10, c;
	for (a = 0, b = 10; a < b; a++, b--) // comma in for init and post
		;
	c = (a, b, a + b); // comma expression: value is the last operand
	printf("comma: %d %d %d\n", a, b, c);

	printf("grades: %d %d %d %d\n", grade(95), grade(72), grade(60), grade(40));
	return 0;
}
