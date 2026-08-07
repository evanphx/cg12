package goc

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// This file is the deliverable of "the cacheable unit is a function, not a
// package".
//
// goc/packagelowering_test.go proved the *package* property: the packages two
// programs share lower to the same IR in both. That is not the property a
// function-granular cache needs. A per-function cache holds one function of a
// package and leaves the rest of the package to the program -- so what it needs
// is that the one function does not depend on the rest, and in particular that a
// non-generic function does not depend on WHICH instantiations its package was
// asked to carry. That is exactly the dependency the package-granular
// classification assumed existed, and it is why Stage 1 excluded 52-89% of
// lowered functions.
//
// The pair below is built to make the question sharp rather than to be
// representative. Both programs call runtime.AddCleanup, which is generic, with
// types declared in their own `main` package. So each program makes the shared
// `runtime` package carry instantiations that (a) the other program does not
// have, and (b) could not possibly be predicted from `runtime`'s own source,
// because the type arguments are declared in the importer. The same holds for
// `slices` through slices.SortFunc. If a non-generic function's lowering were
// sensitive to its package's instantiation set at all, this pair would show it.

// programCleanupTracked and programCleanupPayload share runtime, slices and
// strconv, and instantiate the generics of the first two over disjoint sets of
// locally-declared types.
const programCleanupTracked = `package main

import (
	"runtime"
	"slices"
	"strconv"
)

type tracked struct{ n int }
type note struct{ label int }

func main() {
	subject := &tracked{7}
	runtime.AddCleanup(subject, func(n note) { println("gone", n.label) }, note{1})
	all := []*tracked{{3}, {1}, {2}}
	slices.SortFunc(all, func(left, right *tracked) int { return left.n - right.n })
	println(strconv.Itoa(subject.n), all[0].n)
}
`

const programCleanupPayload = `package main

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
)

type other struct{ s string }
type payload struct{ text string }

func main() {
	subject := &other{"x"}
	runtime.AddCleanup(subject, func(p payload) { println("gone", p.text) }, payload{"p"})
	all := []*other{{"ccc"}, {"a"}}
	slices.SortFunc(all, func(left, right *other) int { return len(left.s) - len(right.s) })
	println(strconv.Itoa(len(subject.s)), strings.ToUpper(all[0].s))
}
`

// instantiationsOf is every instantiation symbol the module carries for one
// package, sorted.
func instantiationsOf(module *ir.Module, path string) []string {
	var symbols []string
	for _, function := range module.Funcs {
		if function.Start == nil || !strings.HasPrefix(function.Name, path+".") {
			continue
		}
		if IsGenericInstanceSymbol(function.Name) {
			symbols = append(symbols, function.Name)
		}
	}
	sort.Strings(symbols)
	return symbols
}

// TestFunctionLoweringDoesNotDependOnItsPackagesInstantiations is the property a
// function-granular cache needs and a package-granular one does not: a function
// that is not itself an instantiation lowers to the same bytes in two programs
// that make its package carry entirely different instantiations.
func TestFunctionLoweringDoesNotDependOnItsPackagesInstantiations(t *testing.T) {
	t.Parallel()

	tracked, err := CompileExecutableFor(TargetARM64, "tracked.go", []byte(programCleanupTracked))
	require.NoError(t, err)
	payload, err := CompileExecutableFor(TargetARM64, "payload.go", []byte(programCleanupPayload))
	require.NoError(t, err)

	// The comparison is worth nothing if the two programs happen to ask for the
	// same instantiations, so that is checked before anything else and checked on
	// the package that matters. `runtime` is the package Stage 1's exclusion cost
	// the most: one package, roughly a third of the module.
	leftRuntime := instantiationsOf(tracked, "runtime")
	rightRuntime := instantiationsOf(payload, "runtime")
	require.NotEmpty(t, leftRuntime, "the first program makes runtime carry no instantiation, so this proves nothing")
	require.NotEmpty(t, rightRuntime, "the second program makes runtime carry no instantiation, so this proves nothing")
	require.Empty(t, intersect(leftRuntime, rightRuntime),
		"the two programs ask runtime for the same instantiations, so this proves nothing")
	// And they are parameterised by types the importing program declares, which
	// is the case no per-package unit could ever contain.
	require.True(t, anyNames(leftRuntime, "main."), "runtime's instantiations do not use a type from the importing program: %v", leftRuntime)
	require.True(t, anyNames(rightRuntime, "main."), "runtime's instantiations do not use a type from the importing program: %v", rightRuntime)
	t.Logf("runtime carries %d instantiations in one program and %d in the other, disjoint:\n  %s\n  %s",
		len(leftRuntime), len(rightRuntime), strings.Join(leftRuntime, "\n  "), strings.Join(rightRuntime, "\n  "))

	paths := ModuleImportPaths(tracked, payload)
	leftDigests := loweredFunctionDigestsAll(t, tracked)
	rightDigests := loweredFunctionDigestsAll(t, payload)
	classification := make(map[string]CacheUnitReason, len(tracked.Funcs))
	for _, function := range tracked.Funcs {
		classification[function.Name] = ClassifyCacheUnit(function)
	}

	// Which packages declare a generic at all, from Stage 1's own classifier, so
	// the two accountings are read off the same programs.
	eligibility, err := ClassifyPackageCacheEligibility(TargetARM64, "tracked.go", []byte(programCleanupTracked))
	require.NoError(t, err)
	declaresGeneric := make(map[string]bool, len(eligibility.Generic))
	for _, path := range eligibility.Generic {
		declaresGeneric[path] = true
	}

	type packageResult struct {
		cacheableShared     int
		cacheableIdentical  int
		cacheableDiffering  []string
		instantiationShared int
		instantiationSame   int
		generatedShared     int
		generatedSame       int
		generatedDiffer     []string
	}
	results := map[string]*packageResult{}
	resultFor := func(path string) *packageResult {
		existing, ok := results[path]
		if !ok {
			existing = &packageResult{}
			results[path] = existing
		}
		return existing
	}

	for name, digest := range leftDigests {
		other, shared := rightDigests[name]
		if !shared {
			continue
		}
		path := PackageOfFunction(name, paths)
		// The two programs' own `main` packages are different source and are
		// expected to differ; a cache would never share them either.
		if path == "" || path == "main" {
			continue
		}
		result := resultFor(path)
		switch classification[name] {
		case CacheUnitInstantiation:
			result.instantiationShared++
			if other == digest {
				result.instantiationSame++
			}
			continue
		case CacheUnitInterfaceCallWrapper, CacheUnitInterfaceMethodDispatcher:
			result.generatedShared++
			if other == digest {
				result.generatedSame++
			} else {
				result.generatedDiffer = append(result.generatedDiffer, name)
			}
			continue
		}
		result.cacheableShared++
		if other == digest {
			result.cacheableIdentical++
		} else {
			result.cacheableDiffering = append(result.cacheableDiffering, name)
		}
	}

	packages := make([]string, 0, len(results))
	for path := range results {
		packages = append(packages, path)
	}
	sort.Strings(packages)

	shared, identical := 0, 0
	genericShared, genericIdentical, genericPackages := 0, 0, 0
	instantiationShared, instantiationSame := 0, 0
	generatedShared, generatedSame := 0, 0
	var generatedDiffering []string
	var offenders []string
	for _, path := range packages {
		result := results[path]
		shared += result.cacheableShared
		identical += result.cacheableIdentical
		instantiationShared += result.instantiationShared
		instantiationSame += result.instantiationSame
		generatedShared += result.generatedShared
		generatedSame += result.generatedSame
		generatedDiffering = append(generatedDiffering, result.generatedDiffer...)
		if declaresGeneric[path] {
			genericPackages++
			genericShared += result.cacheableShared
			genericIdentical += result.cacheableIdentical
		}
		if len(result.cacheableDiffering) == 0 {
			continue
		}
		sample := result.cacheableDiffering
		sort.Strings(sample)
		if len(sample) > 6 {
			sample = sample[:6]
		}
		offenders = append(offenders, fmt.Sprintf("  %-32s %4d of %4d cacheable functions differ: %s",
			path, len(result.cacheableDiffering), result.cacheableShared, strings.Join(sample, ", ")))
	}

	runtimeResult := results["runtime"]
	require.NotNil(t, runtimeResult, "the two programs share no runtime function")
	t.Logf("%d packages shared, %d cacheable functions lowered by both, %d identical",
		len(packages), shared, identical)
	t.Logf("of those, %d packages declare a generic and contribute %d cacheable functions, %d identical",
		genericPackages, genericShared, genericIdentical)
	t.Logf("runtime alone: %d cacheable functions shared, %d identical",
		runtimeResult.cacheableShared, runtimeResult.cacheableIdentical)
	t.Logf("instantiations shared by both programs: %d, %d identical",
		instantiationShared, instantiationSame)
	// Reported, never asserted. These two families are excluded because their
	// bodies are chosen from the assembled program, not because they were
	// observed to differ; a pair in which both programs lower the same methods
	// will not make them differ. The number is here so that whoever decides
	// whether a cache can hold a wrapper and re-run the redirect step has the
	// measurement in front of them rather than an intuition.
	sort.Strings(generatedDiffering)
	t.Logf("interface-call wrappers and dispatchers shared by both programs: %d, %d identical; %d differ: %s",
		generatedShared, generatedSame, len(generatedDiffering), strings.Join(generatedDiffering, ", "))

	if len(offenders) > 0 {
		t.Errorf("a function's lowering depends on something outside its own package: %d of %d cacheable functions differ between the two programs\n%s",
			shared-identical, shared, strings.Join(offenders, "\n"))
	}
	// The runtime figure is asserted separately from the whole-closure one so a
	// regression confined to runtime cannot hide inside a large denominator.
	require.Equal(t, runtimeResult.cacheableShared, runtimeResult.cacheableIdentical,
		"runtime's cacheable functions are not identical between the two programs")
	require.Greater(t, runtimeResult.cacheableShared, 1000,
		"runtime contributed too few shared functions for this to be the case it is meant to be")

	// An instantiation shared by both programs lowers identically too. That is not
	// what licenses the cache -- WHICH instantiations exist is still a
	// whole-program fact, which is why they are their own units -- but it does say
	// an instantiation is keyable on its origin and type arguments alone.
	require.Equal(t, instantiationShared, instantiationSame,
		"an instantiation present in both programs lowered differently")
}

func intersect(left, right []string) []string {
	inRight := make(map[string]bool, len(right))
	for _, name := range right {
		inRight[name] = true
	}
	var both []string
	for _, name := range left {
		if inRight[name] {
			both = append(both, name)
		}
	}
	return both
}

func anyNames(symbols []string, needle string) bool {
	for _, symbol := range symbols {
		if strings.Contains(symbol, needle) {
			return true
		}
	}
	return false
}
