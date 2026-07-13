package cc_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// optimizedIR compiles C to cg12 IR and runs the optimizer, returning the IR text.
func optimizedIR(t *testing.T, src string) string {
	t.Helper()
	mod, err := cc.Compile("main.c", src)
	require.NoError(t, err)
	opt.OptimizeModule(mod)
	return mod.String()
}

// TestEscapeDecomposition is the escape-analysis rubric, driven by the programs in
// testdata/escape. It shows a non-escaping compound value — a local array or
// struct — being decomposed into SSA variables (its stack allocation and every
// element/field load and store are eliminated), while an aggregate whose address
// escapes, or that is indexed dynamically, is proven unsafe to decompose and left
// in memory. Each program carries a "// decompose: true|false" directive and a
// sibling .out with its exact stdout, so the IR transform is checked and the
// program is run against libc to confirm the transform is correct.
func TestEscapeDecomposition(t *testing.T) {
	srcs, err := filepath.Glob("testdata/escape/*.c")
	require.NoError(t, err)
	require.NotEmpty(t, srcs)

	for _, path := range srcs {
		name := filepath.Base(path)
		t.Run(name[:len(name)-len(".c")], func(t *testing.T) {
			src := readFile(t, path)
			want := readFile(t, path[:len(path)-len(".c")]+".out")
			decompose := directiveBool(t, src, "decompose")

			ir := optimizedIR(t, src)
			if decompose {
				// The aggregate is gone: no allocation, and no element loads/stores.
				require.NotContainsf(t, ir, "alloc", "expected full decomposition, got:\n%s", ir)
				require.NotContains(t, ir, "load", "no element loads should remain")
				require.NotContains(t, ir, "store", "no element stores should remain")
			} else {
				require.Containsf(t, ir, "alloc", "expected the aggregate to stay in memory, got:\n%s", ir)
				require.True(t, strings.Contains(ir, "load") || strings.Contains(ir, "store"),
					"a kept aggregate is accessed through memory")
			}

			out, code := compileAndRunOpt(t, src, true)
			require.Equal(t, 0, code, "exit code")
			require.Equal(t, want, out)
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// directiveBool reads a "// <key>: true|false" directive from the source header.
func directiveBool(t *testing.T, src, key string) bool {
	t.Helper()
	marker := "// " + key + ":"
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i+len(marker):]) == "true"
		}
	}
	t.Fatalf("no %q directive in source", key)
	return false
}
