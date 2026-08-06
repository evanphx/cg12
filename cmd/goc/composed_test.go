package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// composedFloatProgram is a dependent chain of multiply-adds over two slices,
// the same shape as goc/testdata/perf_bench/float/main.go's dotProduct.
//
// It is the case the split default got wrong and the reason this test exists.
// dotProduct is entirely program-local -- it calls nothing outside main -- and
// yet compiling it against an object pack gave it a 160-byte frame and 95
// instructions where a monolithic build gives it 64 and 72. What differed was not
// the function but what the optimizer could see around it: subtract first and the
// module the optimizer runs over is ~600 functions whose callees are all external
// symbols, so nothing inlines and the escape and frame decisions change.
const composedFloatProgram = `package main

func dotProduct(left, right []float64) float64 {
	total := 0.0
	for i := range left {
		total += left[i] * right[i]
	}
	return total
}

func main() {
	left := make([]float64, 64)
	right := make([]float64, 64)
	for i := range left {
		left[i] = float64(i)
		right[i] = float64(i) * 0.5
	}
	println(int(dotProduct(left, right)))
}
`

// TestAComposedProgramIsCompiledLikeAMonolithicOne is the regression guard for
// the whole of this change, stated as the invariant rather than as a symptom:
// **every function a composed program module emits is the function a monolithic
// build emits.**
//
// It compares post-optimization IR rather than machine code, because that is
// where the difference is introduced -- the backend is the same backend either
// way -- and because a digest of ir.Func.String() is the comparison the rest of
// the tree's differential tooling already uses.
//
// Only the program's own package is compared. The rest of a composed module is
// subtracted and supplied by the pack's object, which was optimized in the pack's
// module rather than this program's; that is what an object pack is, and it is
// not what this test is about.
func TestAComposedProgramIsCompiledLikeAMonolithicOne(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a pack and compiles the same program twice")
	}
	pack := sharedOptimizedPrebuiltRuntime(t)

	monolithic, err := goc.CompileExecutable("float.go", []byte(composedFloatProgram))
	require.NoError(t, err)
	opt.OptimizeModule(monolithic)

	program, err := goc.CompileComposedExecutableAgainstRuntimeFor(
		goc.TargetARM64, "float.go", []byte(composedFloatProgram), &pack.Manifest)
	require.NoError(t, err)
	require.NotNil(t, program.Finish, "a composed build owes its caller the subtraction")

	// Before the subtraction the module is the whole program, which is the only
	// state the optimizer can inline a pack function into a program one from.
	require.Greater(t, len(program.Module.Funcs), len(monolithic.Funcs)/2,
		"a composed module must still hold the pack's functions when the optimizer runs")

	opt.OptimizeModule(program.Module)
	require.NoError(t, program.Finish(program.Module))
	assert.NotZero(t, program.SubtractedFunctions, "the subtraction still has to happen")

	monolithicDigests := programPackageDigests(monolithic)
	composedDigests := programPackageDigests(program.Module)
	require.NotEmpty(t, monolithicDigests)

	// The functions the program declares must be there in both. The rest of
	// package main's symbols are generated, and one of them is legitimately not:
	// a pack root is itself `package main` with a `func main() {}`, so it
	// generates the same go-internal func-value wrapper for `main` that any
	// program does, and its object defines it. The wrapper's body is `bl main_main`
	// against a symbol the pack deliberately leaves undefined, so the linker
	// resolves it to *this* program's main -- one thunk, correctly shared, and the
	// program's own copy is subtracted like any other duplicate.
	for _, declared := range []string{"main.main", "main.dotProduct"} {
		require.Contains(t, monolithicDigests, declared)
		require.Contains(t, composedDigests, declared, "a composed build must still emit the program's own functions")
	}

	var differing []string
	for name, digest := range composedDigests {
		if other, present := monolithicDigests[name]; present && other != digest {
			differing = append(differing, name)
		}
	}
	sort.Strings(differing)
	assert.Empty(t, differing,
		"a composed build must generate the program's own code exactly as a monolithic build does;\n"+
			"if this fires, the subtraction has moved back in front of the optimizer or something\n"+
			"else has changed what the optimizer sees. It is not a tolerance -- there is one right answer.")
}

// programPackageDigests digests every function of the compiled program's own
// package, keyed by name.
func programPackageDigests(module *ir.Module) map[string]string {
	digests := map[string]string{}
	for _, function := range module.Funcs {
		if !strings.HasPrefix(function.Name, "main.") {
			continue
		}
		sum := sha256.Sum256([]byte(function.String()))
		digests[function.Name] = hex.EncodeToString(sum[:8])
	}
	return digests
}
