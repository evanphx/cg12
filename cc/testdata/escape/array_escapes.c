// decompose: false
// &v[1] is handed to an external function, so v's address escapes and the array
// must stay in memory.
#include <stdio.h>
#include <string.h>
int main(void){
	int v[3];
	v[0] = 10; v[1] = 20; v[2] = 30;
	memset(&v[1], 0, sizeof(int));
	printf("%d\n", v[0] + v[1] + v[2]);
	return 0;
}
