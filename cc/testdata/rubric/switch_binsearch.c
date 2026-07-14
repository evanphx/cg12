#include <stdio.h>

/* Switches large enough to be lowered as an ordered binary search, plus sparse,
 * negative, and unsigned case values, to check the dispatch matches a linear
 * chain for every probe. */

int many(int n) {
	switch (n) {
	case 1: return 101;
	case 2: return 102;
	case 3: return 103;
	case 5: return 105;
	case 8: return 108;
	case 13: return 113;
	case 21: return 121;
	case 34: return 134;
	case 55: return 155;
	case 89: return 189;
	case 144: return 244;
	case 233: return 333;
	default: return -1;
	}
}

int signedcases(int n) {
	switch (n) {
	case -100: return 1;
	case -7: return 2;
	case -1: return 3;
	case 0: return 4;
	case 42: return 5;
	case 1000: return 6;
	default: return 0;
	}
}

const char *classify(unsigned u) {
	switch (u) {
	case 0u: return "zero";
	case 1u: return "one";
	case 1000u: return "thousand";
	case 4000000000u: return "big"; /* > INT_MAX, needs unsigned compare */
	default: return "other";
	}
}

int main(void) {
	int probe[] = {0, 1, 3, 4, 8, 55, 100, 144, 233, 500};
	for (int i = 0; i < 10; i++) {
		printf("many(%d)=%d\n", probe[i], many(probe[i]));
	}
	int sp[] = {-100, -50, -7, -1, 0, 42, 999, 1000};
	for (int i = 0; i < 8; i++) {
		printf("signed(%d)=%d\n", sp[i], signedcases(sp[i]));
	}
	unsigned up[] = {0u, 1u, 2u, 1000u, 4000000000u, 4000000001u};
	for (int i = 0; i < 6; i++) {
		printf("classify(%u)=%s\n", up[i], classify(up[i]));
	}
	return 0;
}
