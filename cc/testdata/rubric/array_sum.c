#include <stdio.h>
int main(void) {
	int xs[5];
	for (int i = 0; i < 5; i++) xs[i] = i * i;
	int sum = 0;
	for (int i = 0; i < 5; i++) sum += xs[i];
	printf("%d\n", sum);
	return 0;
}
