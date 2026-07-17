package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// The C standard leaves a plain char's signedness to the implementation, and the
// two targets answer differently: AArch64 says unsigned, x86-64 SysV says
// signed. cc hardcoded arm64 and amd64's own tests fed it C, so every one of
// them was getting AArch64's answers -- `char c = 200; c < 0` came out false
// where a real x86-64 compiler says true.
//
// Verified against the real thing: clang --target=x86_64 on this program, run
// under qemu, exits 1; the native arm64 gcc exits 0.
func TestPlainCharIsSignedOnThisTarget(t *testing.T) {
	const src = `int runtest(void){
		char c = 200;
		if (c >= 0) return 1;           /* x86-64: char is signed, so 200 is -56 */
		if ((int)c != -56) return 2;
		if ((unsigned char)c != 200) return 3;  /* and unsigned char is still unsigned */
		if ((signed char)c != -56) return 4;
		return 0;
	}`
	m, err := cc.CompileFor(cc.TargetAMD64, "c.c", src)
	require.NoError(t, err)
	require.Equal(t, 0, runObj(t, m), "a plain char is signed on x86-64")
}

// The target has to actually reach the type checker: a bad one is an error, not
// a silent fallback to whatever the default was.
func TestUnknownTargetIsRefused(t *testing.T) {
	_, err := cc.CompileFor("nosucharch", "c.c", `int f(void){ return 0; }`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nosucharch")
}
