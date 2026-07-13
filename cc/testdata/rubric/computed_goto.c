#include <stdio.h>

// Computed goto (a GNU extension): &&label yields a label's address and
// goto *expr branches to it. This is the classic threaded-interpreter dispatch.
int run(const int *code, int n) {
	void *tab[] = {&&OP_ADD, &&OP_MUL, &&OP_SUB, &&OP_HALT};
	int pc = 0, acc = 0;
	goto *tab[code[pc]];
OP_ADD:
	acc += 5;
	pc++;
	goto *tab[code[pc]];
OP_MUL:
	acc *= 3;
	pc++;
	goto *tab[code[pc]];
OP_SUB:
	acc -= 2;
	pc++;
	goto *tab[code[pc]];
OP_HALT:
	return acc;
}

int main(void) {
	int prog[] = {0, 0, 1, 2, 3}; // ((0+5+5)*3) - 2 = 28
	printf("interp=%d\n", run(prog, 5));

	// A plain label address and a direct computed jump.
	void *dst = &&skip;
	int x = 1;
	goto *dst;
	x = 99; // skipped
skip:
	printf("x=%d\n", x);
	return 0;
}
