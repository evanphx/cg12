package goc

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// This file answers the question Stage 2 could not start without: does a cached
// function survive the optimiser?
//
// Everything proven before it is about *lowering*. goc/functionlowering_test.go
// shows that two programs which make `runtime` carry disjoint instantiation sets
// still lower every one of runtime's other functions to identical bytes. That
// licenses a cache of *lowered* IR. It says nothing about what happens after the
// merge, when opt.OptimizeModule runs over the whole assembled module and the
// inliner may splice an instantiated body into a non-generic caller in the same
// package.
//
// The measurement below runs the full pipeline on the same two boundary programs
// and compares the *post-optimiser* form of the functions the lowering test found
// identical. Whichever way it comes out, it decides the design:
//
//   - identical => post-optimiser IR could be cached too, and the cache's ceiling
//     is the whole compile rather than the front end;
//   - divergent => the divergence is program-dependent, so a cache must either
//     capture it in the key or hold pre-optimiser IR and re-optimise after the
//     merge.
//
// It is reported, not asserted, for the same reason the wrapper counts in
// functionlowering_test.go are: the number is the input to a design decision, and
// a test that failed on it would be asserting that the inliner must not do its
// job.

// optimiserSurvival is one program pair's post-optimiser comparison.
type optimiserSurvival struct {
	// sharedLowered is the functions both programs lowered identically and that
	// the classifier called cacheable -- the set a cache would hold.
	sharedLowered int
	// survivedBoth is how many of those still exist in both modules after
	// optimisation. The optimiser deletes functions (deadfunc), so a cached entry
	// may simply not be in the finished module.
	survivedBoth int
	// identicalAfter is how many of the survivors are still byte-identical.
	identicalAfter int
	// differing names the ones that are not, with the reason where one is
	// available.
	differing []string
	// inlinedInto counts survivors whose post-optimiser body carries an inline
	// site, i.e. the inliner spliced something into them.
	inlinedInto int
}

// TestCacheableFunctionsAfterTheOptimiser measures whether the functions a
// function-granular cache may hold are still the same code in two programs once
// opt.OptimizeModule has run over each whole assembled module.
func TestCacheableFunctionsAfterTheOptimiser(t *testing.T) {
	t.Parallel()

	tracked, err := CompileExecutableFor(TargetARM64, "tracked.go", []byte(programCleanupTracked))
	require.NoError(t, err)
	payload, err := CompileExecutableFor(TargetARM64, "payload.go", []byte(programCleanupPayload))
	require.NoError(t, err)

	paths := ModuleImportPaths(tracked, payload)

	// Classify before optimising. After the inliner has run, a function's body no
	// longer tells you what family it was lowered as -- the position test in
	// synthesizedInterfaceDispatcher in particular reads a body the optimiser has
	// rewritten. The cache classifies at write time, which is pre-optimiser, so
	// that is what is recorded here.
	classification := make(map[string]CacheUnitReason, len(tracked.Funcs))
	for _, function := range tracked.Funcs {
		if function.Start == nil {
			continue
		}
		classification[function.Name] = ClassifyCacheUnit(function)
	}
	loweredIn := func(module *ir.Module) map[string]bool {
		names := make(map[string]bool, len(module.Funcs))
		for _, function := range module.Funcs {
			if function.Start != nil {
				names[function.Name] = true
			}
		}
		return names
	}
	rightLowered := loweredIn(payload)

	// The set a cache would hold: cacheable, lowered by both, not the root package.
	cacheable := map[string]bool{}
	for name, reason := range classification {
		if !reason.Cacheable() || !rightLowered[name] {
			continue
		}
		path := PackageOfFunction(name, paths)
		if path == "" || path == "main" {
			continue
		}
		cacheable[name] = true
	}
	require.Greater(t, len(cacheable), 1000, "too few shared cacheable functions for this to mean anything")

	opt.OptimizeModule(tracked)
	opt.OptimizeModule(payload)

	leftAfter := postOptimiserDigests(t, tracked)
	rightAfter := postOptimiserDigests(t, payload)
	leftInline := inlineSiteCounts(tracked)
	rightInline := inlineSiteCounts(payload)

	survival := optimiserSurvival{sharedLowered: len(cacheable)}
	byPackage := map[string]*optimiserSurvival{}
	rowFor := func(path string) *optimiserSurvival {
		row, ok := byPackage[path]
		if !ok {
			row = &optimiserSurvival{}
			byPackage[path] = row
		}
		return row
	}
	names := make([]string, 0, len(cacheable))
	for name := range cacheable {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		row := rowFor(PackageOfFunction(name, paths))
		row.sharedLowered++
		left, inLeft := leftAfter[name]
		right, inRight := rightAfter[name]
		if !inLeft || !inRight {
			continue
		}
		survival.survivedBoth++
		row.survivedBoth++
		if leftInline[name] > 0 || rightInline[name] > 0 {
			survival.inlinedInto++
			row.inlinedInto++
		}
		if left == right {
			survival.identicalAfter++
			row.identicalAfter++
			continue
		}
		survival.differing = append(survival.differing, name)
		row.differing = append(row.differing, name)
	}

	t.Logf("post-optimiser survival of the cacheable set (%s vs %s, -O):",
		"tracked.go", "payload.go")
	t.Logf("  %d cacheable functions lowered by both programs", survival.sharedLowered)
	t.Logf("  %d survive optimisation in both modules (%d were eliminated in at least one)",
		survival.survivedBoth, survival.sharedLowered-survival.survivedBoth)
	t.Logf("  %d of the survivors are byte-identical after the optimiser, %d differ (%.1f%% identical)",
		survival.identicalAfter, len(survival.differing),
		100*float64(survival.identicalAfter)/float64(max(survival.survivedBoth, 1)))
	t.Logf("  %d of the survivors had something inlined into them", survival.inlinedInto)

	packages := make([]string, 0, len(byPackage))
	for path := range byPackage {
		packages = append(packages, path)
	}
	sort.Strings(packages)
	for _, path := range packages {
		row := byPackage[path]
		if len(row.differing) == 0 {
			t.Logf("  %-28s %4d shared, %4d survive, all identical", path, row.sharedLowered, row.survivedBoth)
			continue
		}
		sample := append([]string(nil), row.differing...)
		if len(sample) > 8 {
			sample = sample[:8]
		}
		t.Logf("  %-28s %4d shared, %4d survive, %4d DIFFER: %s",
			path, row.sharedLowered, row.survivedBoth, len(row.differing), strings.Join(sample, ", "))
	}

	// The headline the report quotes.
	t.Logf("VERDICT: post-optimiser identity of cached functions is %d/%d = %.1f%%",
		survival.identicalAfter, survival.survivedBoth,
		100*float64(survival.identicalAfter)/float64(max(survival.survivedBoth, 1)))
}

// postOptimiserDigests digests every function of an optimised module the same
// way loweredFunctionDigestsAll does. It is a separate function only because it
// must not be confused with the pre-optimiser one at a call site: both renumber
// SrcPos.File in place, so a module may be digested exactly once.
func postOptimiserDigests(t *testing.T, module *ir.Module) map[string]string {
	t.Helper()
	digests := make(map[string]string, len(module.Funcs))
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		digest, err := loweredFunctionDigest(function)
		require.NoError(t, err, "digesting %s", function.Name)
		digests[function.Name] = digest
	}
	return digests
}

// inlineSiteCounts counts, per function, how many instructions carry an inline
// site -- i.e. how much of the body came from somewhere else.
func inlineSiteCounts(module *ir.Module) map[string]int {
	counts := make(map[string]int, len(module.Funcs))
	for _, function := range module.Funcs {
		total := 0
		for _, block := range function.Blocks {
			for index := range block.Instrs {
				if block.Instrs[index].Inl != nil {
					total++
				}
			}
		}
		counts[function.Name] = total
	}
	return counts
}

// TestOptimiserIsAFunctionOfTheWholeModule is the mechanism behind whatever
// TestCacheableFunctionsAfterTheOptimiser reports: it names, for one package,
// which cached functions had a body from a *different* cache unit spliced into
// them, using opt's own dependency recorder rather than inferring it from the
// finished IR.
func TestOptimiserIsAFunctionOfTheWholeModule(t *testing.T) {
	// opt.Record is process-global and refuses to nest, so this one may not run
	// in parallel with anything else that records.
	tracked, err := CompileExecutableFor(TargetARM64, "tracked.go", []byte(programCleanupTracked))
	require.NoError(t, err)

	classification := make(map[string]CacheUnitReason, len(tracked.Funcs))
	for _, function := range tracked.Funcs {
		if function.Start != nil {
			classification[function.Name] = ClassifyCacheUnit(function)
		}
	}
	paths := ModuleImportPaths(tracked)

	deps := opt.Record(func() { opt.OptimizeModule(tracked) })

	crossFamily := 0
	crossPackage := 0
	var examples []string
	for _, function := range tracked.Funcs {
		reason, known := classification[function.Name]
		if !known || !reason.Cacheable() {
			continue
		}
		host := PackageOfFunction(function.Name, paths)
		for _, spliced := range deps.Spliced(function) {
			splicedReason, known := classification[spliced.Name]
			if !known {
				continue
			}
			if !splicedReason.Cacheable() {
				crossFamily++
				if len(examples) < 12 {
					examples = append(examples, fmt.Sprintf("%s <- %s (%s)", function.Name, spliced.Name, splicedReason))
				}
			}
			if PackageOfFunction(spliced.Name, paths) != host {
				crossPackage++
			}
		}
	}
	t.Logf("inliner spliced a NON-cacheable body into a cacheable one %d times", crossFamily)
	t.Logf("inliner spliced across package boundaries %d times", crossPackage)
	for _, example := range examples {
		t.Logf("  %s", example)
	}
}
