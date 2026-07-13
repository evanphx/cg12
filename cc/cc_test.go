package cc_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
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
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc not available")
	}

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
