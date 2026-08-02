package goc_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// TestSpikeReductionsFrameEscapes compiles the reductions ESCAPE_IR_PLAN.md
// argues from and prints what opt.FrameEscapes says about each. It is evidence,
// not a gate: the point of two of them is that the checker is silent on a
// program that is nonetheless miscompiled.
//
// The programs live in testdata/spike so that testdata/*.go -- the corpus glob
// TestFrameEscapeAudit and the corpus runner use -- does not pick them up.
func TestSpikeReductionsFrameEscapes(t *testing.T) {
	programs, err := filepath.Glob("testdata/spike/*.go")
	require.NoError(t, err)
	require.NotEmpty(t, programs)

	for _, program := range programs {
		source, err := os.ReadFile(program)
		require.NoError(t, err)
		module, err := goc.CompileExecutable(program, source)
		require.NoError(t, err, program)
		escapes := opt.FrameEscapes(module)
		fmt.Printf("SPIKE %s: %d frame-escape findings (unoptimized)\n", filepath.Base(program), len(escapes))
		for _, escape := range escapes {
			fmt.Printf("SPIKE   %s\n", escape.String())
		}
		// The audit runs on the unoptimized module. Run it again after the
		// standard pipeline: a finding that only shows up here was hidden by a
		// phi or a copy the front end emitted and the cleanup folds away, which
		// is a gap in the checker rather than a difference in the program.
		opt.OptimizeModule(module)
		optimized := opt.FrameEscapes(module)
		fmt.Printf("SPIKE %s: %d frame-escape findings (optimized)\n", filepath.Base(program), len(optimized))
		for _, escape := range optimized {
			fmt.Printf("SPIKE   %s\n", escape.String())
		}
	}
}
