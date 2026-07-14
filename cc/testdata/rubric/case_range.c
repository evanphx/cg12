#include <stdio.h>

/* GNU case ranges: "case lo ... hi:" matches every value in [lo, hi].
 * Regression for a range matching only its low value. Ranges mix with single
 * cases and a default; negative and character ranges work too. */

const char *classify(int n) {
	switch (n) {
	case 0:
		return "zero";
	case 1 ... 9:
		return "digit";
	case 10 ... 99:
		return "two-digit";
	case -50 ... -1:
		return "negative";
	default:
		return "big";
	}
}

const char *kind(char c) {
	switch (c) {
	case 'a' ... 'z':
		return "lower";
	case 'A' ... 'Z':
		return "upper";
	case '0' ... '9':
		return "digit";
	default:
		return "other";
	}
}

int main(void) {
	int probe[] = {0, 1, 5, 9, 10, 42, 99, 100, -1, -50, -51};
	for (int i = 0; i < 11; i++) {
		printf("%d -> %s\n", probe[i], classify(probe[i]));
	}
	char cs[] = {'m', 'Q', '7', '+'};
	for (int i = 0; i < 4; i++) {
		printf("%c -> %s\n", cs[i], kind(cs[i]));
	}
	return 0;
}
