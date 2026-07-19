package cc_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// compileAndRun compiles C source to an arm64 object with cg12, links it with the
// native toolchain (so libc is available), runs it, and returns stdout and the
// exit code.
func compileAndRun(t *testing.T, src string) (string, int) {
	return compileAndRunOpt(t, src, false)
}

// compileAndRunOpt is compileAndRun, optionally running the cg12 optimizer first.
func compileAndRunOpt(t *testing.T, src string, optimize bool) (string, int) {
	t.Helper()
	gcc := testenv.Tool(t, "gcc")

	mod, err := cc.Compile("main.c", src)
	require.NoError(t, err)
	if optimize {
		opt.OptimizeModule(mod)
	}
	code, err := arm64.CompileObject(mod)
	require.NoError(t, err)

	dir := t.TempDir()
	obj := filepath.Join(dir, "test.o")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(obj, code, 0o644))

	out, err := exec.Command(gcc, "-o", bin, obj).CombinedOutput()
	require.NoErrorf(t, err, "link: %s", out)

	cmd := exec.Command(bin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err = cmd.Run()
	ec := 0
	if ee, ok := err.(*exec.ExitError); ok {
		ec = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return stdout.String(), ec
}

// TestExplicitAtomicsRun compiles and runs the explicit atomic builtins across
// every width and every operation. The interpreter of these on a single thread is
// an ordinary load/modify/store, so this checks the value semantics; the atomicity
// is checked by the arm64 lowering test (an LDAXR/STLXR loop). The second half
// uses <stdatomic.h> on _Atomic-cast storage -- QuickJS's exact pattern, which
// runs through the __auto_type-based generic forms.
func TestExplicitAtomicsRun(t *testing.T) {
	_, code := compileAndRun(t, `
#include <stdint.h>
#include <stdatomic.h>
int main(void) {
	int i = 10;
	if (__atomic_fetch_add(&i, 5, __ATOMIC_SEQ_CST) != 10) return 1;   // old
	if (i != 15) return 2;
	if (__atomic_add_fetch(&i, 5, __ATOMIC_SEQ_CST) != 20) return 3;   // new
	if (__atomic_fetch_sub(&i, 3, __ATOMIC_SEQ_CST) != 20) return 4;   // i=17
	if (__atomic_fetch_and(&i, 0xF, __ATOMIC_SEQ_CST) != 17) return 5; // i=1
	if (__atomic_fetch_or(&i, 4, __ATOMIC_SEQ_CST) != 1) return 6;     // i=5
	if (__atomic_fetch_xor(&i, 1, __ATOMIC_SEQ_CST) != 5) return 7;    // i=4
	if (i != 4) return 8;

	__atomic_store_n(&i, 42, __ATOMIC_SEQ_CST);
	if (__atomic_load_n(&i, __ATOMIC_SEQ_CST) != 42) return 9;
	if (__atomic_exchange_n(&i, 7, __ATOMIC_SEQ_CST) != 42) return 10; // old
	if (i != 7) return 11;

	int exp = 7;
	if (!__atomic_compare_exchange_n(&i, &exp, 99, 0, __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST)) return 12;
	if (i != 99) return 13;
	exp = 7; // mismatch: no swap, exp updated to the value seen
	if (__atomic_compare_exchange_n(&i, &exp, 1, 0, __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST)) return 14;
	if (exp != 99 || i != 99) return 15;

	long L = 1000;                                                          // 64-bit
	if (__atomic_fetch_add(&L, 500, __ATOMIC_SEQ_CST) != 1000 || L != 1500) return 16;
	unsigned char b = 200;                                                  // 8-bit, wraps
	if (__atomic_fetch_add(&b, 100, __ATOMIC_SEQ_CST) != 200 || b != (unsigned char)44) return 17;
	unsigned short h = 0xFF00;                                              // 16-bit
	if (__atomic_fetch_or(&h, 0xFF, __ATOMIC_SEQ_CST) != 0xFF00 || h != 0xFFFF) return 18;

	// <stdatomic.h> on _Atomic-cast storage (the generic forms via __auto_type).
	uint32_t raw = 5;
	if (atomic_load((_Atomic(uint32_t) *)&raw) != 5) return 19;
	atomic_store((_Atomic(uint32_t) *)&raw, 9);
	if (raw != 9) return 20;
	if (atomic_fetch_add((_Atomic(uint32_t) *)&raw, 1) != 9 || raw != 10) return 21;
	uint32_t rexp = 10;
	if (!atomic_compare_exchange_strong((_Atomic(uint32_t) *)&raw, &rexp, 100) || raw != 100) return 22;
	return 0;
}`)
	require.Equal(t, 0, code)
}

func TestHelloWorld(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
int main(void) {
	printf("hello, world\n");
	return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "hello, world\n", out)
}
