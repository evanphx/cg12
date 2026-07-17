package cc_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// optIR compiles and optimizes, returning the IR text.
func optIR(t *testing.T, src string) string {
	t.Helper()
	m, err := cc.Compile("v.c", src)
	require.NoError(t, err)
	opt.OptimizeModule(m)
	return m.String()
}

// Reading a volatile object is an event, not merely a way to get its value: it
// is how MMIO registers, signal-handler flags, and another thread's variables
// are observed. Two reads must stay two reads.
func TestVolatileReadsAreNotFolded(t *testing.T) {
	plain := optIR(t, `int f(int *p){ int a = *p; int b = *p; return a + b; }`)
	require.Equal(t, 1, strings.Count(plain, "loaduw"),
		"an ordinary pointer's two reads may fold to one:\n%s", plain)

	vol := optIR(t, `int f(volatile int *p){ int a = *p; int b = *p; return a + b; }`)
	require.Equal(t, 2, strings.Count(vol, "loaduw"),
		"a volatile pointer's two reads must both happen:\n%s", vol)
}

// A volatile read whose value is discarded still has to happen -- that is the
// idiom for acknowledging a device register.
func TestVolatileReadWithUnusedResultSurvives(t *testing.T) {
	vol := optIR(t, `void f(volatile int *p){ (void)*p; }`)
	require.Equal(t, 1, strings.Count(vol, "loaduw"),
		"the discarded volatile read must survive:\n%s", vol)

	plain := optIR(t, `void f(int *p){ (void)*p; }`)
	require.Equal(t, 0, strings.Count(plain, "loaduw"),
		"an ordinary discarded read is dead:\n%s", plain)
}

// Writes to a volatile object are events too: every one must be performed, in
// order, even where the earlier value is never read.
func TestVolatileWritesAreNotFolded(t *testing.T) {
	vol := optIR(t, `void f(volatile int *p){ *p = 1; *p = 2; }`)
	require.Equal(t, 2, strings.Count(vol, "storew"),
		"both volatile writes must happen:\n%s", vol)
}

// A volatile local keeps its storage: promoting it to a register would turn
// every access into no access at all.
func TestVolatileLocalKeepsItsStorage(t *testing.T) {
	vol := optIR(t, `int f(void){ volatile int x = 1; x = 2; return x; }`)
	require.Contains(t, vol, "alloc", "the volatile local is not promoted:\n%s", vol)
	require.Equal(t, 1, strings.Count(vol, "loaduw"), "the read happens:\n%s", vol)

	plain := optIR(t, `int f(void){ int x = 1; x = 2; return x; }`)
	require.NotContains(t, plain, "loaduw", "an ordinary local promotes away:\n%s", plain)
}
