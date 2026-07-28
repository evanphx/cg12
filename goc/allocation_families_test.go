package goc

import (
	"go/build"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RUNTIME_PLAN.md section 6 asks for allocations across every size-class family
// in runtime/malloc_generated.go, and section 4.2 asks that a family which
// cannot be reached from supported Go be classified with a reason and, where
// possible, a test that proves the configuration boundary. This is that test.
//
// All 66 functions in malloc_generated.go are reached only through
// mallocScanTable and mallocNoScanTable, and runtime.mallocgc indexes those
// tables only when sizeSpecializedMallocEnabled is true. That constant is
// goexperiment.SizeSpecializedMalloc && ..., and internal/goexperiment selects
// between its "on" and "off" files on the goexperiment.sizespecializedmalloc
// build tag. goc builds the standard library with go/build's default context, so
// the tag is present only if the host toolchain's default GOEXPERIMENT has it.
//
// If this test starts failing, the experiment has become the default. The right
// response is to reclassify runtime/malloc_generated.go as required and give the
// alloc/* capabilities the specialized families to reach, not to weaken the
// test.
func TestSizeSpecializedMallocIsDisabledForThisBuild(t *testing.T) {
	const experimentTag = "goexperiment.sizespecializedmalloc"

	for _, tag := range build.Default.ToolTags {
		assert.NotEqual(t, experimentTag, tag,
			"the size-specialized malloc experiment is on by default now, so "+
				"runtime/malloc_generated.go is reachable and its classification in "+
				"cmd/goc/runtime_coverage_classifications.json is wrong")
	}

	// The file selection is the mechanism, so check it directly rather than
	// trusting the tag list to mean what it appears to mean.
	loader := newSourceLoader(nil, TargetARM64)
	require.NotEmpty(t, loader.root, "the source loader has no GOROOT")
	context := build.Default
	context.GOARCH = TargetARM64.GOARCH()
	context.CgoEnabled = false
	context.GOROOT = loader.root

	directory := filepath.Join(loader.root, "src", "internal", "goexperiment")
	pkg, err := context.ImportDir(directory, 0)
	require.NoError(t, err)

	selected := make(map[string]bool, len(pkg.GoFiles))
	for _, name := range pkg.GoFiles {
		selected[name] = true
	}
	assert.True(t, selected["exp_sizespecializedmalloc_off.go"],
		"internal/goexperiment did not select the disabled variant of the experiment")
	assert.False(t, selected["exp_sizespecializedmalloc_on.go"],
		"internal/goexperiment selected the enabled variant of the experiment")
}
