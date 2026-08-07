package goc

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The bound was tested and the trigger was not, and that is the whole story of
// how a cache with a working size limit reached 1.41 GB against a 1 GiB bound.
//
// internal/cachefile's tests fill a directory and call Trim, and every one of
// them passed while the real path called Trim once a day, before its own writes,
// having already claimed the next 24 hours with a stamp it wrote first. A trimmer
// that is never reached is a trimmer that works.
//
// So this file does not call Trim. It compiles programs, which is the only thing
// that puts bytes in a cache directory, and then asks the directory how big it
// is. Everything between those two facts -- when eviction is triggered, whether
// it runs before or after the writes, whether a previous build's stamp locks it
// out -- is under test by being in the way.
//
// None of these call t.Parallel, and they must not: they move functionCacheBudget
// and functionCacheDefaultOn, which are process globals. t.Setenv already forbids
// it, and the ordering is what makes that safe -- go test runs the serial tests
// to completion before it resumes the parallel ones, so the cache tests that do
// run in parallel never see a budget somebody shrank.

// directorySize is every byte in a cache directory, by the same walk Trim uses
// but without Trim: both layouts, no exclusions, no policy. If this number is
// over the budget then the bound is not holding, whatever the trimmer would have
// done had anything called it.
func directorySize(t *testing.T, directory string) int64 {
	t.Helper()
	var total int64
	require.NoError(t, filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	}))
	return total
}

// compileGeneration compiles a program against directory as a distinct
// generation of units: every package's key differs from every other generation's,
// so nothing is a hit and the whole set is written again.
//
// The clause it moves is the optimisation pipeline rather than the compiler
// binary's hash. The two mint a generation identically -- both are key clauses
// covering every package -- and the compiler's hash is the one that does it in
// the field, but a test running inside one process cannot change the hash of the
// binary it is running. GOC_OPT_SKIP is the honest stand-in: it is a real key
// clause, changed from the environment, that a bisection run moves for real.
func compileGeneration(t *testing.T, directory string, generation int) {
	t.Helper()
	t.Setenv("CG12_NOCACHE", "")
	t.Setenv("CG12_FUNC_CACHE", directory)
	t.Setenv("GOC_OPT_SKIP", fmt.Sprintf("generation-%d", generation))

	_, err := CompileExecutableFor(TargetARM64, "trigger.go", []byte(programCacheSmall))
	require.NoError(t, err)
}

// TestTheCacheStaysUnderItsBudgetAcrossGenerations is the gate's finding as a
// test: a box that keeps minting new generations of units must not keep the old
// ones, and must not need a day to notice.
//
// The workload is the shape of the one that reached 1.41 GB -- generation after
// generation into one directory, faster than any daily interval can see -- scaled
// down by moving the budget rather than by waiting two hours. The budget is set
// to two generations from a measured one, so there is room for a working set and
// no room for a history.
func TestTheCacheStaysUnderItsBudgetAcrossGenerations(t *testing.T) {
	withDefaultOn(t)
	directory := t.TempDir()

	previous := functionCacheBudget
	t.Cleanup(func() { functionCacheBudget = previous })
	// Out of the way for the measuring compile: whatever one generation costs, it
	// is not a gibibyte.
	functionCacheBudget = previous

	compileGeneration(t, directory, 0)
	generation := directorySize(t, directory)
	require.Greater(t, generation, int64(0), "a cold compile stored nothing, so this test measures nothing")
	t.Logf("one generation is %d bytes", generation)

	functionCacheBudget = 2 * generation

	const generations = 8
	for round := 1; round <= generations; round++ {
		compileGeneration(t, directory, round)
		size := directorySize(t, directory)
		t.Logf("generation %d: %d bytes against a %d byte budget", round, size, functionCacheBudget)
		require.LessOrEqual(t, size, functionCacheBudget,
			"the directory is over its budget after generation %d -- cleanup did not run", round)
	}

	// And the counterfactual, so the assertion above cannot be satisfied by a
	// cache that simply never wrote anything: eight generations that were all kept
	// would be around eight times one, and the bound is two.
	final := directorySize(t, directory)
	require.Greater(t, final, generation/2,
		"the directory is far smaller than one generation, so nothing was being stored to evict")
	require.Less(t, final, generations*generation,
		"nothing was evicted at all")
	t.Logf("steady state after %d generations: %d bytes, budget %d, one generation %d",
		generations, final, functionCacheBudget, generation)
}

// TestTrimmingKeepsTheGenerationInUse is the property that makes the bound safe
// to enforce this often. Eviction is least-recently-used and a hit refreshes an
// entry, so the generation a build is actually using is the last thing to go: the
// compile after eight rounds of eviction still hits.
//
// Without it a working size bound is just a slower way of having no cache.
func TestTrimmingKeepsTheGenerationInUse(t *testing.T) {
	withDefaultOn(t)
	directory := t.TempDir()

	previous := functionCacheBudget
	t.Cleanup(func() { functionCacheBudget = previous })

	compileGeneration(t, directory, 0)
	generation := directorySize(t, directory)
	functionCacheBudget = 2 * generation

	// Four rounds against a two-generation budget, so eviction has run repeatedly
	// by the end and the first generation is long gone.
	const rounds = 4
	for round := 1; round <= rounds; round++ {
		compileGeneration(t, directory, round)
	}

	// The generation the last round wrote is the youngest, so it survived. Compile
	// it again and it should come back out of the cache.
	compileGeneration(t, directory, rounds)
	stats := LastFunctionCacheStats()
	t.Logf("after eviction: %s", stats)
	require.Greater(t, stats.PackagesHit, 10,
		"eviction took the working set with it")
	require.LessOrEqual(t, directorySize(t, directory), functionCacheBudget)
}

// TestEvictionRunsAfterTheWritesNotBefore pins the ordering the old code had
// backwards. Trimming first means every build ends over the budget by its own
// output even when the trigger fires, which is a bound that is never true at the
// moment anybody would look at it.
//
// A budget of one generation makes the difference unmissable: trim-then-write
// leaves two generations on disk, write-then-trim leaves one.
func TestEvictionRunsAfterTheWritesNotBefore(t *testing.T) {
	withDefaultOn(t)
	directory := t.TempDir()

	previous := functionCacheBudget
	t.Cleanup(func() { functionCacheBudget = previous })

	compileGeneration(t, directory, 0)
	generation := directorySize(t, directory)
	functionCacheBudget = generation

	compileGeneration(t, directory, 1)
	require.LessOrEqual(t, directorySize(t, directory), functionCacheBudget,
		"the build's own output is outside the bound, so eviction ran before the writes")
}
