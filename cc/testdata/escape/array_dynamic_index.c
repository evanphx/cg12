// decompose: false
// The array is indexed by a runtime value, so the element offsets are not
// statically known and the aggregate cannot be decomposed.
#include <stdio.h>
int main(void){
	int v[4];
	for (int i = 0; i < 4; i++) v[i] = i * i;
	int s = 0;
	for (int i = 0; i < 4; i++) s += v[i];
	printf("%d\n", s);
	return 0;
}
