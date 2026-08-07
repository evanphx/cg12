package goc

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGenericInstanceSymbolPredicate pins the shapes the predicate has to get
// right. Each case here is a symbol the compiler actually emits.
func TestGenericInstanceSymbolPredicate(t *testing.T) {
	t.Parallel()

	instantiations := []struct{ symbol, origin, arguments string }{
		// The plain case.
		{"slices.Sort[[]int,int]", "slices.Sort", "[]int,int"},
		// A method on a generic type: the receiver's type arguments land in the
		// same list.
		{"internal/runtime/atomic.Pointer.Load[runtime.g]", "internal/runtime/atomic.Pointer.Load", "runtime.g"},
		// A type argument that is itself bracketed, which is why the scan counts
		// bracket depth instead of stopping at the first `]`.
		{"slices.SortFunc[[]*main.tracked,*main.tracked]", "slices.SortFunc", "[]*main.tracked,*main.tracked"},
		// A type argument that is a function type, which brings parentheses and
		// nested signatures with it.
		{"internal/runtime/atomic.Pointer.Load[func(string) func()]", "internal/runtime/atomic.Pointer.Load", "func(string) func()"},
		// A closure derived from an instantiation. The compiler appends a dotted
		// suffix, and the whole thing is as importer-dependent as its parent.
		{
			"runtime.callCleanup[main.note].gointernal.funcvalue.runtime_callCleanup_main_note.a61023066d5f9d40",
			"runtime.callCleanup",
			"main.note",
		},
	}
	for _, each := range instantiations {
		origin, arguments, ok := genericInstanceOrigin(each.symbol)
		require.Truef(t, ok, "%s should be an instantiation", each.symbol)
		require.Equal(t, each.origin, origin, "origin of %s", each.symbol)
		require.Equal(t, each.arguments, arguments, "type arguments of %s", each.symbol)
		require.True(t, IsGenericInstanceSymbol(each.symbol))
	}

	ordinary := []string{
		"runtime.mallocgc",
		"strconv.Itoa",
		"net/http.(*Transport).RoundTrip",
		// The one that makes this a real predicate rather than strings.Contains:
		// an interface type written out in full inside a symbol, whose method
		// returns a slice. The `[]` is inside braces and is not a type-argument
		// list.
		"errors.interface{Unwrap() []error}.Unwrap",
		"main.f.interfacecall",
		// An unterminated bracket is not a type-argument list either.
		"weird.name[unclosed",
	}
	for _, symbol := range ordinary {
		require.Falsef(t, IsGenericInstanceSymbol(symbol), "%s should not be an instantiation", symbol)
	}
}

// TestGenericInstanceSymbolMatchesTheCompilersOwnAnswer is what keeps the
// predicate honest. [IsGenericInstanceSymbol] reads a name, but the compiler
// does not have to guess: reachableFunctions carries the type arguments an
// instantiation was discovered with, and goc/genericshape.go's census hook
// records the symbol it produced for each. This compiles a real program and
// checks the two agree in both directions.
//
// Sequential, because the census is a package-global sink: a compile started by
// any other test in the process would land in this one's census. See
// goc/sequential_tests.txt.
func TestGenericInstanceSymbolMatchesTheCompilersOwnAnswer(t *testing.T) {
	finish := installGenericCensus()
	module, err := CompileExecutableFor(TargetARM64, "tracked.go", []byte(programCleanupTracked))
	census := finish()
	require.NoError(t, err)
	require.NotEmpty(t, census.Instantiations, "the program produced no instantiation, so this proves nothing")

	recorded := make(map[string]bool, len(census.Instantiations))
	for _, instantiation := range census.Instantiations {
		recorded[instantiation.Symbol] = true
		require.Truef(t, IsGenericInstanceSymbol(instantiation.Symbol),
			"the compiler instantiated %s and the predicate does not recognise it", instantiation.Symbol)
	}

	// The other direction: every symbol the predicate calls an instantiation is
	// one the compiler instantiated, or is derived from one by a dotted suffix.
	var unexplained []string
	present := make(map[string]bool, len(module.Funcs))
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		present[function.Name] = true
		if !IsGenericInstanceSymbol(function.Name) {
			continue
		}
		if recorded[function.Name] {
			continue
		}
		derived := false
		for symbol := range recorded {
			if strings.HasPrefix(function.Name, symbol+".") {
				derived = true
				break
			}
		}
		if !derived {
			unexplained = append(unexplained, function.Name)
		}
	}
	sort.Strings(unexplained)
	require.Emptyf(t, unexplained,
		"%d symbols are read as instantiations but the compiler instantiated nothing they could come from:\n  %s",
		len(unexplained), strings.Join(unexplained, "\n  "))

	// And every instantiation that reached the module is classified as one rather
	// than as something a cache may hold.
	for _, function := range module.Funcs {
		if function.Start == nil || !recorded[function.Name] {
			continue
		}
		require.Equalf(t, CacheUnitInstantiation, ClassifyCacheUnit(function),
			"%s is an instantiation and was classified otherwise", function.Name)
	}
	t.Logf("%d instantiations recorded by the compiler, all recognised; %d module functions, none misread",
		len(census.Instantiations), len(present))
}

// TestFunctionCacheCensusPartitionsEveryLoweredFunction checks the arithmetic
// the measurement rests on: the four categories are disjoint and cover
// everything.
func TestFunctionCacheCensusPartitionsEveryLoweredFunction(t *testing.T) {
	t.Parallel()

	module, err := CompileExecutableFor(TargetARM64, "tracked.go", []byte(programCleanupTracked))
	require.NoError(t, err)

	census := CensusFunctionCache(module, ModuleImportPaths(module))
	require.Equal(t, census.Lowered,
		census.Cacheable+census.Instantiations+census.InterfaceCallWrappers+census.InterfaceMethodDispatchers,
		"the categories do not partition the lowered functions")
	require.Greater(t, census.Lowered, 2000)
	require.Greater(t, census.Instantiations, 0)
	require.Greater(t, census.InterfaceCallWrappers, 0)

	byPackage := 0
	for _, row := range census.ByPackage {
		require.Equal(t, row.Lowered,
			row.Cacheable+row.Instantiations+row.InterfaceCallWrappers+row.InterfaceMethodDispatchers,
			"package %s does not partition", row.Path)
		byPackage += row.Lowered
	}
	require.Equal(t, census.Lowered, byPackage, "the per-package rows do not sum to the whole")
	t.Logf("%d lowered, %d cacheable (%.2f%%), %d instantiations, %d call wrappers, %d dispatchers",
		census.Lowered, census.Cacheable, 100*census.CacheableShare(),
		census.Instantiations, census.InterfaceCallWrappers, census.InterfaceMethodDispatchers)
}

// TestFunctionCacheEntryValidNamesTheClauseThatMoved walks every clause of the
// key, moves it, and requires that Valid both refuses the entry and says which
// clause it was. That is the whole reason the clauses are separate fields: a
// single opaque digest can only ever answer "no".
func TestFunctionCacheEntryValidNamesTheClauseThatMoved(t *testing.T) {
	t.Parallel()

	identity, err := ProgramCompileIdentity(TargetARM64, true, "tracked.go", []byte(programCleanupTracked))
	require.NoError(t, err)
	require.Contains(t, identity.Packages, "runtime")
	require.Contains(t, identity.Packages, "strconv")

	entry, err := NewFunctionCacheEntry("strconv.Itoa", "strconv", identity)
	require.NoError(t, err)
	require.NotEmpty(t, entry.Deps, "strconv imports nothing, so the dependency clause is untested")

	ok, reason := entry.Valid(identity)
	require.Truef(t, ok, "a fresh entry is invalid against the compile that wrote it: %s", reason)

	// Each case copies the identity, moves one clause and expects one reason.
	for _, each := range []struct {
		name   string
		move   func(*CompileIdentity)
		reason string
	}{
		{"target", func(c *CompileIdentity) { c.Target = "amd64" }, "target moved"},
		{"-O", func(c *CompileIdentity) { c.Optimize = !c.Optimize }, "-O moved"},
		{"text layout", func(c *CompileIdentity) { c.TextLayout = "elsewhere" }, "text layout policy moved"},
		{"pipeline", func(c *CompileIdentity) { c.Pipeline = "other" }, "optimiser pipeline moved"},
		{"compiler", func(c *CompileIdentity) { c.Compiler = digestOf("a different binary") }, "compiler binary moved"},
		{
			"own source",
			func(c *CompileIdentity) {
				owner := c.Packages["strconv"]
				owner.Source = digestOf("edited")
				c.Packages["strconv"] = owner
			},
			"package strconv source moved",
		},
		{
			"a dependency's source",
			func(c *CompileIdentity) {
				// Moving the transitive identity of something strconv imports is
				// BUILD_CACHE.md §3.2 clause 4 in action: the clause that has to
				// notice a change in a package this one was compiled against.
				dependency := c.Packages[entry.Deps[0].Path]
				dependency.Transitive = digestOf("edited")
				c.Packages[entry.Deps[0].Path] = dependency
			},
			"dependency " + entry.Deps[0].Path + " moved",
		},
		{
			"a dependency leaving",
			func(c *CompileIdentity) { delete(c.Packages, entry.Deps[0].Path) },
			"dependency " + entry.Deps[0].Path + " left the compile",
		},
		{
			"the package leaving",
			func(c *CompileIdentity) { delete(c.Packages, "strconv") },
			"package strconv is not loaded",
		},
		{
			"the import set",
			func(c *CompileIdentity) {
				owner := c.Packages["strconv"]
				owner.Imports = owner.Imports[:len(owner.Imports)-1]
				c.Packages["strconv"] = owner
			},
			"package strconv changed its import set",
		},
	} {
		moved := copyCompileIdentity(identity)
		each.move(moved)
		ok, reason := entry.Valid(moved)
		require.Falsef(t, ok, "moving %s did not invalidate the entry", each.name)
		require.Equalf(t, each.reason, reason, "moving %s reported the wrong clause", each.name)
	}

	// The format version is on the entry rather than on the compile, so it moves
	// the other way round.
	stale := *entry
	stale.UnitVersion = FunctionCacheUnitVersion + 1
	ok, reason = stale.Valid(identity)
	require.False(t, ok)
	require.Equal(t, "unit format version moved", reason)
}

// TestPackageIdentityIsTransitive is the clause that makes the scheme sound
// under cross-package inlining, checked as a property rather than as a digest: a
// package's transitive identity has to move when anything it can reach moves,
// and the recursion has to bottom out.
func TestPackageIdentityIsTransitive(t *testing.T) {
	t.Parallel()

	identity, err := ProgramCompileIdentity(TargetARM64, true, "tracked.go", []byte(programCleanupTracked))
	require.NoError(t, err)

	// Every package's imports have identities of their own, or the recursion
	// would have folded a zero into somebody's key without saying so.
	for path, each := range identity.Packages {
		for _, imported := range each.Imports {
			require.Containsf(t, identity.Packages, imported, "%s imports %s, which has no identity", path, imported)
		}
		require.NotEqual(t, CacheDigest{}, each.Transitive, "%s has an empty transitive identity", path)
	}

	// Moving one leaf moves everything that can reach it and nothing else.
	moved := copyCompileIdentity(identity)
	leaf := moved.Packages["internal/runtime/atomic"]
	require.NotEmpty(t, leaf.Path, "internal/runtime/atomic is not in the closure")
	leaf.Source = digestOf("edited")
	moved.Packages["internal/runtime/atomic"] = leaf
	require.NoError(t, closeTransitiveIdentities(moved.Packages))

	reaches := func(from string) bool {
		seen := map[string]bool{}
		var walk func(string) bool
		walk = func(path string) bool {
			if path == "internal/runtime/atomic" {
				return true
			}
			if seen[path] {
				return false
			}
			seen[path] = true
			for _, imported := range identity.Packages[path].Imports {
				if walk(imported) {
					return true
				}
			}
			return false
		}
		return walk(from)
	}
	changed, unchanged := 0, 0
	for path, before := range identity.Packages {
		after := moved.Packages[path]
		if reaches(path) {
			require.NotEqualf(t, before.Transitive, after.Transitive,
				"%s can reach the edited package and its transitive identity did not move", path)
			changed++
			continue
		}
		require.Equalf(t, before.Transitive, after.Transitive,
			"%s cannot reach the edited package and its transitive identity moved anyway", path)
		unchanged++
	}
	require.Greater(t, changed, 1, "the edit reached nothing but itself")
	require.Greater(t, unchanged, 0, "the edit reached everything, so the clause is not selective")
	t.Logf("editing internal/runtime/atomic moves %d of %d package identities", changed, changed+unchanged)
}

func copyCompileIdentity(from *CompileIdentity) *CompileIdentity {
	copied := *from
	copied.Packages = make(map[string]PackageIdentity, len(from.Packages))
	for path, identity := range from.Packages {
		identity.Imports = append([]string(nil), identity.Imports...)
		copied.Packages[path] = identity
	}
	return &copied
}
