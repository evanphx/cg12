package goc_test

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// A nosplit chain that does not fit. Each link keeps a large local array alive
// across its call, so the frames stack instead of folding into one another, and
// the three of them together are well past the 920-byte reserve linux/arm64
// keeps below g.stackguard0.
const noSplitOverflowProgram = `package main

//go:nosplit
func deepest(seed int) int {
	var scratch [512]byte
	for index := range scratch {
		scratch[index] = byte(seed + index)
	}
	return int(scratch[seed&0xff]) + int(scratch[(seed+1)&0xff])
}

//go:nosplit
func middle(seed int) int {
	var scratch [512]byte
	for index := range scratch {
		scratch[index] = byte(seed - index)
	}
	total := deepest(seed)
	return total + int(scratch[seed&0xff])
}

//go:nosplit
func top(seed int) int {
	var scratch [512]byte
	for index := range scratch {
		scratch[index] = byte(seed * index)
	}
	total := middle(seed)
	return total + int(scratch[seed&0xff])
}

func main() {
	if top(3) == 0 {
		println("zero")
	}
}
`

// The same shape, sized to fit: a budget that has never rejected anything is not
// evidence of anything, and neither is one that rejects everything.
const noSplitFittingProgram = `package main

//go:nosplit
func deepest(seed int) int {
	var scratch [64]byte
	for index := range scratch {
		scratch[index] = byte(seed + index)
	}
	return int(scratch[seed&0x3f])
}

//go:nosplit
func middle(seed int) int {
	var scratch [64]byte
	for index := range scratch {
		scratch[index] = byte(seed - index)
	}
	return deepest(seed) + int(scratch[seed&0x3f])
}

func main() {
	if middle(3) == 0 {
		println("zero")
	}
}
`

func requireARM64(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("the nosplit frame budget is measured against linux/arm64 frames")
	}
}

func compileWithBudget(t *testing.T, name, source string) error {
	t.Helper()
	module, err := goc.CompileExecutable(name, []byte(source))
	require.NoError(t, err, "compile to IR")
	opt.OptimizeModule(module)
	_, err = arm64.CompileObject(module)
	return err
}

// The budget fires, and the error is the one a person could act on.
func TestNoSplitBudgetRejectsAnOverflowingChain(t *testing.T) {
	t.Parallel()
	requireARM64(t)
	err := compileWithBudget(t, "nosplit_overflow.go", noSplitOverflowProgram)
	require.Error(t, err, "a nosplit chain over the reserve produced an object")

	message := err.Error()
	for _, want := range []string{
		"nosplit frame budget",
		"nosplit stack overflow",
		"main_middle",  // the chain, by name
		"main_deepest", //
		"920-byte limit",
		"over",
	} {
		require.Contains(t, message, want)
	}
	// The frame sizes have to be in there: "this chain is too big" is not
	// actionable, and "middle is 576 of the 920 bytes" is.
	require.Regexp(t, `\n\s+\d+\s+main_middle\n`, message)
	require.Regexp(t, `\n\s+\d+\s+main_deepest\n`, message)
}

func TestNoSplitBudgetAcceptsAChainThatFits(t *testing.T) {
	t.Parallel()
	requireARM64(t)
	require.NoError(t, compileWithBudget(t, "nosplit_fits.go", noSplitFittingProgram))
}

// The budget is a build failure, not a diagnostic: no object comes back.
func TestNoSplitBudgetProducesNoObject(t *testing.T) {
	t.Parallel()
	requireARM64(t)
	module, err := goc.CompileExecutable("nosplit_overflow.go", []byte(noSplitOverflowProgram))
	require.NoError(t, err)
	opt.OptimizeModule(module)
	object, err := arm64.CompileToObject(module)
	require.Error(t, err)
	require.Nil(t, object)
}

// The corpus builds, which is the other half of the claim: the register in
// arm64/nosplit_debt.go covers what was already over the reserve, and nothing
// this branch does adds to it.
func TestNoSplitBudgetAcceptsACorpusProgram(t *testing.T) {
	t.Parallel()
	requireARM64(t)
	const program = "testdata/runtime_lock_osthread.go"
	source, err := os.ReadFile(program)
	require.NoError(t, err)
	require.NoError(t, compileWithBudget(t, program, string(source)))
}

func TestNoSplitBudgetErrorNamesTheAllocatorChainWhenItIsForced(t *testing.T) {
	requireARM64(t)
	// GOC_NOSPLIT_LIMIT is how a test makes the budget fire without having to
	// construct 920 bytes of frames. Setting it also drops the debt register
	// (see checkNoSplitBudget), so what comes back is the runtime's real deepest
	// chain rather than the registered one.
	t.Setenv("GOC_NOSPLIT_LIMIT", "64")
	err := compileWithBudget(t, "nosplit_fits.go", noSplitFittingProgram)
	require.Error(t, err)
	require.Contains(t, err.Error(), "64-byte limit")
	require.True(t, strings.Contains(err.Error(), "nosplit stack overflow"))
}
