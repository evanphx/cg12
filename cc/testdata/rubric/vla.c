#include <stdio.h>

/* Variable-length arrays (C99): storage sized by a runtime value, allocated on
 * the stack. Covers 1-D and 2-D VLAs, an expression-computed size, sizeof (a
 * runtime value), a VLA parameter, and a VLA inside a loop (which allocates and
 * is reclaimed each iteration via the frame pointer). */

int sum1d(int n) {
	int a[n];
	for (int i = 0; i < n; i++) a[i] = i * i;
	int s = 0;
	for (int i = 0; i < n; i++) s += a[i];
	return s;
}

int diag2d(int n, int m) {
	int a[n][m];
	for (int i = 0; i < n; i++)
		for (int j = 0; j < m; j++)
			a[i][j] = i * m + j;
	int s = 0;
	for (int i = 0; i < n && i < m; i++) s += a[i][i];
	return s;
}

/* VLA parameter (its size comes from an earlier parameter). */
int dot(int n, int x[n], int y[n]) {
	int s = 0;
	for (int i = 0; i < n; i++) s += x[i] * y[i];
	return s;
}

int loops(int n) {
	int last = 0;
	for (int k = 0; k < 64; k++) {
		int a[n + 1];
		for (int i = 0; i <= n; i++) a[i] = k + i;
		last = a[n];
	}
	return last;
}

int main(void) {
	printf("sum1d(5)=%d sum1d(10)=%d\n", sum1d(5), sum1d(10));
	printf("diag2d(4,5)=%d\n", diag2d(4, 5));

	int x[4] = {1, 2, 3, 4}, y[4] = {5, 6, 7, 8};
	printf("dot=%d\n", dot(4, x, y));

	int n = 6;
	int buf[n];
	printf("sizeof=%zu loops=%d\n", sizeof(buf), loops(7));
	return 0;
}
