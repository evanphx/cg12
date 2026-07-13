#include <stdio.h>
int is_even(int n);
int is_odd(int n) { return n == 0 ? 0 : is_even(n - 1); }
int is_even(int n) { return n == 0 ? 1 : is_odd(n - 1); }
int main(void) {
	printf("%d %d\n", is_even(10), is_odd(10));
	return 0;
}
