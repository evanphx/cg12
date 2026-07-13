// decompose: true
// A non-escaping array with constant indices: alias analysis resolves each
// element and the whole aggregate becomes SSA values.
#include <stdio.h>
int main(void){
	int v[3];
	v[0] = 10; v[1] = 20; v[2] = 30;
	printf("%d\n", v[0] + v[1] + v[2]);
	return 0;
}
