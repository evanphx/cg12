#include <stdio.h>
int slen(const char *s) { int n = 0; while (*s++) n++; return n; }
int main(void) {
	printf("%d\n", slen("hello, cg12"));
	return 0;
}
