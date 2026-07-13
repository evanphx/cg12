#include <stdio.h>

// Enumerations: implicit numbering, an explicit value, and continuation from it.
enum Weekday { MON, TUE, WED, THU, FRI, SAT, SUN };
enum Flags { NONE = 0, READ = 1, WRITE = 2, EXEC = 4, RW = READ | WRITE };
enum Big { START = 100, NEXT, AFTER };

int is_weekend(enum Weekday d) {
	return d == SAT || d == SUN;
}

int main(void) {
	printf("days: %d %d %d %d\n", MON, WED, FRI, SUN);
	printf("flags: %d %d %d %d\n", READ, WRITE, EXEC, RW);
	printf("big: %d %d %d\n", START, NEXT, AFTER);
	printf("weekend: %d %d %d\n", is_weekend(MON), is_weekend(SAT), is_weekend(SUN));

	// An enum value used in a switch.
	enum Flags f = EXEC;
	switch (f) {
	case READ:
		printf("read\n");
		break;
	case EXEC:
		printf("exec\n");
		break;
	default:
		printf("other\n");
		break;
	}
	return 0;
}
