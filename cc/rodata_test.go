package cc_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// C leaves writing through a cast away from const undefined precisely so an
// implementation may put the object where the hardware refuses. cg12 put const
// data and string literals in writable .data, so the write simply happened: this
// program printed "7 Hello" where gcc SIGSEGVs.
//
// Being const in the source is not what makes it read-only. The page it lands on
// is.
func TestConstDataIsNotWritable(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"a const object", `
const int ci = 42;
int main(void){ *(int*)&ci = 7; return ci; }`},
		{"a string literal", `
const char *msg = "hello";
int main(void){ ((char*)msg)[0] = 'H'; return msg[0]; }`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, code := compileAndRun(t, c.src)
			// runExe reports a signal as 128+signo; either way it is not a clean exit.
			require.NotEqual(t, 0, code, "the write must fault, not succeed")
		})
	}
}

// And reading it still works, which is the whole point of the section existing
// rather than the data simply being absent.
func TestConstDataIsReadable(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
const int ci = 42;
const char msg[] = "hello";
const double pi = 3.5;
const struct { int a; long b; } s = {1, 2};
int main(void){
	printf("%d %s %g %d %ld\n", ci, msg, pi, s.a, s.b);
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "42 hello 3.5 1 2\n", out)
}
