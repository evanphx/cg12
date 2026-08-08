// decompose: true
// A memset of a small constant size is not an escape. opt.ExpandSmallCopies
// rewrites it into the store it stands for before escape analysis has to decide
// anything, so &v[1] is never handed to an external function at all and the array
// decomposes like any other local.
//
// This is the same program as array_escapes.c with the volatile removed, and the
// pair is the point: whether the aggregate survives turns entirely on whether the
// length is known.
#include <stdio.h>
#include <string.h>
int main(void){
	int v[3];
	v[0] = 10; v[1] = 20; v[2] = 30;
	memset(&v[1], 0, sizeof(int));
	printf("%d\n", v[0] + v[1] + v[2]);
	return 0;
}
