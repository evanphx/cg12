package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// The mirror of amd64's: a plain char is UNSIGNED here, which is what the native
// gcc on this host says too. The same source compiled for the two targets is
// supposed to differ.
func TestPlainCharIsUnsignedOnThisTarget(t *testing.T) {
	const src = `int runtest(void){
		char c = 200;
		if (c < 0) return 1;            /* arm64: char is unsigned, so 200 is 200 */
		if ((int)c != 200) return 2;
		if ((signed char)c != -56) return 3;   /* and signed char is still signed */
		return 0;
	}`
	m, err := cc.Compile("c.c", src) // the default target is arm64
	require.NoError(t, err)
	_, code := buildAndRun(t, m, `
extern int runtest(void);
int main(void){ return runtest(); }`)
	require.Equal(t, 0, code, "a plain char is unsigned on AArch64")
}
