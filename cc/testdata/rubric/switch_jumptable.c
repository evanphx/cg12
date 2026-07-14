#include <stdio.h>

/* Dense switches that lower to an indexed jump table: contiguous ranges, ranges
 * with gaps (gap values fall to the default), a negative-based range (nonzero
 * bias), and probes on and just past both ends of the range. */

int contiguous(int n) {
	switch (n) {
	case 0: return 10;
	case 1: return 11;
	case 2: return 12;
	case 3: return 13;
	case 4: return 14;
	case 5: return 15;
	case 6: return 16;
	case 7: return 17;
	case 8: return 18;
	case 9: return 19;
	default: return -1;
	}
}

int withgaps(int n) {
	switch (n) {
	case 100: return 1;
	case 101: return 2;
	case 103: return 3; /* 102 is a gap -> default */
	case 104: return 4;
	case 107: return 5; /* 105,106 gaps */
	case 108: return 6;
	case 109: return 7;
	case 110: return 8;
	default: return 0;
	}
}

int negbase(int n) {
	switch (n) {
	case -8: return 1;
	case -7: return 2;
	case -6: return 3;
	case -5: return 4;
	case -3: return 5; /* -4 gap */
	case -2: return 6;
	case -1: return 7;
	case 0: return 8;
	case 1: return 9;
	default: return 0;
	}
}

int main(void) {
	for (int i = -2; i <= 11; i++) {
		printf("c(%d)=%d\n", i, contiguous(i));
	}
	for (int i = 99; i <= 111; i++) {
		printf("g(%d)=%d\n", i, withgaps(i));
	}
	for (int i = -9; i <= 2; i++) {
		printf("n(%d)=%d\n", i, negbase(i));
	}
	return 0;
}
