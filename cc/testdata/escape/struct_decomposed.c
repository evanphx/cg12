// decompose: true
// A non-escaping struct whose fields are set from a runtime value: each field is
// a constant offset, so the whole struct becomes SSA values (p.x/p.y turn into
// argc*10 / argc*20 directly).
#include <stdio.h>
struct Point { int x; int y; };
int main(int argc, char **argv){
	struct Point p;
	p.x = argc * 10;
	p.y = argc * 20;
	printf("%d\n", p.x + p.y);
	return 0;
}
