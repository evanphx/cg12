#include <stdio.h>

// A static local keeps its value across calls; each function has its own.
int next_id(void) {
	static int id = 0;
	return ++id;
}

int accumulate(int x) {
	static int total = 100;
	total += x;
	return total;
}

// goto driving a hand-written loop and an early exit from nested loops.
int first_pair(int target) {
	for (int i = 1; i <= 9; i++) {
		for (int j = 1; j <= 9; j++) {
			if (i * j == target) {
				goto found;
			}
		}
	}
	return -1;
found:
	return target;
}

int main(void) {
	printf("ids: %d %d %d\n", next_id(), next_id(), next_id());
	printf("acc: %d %d\n", accumulate(5), accumulate(20));

	int i = 0, sum = 0;
loop:
	if (i <= 5) {
		sum += i;
		i++;
		goto loop;
	}
	printf("goto sum: %d\n", sum);
	printf("pairs: %d %d\n", first_pair(12), first_pair(97));
	return 0;
}
