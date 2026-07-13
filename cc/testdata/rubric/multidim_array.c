#include <stdio.h>

// Multi-dimensional arrays: a[i] is itself an array that decays to its address
// when indexed again, rather than being loaded.
int grid[3][4];

int main(void) {
	int m[2][3] = {{1, 2, 3}, {4, 5, 6}};

	int sum = 0;
	for (int i = 0; i < 2; i++)
		for (int j = 0; j < 3; j++)
			sum += m[i][j];
	printf("m[1][2]=%d sum=%d\n", m[1][2], sum);

	// A global 2-D array, filled and read back.
	for (int i = 0; i < 3; i++)
		for (int j = 0; j < 4; j++)
			grid[i][j] = i * 10 + j;
	printf("grid[2][3]=%d grid[0][1]=%d\n", grid[2][3], grid[0][1]);

	// Row pointers: a decayed row indexes like a 1-D array.
	int *row = m[1];
	printf("row: %d %d %d\n", row[0], row[1], row[2]);
	return 0;
}
