package cc_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRubric compiles each C program in testdata/rubric with cg12, runs it
// against libc, and checks its stdout against the sibling .out file.
func TestRubric(t *testing.T) {
	srcs, err := filepath.Glob("testdata/rubric/*.c")
	require.NoError(t, err)
	require.NotEmpty(t, srcs, "no rubric programs found")

	for _, src := range srcs {
		name := filepath.Base(src)
		t.Run(name[:len(name)-len(".c")], func(t *testing.T) {
			source, err := os.ReadFile(src)
			require.NoError(t, err)
			want, err := os.ReadFile(src[:len(src)-len(".c")] + ".out")
			require.NoError(t, err, "missing expected-output (.out) file")

			// Every program must produce the expected output both unoptimized and
			// after the cg12 optimizer runs (proving the passes are semantics-preserving
			// on real C).
			for _, o := range []bool{false, true} {
				name := "raw"
				if o {
					name = "opt"
				}
				t.Run(name, func(t *testing.T) {
					out, code := compileAndRunOpt(t, string(source), o)
					require.Equal(t, 0, code, "exit code")
					require.Equal(t, string(want), out)
				})
			}
		})
	}
}
