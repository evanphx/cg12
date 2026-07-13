#include <stdio.h>

// A switch with fallthrough between cases, a default, and break.
const char *size_of(int n) {
	switch (n) {
	case 0:
		return "zero";
	case 1:
	case 2:
	case 3:
		return "small"; // 1, 2 and 3 all fall into this case
	case 10:
		return "ten";
	default:
		return "big";
	}
}

// switch driving accumulation with break, exercising fallthrough explicitly.
int weight(int c) {
	int w = 0;
	switch (c) {
	case 'a':
		w += 1;
		// fallthrough
	case 'b':
		w += 2;
		break;
	case 'c':
		w += 4;
		break;
	}
	return w;
}

int main(void) {
	int probe[] = {0, 1, 2, 3, 10, 42};
	for (int i = 0; i < 6; i++) {
		printf("%d -> %s\n", probe[i], size_of(probe[i]));
	}
	printf("weights: %d %d %d %d\n", weight('a'), weight('b'), weight('c'), weight('z'));
	return 0;
}
