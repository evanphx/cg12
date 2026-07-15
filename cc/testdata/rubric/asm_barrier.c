#include <stdio.h>

/* GNU inline assembly: modernc discards the template's register-binding
 * semantics, so cg12 recognizes a curated set of templates and lowers them to
 * defined IR. The empty template is the standard compiler barrier and `nop` is a
 * hint; both are no-ops for the values computed here, with input operands still
 * evaluated for their side effects. (Unrecognized asm is a hard error, never a
 * silent drop.) */

static int calls;
static int side(int x) {
	calls++;
	return x;
}

int accumulate(int n) {
	int sum = 0;
	for (int i = 0; i < n; i++) {
		sum += i;
		__asm__ volatile("" ::: "memory"); /* compiler barrier: no-op here */
	}
	__asm__("nop");
	/* the input operand's side effect (the call) must survive */
	__asm__ volatile("" : : "r"(side(100)) : "memory");
	return sum;
}

int main(void) {
	printf("sum=%d\n", accumulate(10)); /* 45 */
	printf("calls=%d\n", calls);        /* 1 */
	return 0;
}
