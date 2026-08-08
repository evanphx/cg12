// decompose: false
// &v[1] is handed to an external function, so v's address escapes and the array
// must stay in memory.
//
// The length is read through a volatile so it cannot be folded. That matters:
// with a constant length, opt.ExpandSmallCopies rewrites the memset into a plain
// store, and then nothing has escaped and the array decomposes after all -- which
// is correct, and is what array_small_memset.c covers. Here the call has to
// survive for the escape it models to be real.
#include <stdio.h>
#include <string.h>
int main(void){
	int v[3];
	volatile int n = sizeof(int);
	v[0] = 10; v[1] = 20; v[2] = 30;
	memset(&v[1], 0, (size_t)n);
	printf("%d\n", v[0] + v[1] + v[2]);
	return 0;
}
