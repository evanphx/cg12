package link_test

// C constructs whose correctness only running shows, checked per backend. Each
// lives here rather than in difftest/ because difftest compares against the host
// gcc and so only ever runs arm64: the bugs below were worse on amd64, and one of
// them was invisible on arm64 entirely.

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
)

// C lets a goto jump into a block, past a declaration -- SQLite's VDBE
// interpreter does exactly this. The declaration's storage still exists on the
// jumped-to path, so this program returns 42; gcc agrees.
//
// In the IR the alloca lands in the block holding the declaration, and the use
// lands in the label's block, which is reachable without it. The alloca does not
// dominate its use, so the temporary holding the address is read on a path that
// never wrote it, and the backend's `lea`/`add` for it never ran.
const gotoPastDeclSrc = `
int probe(int x) {
	if (x) goto later;
	{
		int arr[4];
	later:
		arr[0] = 7;
		arr[1] = 35;
		return arr[0] + arr[1];
	}
}
int main(void) { return probe(1); }
`

func gotoPastDeclModule(t *testing.T, target cc.Target) *ir.Module {
	t.Helper()
	m, err := cc.CompileFor(target, "p.c", gotoPastDeclSrc)
	require.NoError(t, err)
	return m
}

// The arm64 backend hoisted allocas to the entry block from the start, so this
// has always passed there. It is here to hold the pair together: the answer must
// be the same on both, and it is the comparison that makes the amd64 result below
// mean something rather than look like a quirk of one target.
func TestArm64GotoPastDeclaration(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(gotoPastDeclModule(t, cc.TargetARM64)))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 42, runExe(t, exe), "the storage exists on the jumped-to path")
}

// The same program on amd64, which had no hoisting pass: it segfaulted, because
// the register that should hold arr's address was never loaded and the first
// store went wherever it happened to point.
//
// The pass was target-independent all along and lived in one backend, so the
// other inherited nothing. That is the failure this pair exists to prevent
// recurring -- a third backend would have inherited nothing too.
func TestAmd64GotoPastDeclaration(t *testing.T) {
	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(gotoPastDeclModule(t, cc.TargetAMD64)))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 42, runX86Static(t, exe), "the storage exists on the jumped-to path")
}

// va_copy on amd64, which segfaulted: modernc defines the builtin as a macro
// expanding to `dst = src`, and models a va_list as an 8-byte pointer, so the
// assignment copied 8 bytes where the state is longer. The destination kept the
// source's address rather than its state, and va_arg through it dereferenced
// that as a register-save area.
//
// The differential harness in difftest/ covers this against gcc, but only on
// arm64 -- where the same bug merely returned a wrong answer. Only the amd64 run
// showed it as a crash, and only this test runs it.
func TestAmd64VaCopy(t *testing.T) {
	m, err := cc.CompileFor(cc.TargetAMD64, "v.c", `
#include <stdarg.h>
static int sum(int n, ...) {
	va_list a, b;
	va_start(a, n);
	va_copy(b, a);
	int x = 0, y = 0;
	for (int i = 0; i < n; i++) x += va_arg(a, int);
	for (int i = 0; i < n; i++) y += va_arg(b, int);
	va_end(a); va_end(b);
	return x + y;
}
int main(void){ return sum(4, 1, 2, 3, 4) == 20 ? 0 : 1; }`)
	require.NoError(t, err)

	l := link.NewWith(amd64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 0, runX86Static(t, exe), "a copied va_list walks the same arguments")
}
