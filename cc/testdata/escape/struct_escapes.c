// decompose: false
// &p is handed to an external function, so the struct's address escapes and it
// must stay in memory.
#include <stdio.h>
#include <string.h>
struct Point { int x; int y; };
int main(void){
	struct Point p;
	p.x = 10; p.y = 20;
	memset(&p, 0, sizeof(struct Point));
	printf("%d\n", p.x + p.y);
	return 0;
}
