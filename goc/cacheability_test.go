package goc

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGenericPackagesAreExcludedFromCaching records how much of the standard
// library a per-package cache cannot hold at this stage, and why.
//
// It is a census with assertions rather than a threshold: the numbers move as
// the vendored standard library moves, and the value of the test is that they
// are written down and that the classification is exercised. What is asserted
// is that the classification answers -- the excluded set is a proper, sorted
// subset of the closure, and it contains packages that demonstrably declare
// generics.
//
// The second number is the one that matters for the design. Counting packages
// understates the exclusion badly, because `runtime` is one package and a third
// of the module, so the census also reports the share of *lowered functions*
// that live in an excluded package.
func TestGenericPackagesAreExcludedFromCaching(t *testing.T) {
	t.Parallel()

	for _, program := range []struct {
		name string
		src  string
	}{
		{"small", programSmall},
		{"wide", programWide},
	} {
		eligibility, err := ClassifyPackageCacheEligibility(TargetARM64, program.name+".go", []byte(program.src))
		require.NoError(t, err)
		require.NotEmpty(t, eligibility.Closure)

		excluded := make(map[string]bool, len(eligibility.Generic))
		for _, path := range eligibility.Generic {
			excluded[path] = true
		}
		require.True(t, sort.StringsAreSorted(eligibility.Generic), "the excluded set must be sorted")
		require.Less(t, len(eligibility.Generic), len(eligibility.Closure),
			"every package in the closure was excluded, which is not a classification")
		for _, path := range eligibility.Generic {
			require.Contains(t, eligibility.Closure, path)
		}
		// runtime declares generic atomics and map helpers; strconv declares
		// bsearch, which is why goc emits strconv.bsearch[[]uint16,uint16].
		require.True(t, excluded["runtime"], "runtime declares generics")
		require.True(t, excluded["strconv"], "strconv declares bsearch")

		module, err := CompileExecutableFor(TargetARM64, program.name+".go", []byte(program.src))
		require.NoError(t, err)
		inExcluded, attributed := 0, 0
		for _, f := range module.Funcs {
			path := packageOfFunction(f.Name, eligibility.Closure)
			if path == "" {
				continue
			}
			attributed++
			if excluded[path] {
				inExcluded++
			}
		}

		t.Logf("%s: %d of %d packages declare generics (%.0f%%); %d of %d attributed functions (%.0f%%) are in one\n  %s",
			program.name,
			len(eligibility.Generic), len(eligibility.Closure),
			100*float64(len(eligibility.Generic))/float64(len(eligibility.Closure)),
			inExcluded, attributed, 100*float64(inExcluded)/float64(attributed),
			strings.Join(eligibility.Generic, "\n  "))
	}
}
