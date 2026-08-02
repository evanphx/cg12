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

// TestSpikeLoopRuleFires reports how many candidates ESCAPE_IR_PLAN.md stage 1's
// loop rule refuses to promote in each reduction. It is the control on the
// corpus measurement: the rule changes nothing across all 385 corpus programs,
// and a rule that never fires anywhere would be indistinguishable from a rule
// that is not wired up.
//
//	$ GOC_ESCAPE_LOOP=1 go test ./goc -run TestSpikeLoopRuleFires -count=1 -v
func TestSpikeLoopRuleFires(t *testing.T) {
	programs, err := filepath.Glob("testdata/spike/*.go")
	require.NoError(t, err)
	require.NotEmpty(t, programs)

	for _, program := range programs {
		source, err := os.ReadFile(program)
		require.NoError(t, err)
		opt.ResetLoopRuleEscapes()
		_, err = goc.CompileExecutable(program, source)
		require.NoError(t, err, program)
		fmt.Printf("SPIKE loop rule %s: on=%v escaped=%d\n",
			filepath.Base(program), opt.LoopCarriedCandidatesEscape, opt.LoopRuleEscapes())
	}
}
