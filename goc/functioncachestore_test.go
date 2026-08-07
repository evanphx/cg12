package goc

import (
	"os"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// This file is the correctness property of the whole design, in the smallest form
// that can hold it: one process, one program, compiled twice against the same
// cache directory, with the second compile forbidden to lower anything the first
// one stored. If the two modules are not the same bytes, nothing else about the
// cache matters.
//
// The cross-process form of the same question -- a cold `goc` and a warm `goc`
// producing the same executable -- is scripts/function-cache-check.sh, because
// that is the one a compiler user actually runs and because a same-process
// comparison cannot see a key that accidentally depends on process state.

// compileWithCache compiles a program with the function cache pointed at
// directory, and returns the module and what the cache did.
func compileWithCache(t *testing.T, directory, name, source string) (*ir.Module, FunctionCacheStats) {
	t.Helper()
	t.Setenv("CG12_FUNC_CACHE", directory)
	t.Setenv("CG12_NOCACHE", "")
	module, err := CompileExecutableFor(TargetARM64, name, []byte(source))
	require.NoError(t, err)
	return module, LastFunctionCacheStats()
}

// moduleBytes is the whole module in its lossless on-disk form, which is the
// comparison that catches everything the IR carries -- a datum's relocation base,
// an aggregate parameter's type, a call convention, a source position -- rather
// than only what the printer shows.
func moduleBytes(t *testing.T, module *ir.Module) []byte {
	t.Helper()
	encoded, err := module.MarshalBinary()
	require.NoError(t, err)
	return encoded
}

// TestWarmCompileIsByteIdenticalToCold is the deliverable.
func TestWarmCompileIsByteIdenticalToCold(t *testing.T) {
	directory := t.TempDir()

	cold, coldStats := compileWithCache(t, directory, "cachecold.go", programCacheSmall)
	t.Logf("cold: %s", coldStats)
	require.Equal(t, 0, coldStats.PackagesHit, "a compile against an empty directory hit something")
	require.Greater(t, coldStats.Wrote, 10, "the cold compile stored nothing")
	coldBytes := moduleBytes(t, cold)

	warm, warmStats := compileWithCache(t, directory, "cachecold.go", programCacheSmall)
	t.Logf("cold: %s", coldStats)
	t.Logf("warm: %s", warmStats)
	require.Greater(t, warmStats.PackagesHit, 10, "the warm compile hit no package")
	require.Greater(t, warmStats.Hits, 1000, "the warm compile replayed almost nothing")

	warmBytes := moduleBytes(t, warm)
	require.Equal(t, len(coldBytes), len(warmBytes), "the warm module is a different size from the cold one")
	require.True(t, string(coldBytes) == string(warmBytes), "the warm module is not byte-identical to the cold one")

	// And the same again through the optimiser and the backend, which is where a
	// difference that the IR comparison tolerated would actually reach the user.
	opt.OptimizeModule(cold)
	opt.OptimizeModule(warm)
	coldObject, coldAssembly, err := arm64.CompileToObjectAndAssembly(cold)
	require.NoError(t, err)
	warmObject, warmAssembly, err := arm64.CompileToObjectAndAssembly(warm)
	require.NoError(t, err)
	require.Equal(t, coldAssembly, warmAssembly, "the assembled sidecar differs")
	coldELF, err := coldObject.MarshalELF()
	require.NoError(t, err)
	warmELF, err := warmObject.MarshalELF()
	require.NoError(t, err)
	require.True(t, string(coldELF) == string(warmELF), "the warm object is not byte-identical to the cold one")
	t.Logf("%d bytes of object, identical", len(coldELF))
}

// TestNoCacheBypassesTheStore holds the switch the merge gates depend on: with
// CG12_NOCACHE=1 the compile reads nothing and writes nothing, whatever else is
// set.
func TestNoCacheBypassesTheStore(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CG12_FUNC_CACHE", directory)
	t.Setenv("CG12_NOCACHE", "1")

	_, err := CompileExecutableFor(TargetARM64, "nocache.go", []byte(programCacheSmall))
	require.NoError(t, err)
	stats := LastFunctionCacheStats()
	require.Equal(t, 0, stats.Packages, "CG12_NOCACHE=1 still consulted the cache")
	require.Equal(t, 0, stats.Wrote, "CG12_NOCACHE=1 still wrote to the cache")

	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Empty(t, entries, "CG12_NOCACHE=1 left files in the cache directory")
	require.False(t, FunctionCacheEnabled())
}

// TestCacheFilledByAnotherProgramIsUsable is the cross-program property, and it is
// the one the key is really for. A same-program cold/warm pair proves the store
// and the merge round-trip; it does not prove that a unit written while compiling
// one program is the right unit for another. The two programs below share
// runtime, slices and strconv and make them carry disjoint generic instantiations
// over locally declared types -- the pair goc/functionlowering_test.go is built
// around -- so the second compile takes its shared packages entirely from units
// the first one wrote.
func TestCacheFilledByAnotherProgramIsUsable(t *testing.T) {
	shared := t.TempDir()

	// Program B compiled cold, with its own private cache, is the control.
	control, _ := compileWithCache(t, t.TempDir(), "payload.go", programCleanupPayload)
	controlBytes := moduleBytes(t, control)

	// Program A fills the shared directory. Program B then compiles against it.
	_, fillStats := compileWithCache(t, shared, "tracked.go", programCleanupTracked)
	require.Greater(t, fillStats.Wrote, 10)
	subject, subjectStats := compileWithCache(t, shared, "payload.go", programCleanupPayload)
	t.Logf("filled by another program: %s", subjectStats)
	require.Greater(t, subjectStats.Hits, 1000,
		"the second program reused almost nothing the first one stored")

	require.True(t, string(controlBytes) == string(moduleBytes(t, subject)),
		"a program compiled from another program's cache differs from one compiled cold")
}

// TestChangedDependencyInvalidatesTheUnit checks the clause that makes the scheme
// sound under cross-package inlining: a package whose *dependency* moved must not
// serve its old unit, even though its own source is untouched.
func TestChangedDependencyInvalidatesTheUnit(t *testing.T) {
	identity, err := ProgramCompileIdentity(TargetARM64, false, "ident.go", []byte(programCacheSmall))
	require.NoError(t, err)

	entry, err := NewFunctionCacheEntry("strconv.Itoa", "strconv", identity)
	require.NoError(t, err)
	valid, reason := entry.Valid(identity)
	require.True(t, valid, "a fresh entry is invalid against the identity that made it: %s", reason)
	before := packageCacheKeyDigest(entry)

	// Move one of strconv's dependencies and nothing else.
	require.NotEmpty(t, entry.Deps)
	moved := *identity
	moved.Packages = make(map[string]PackageIdentity, len(identity.Packages))
	for path, each := range identity.Packages {
		moved.Packages[path] = each
	}
	dependency := moved.Packages[entry.Deps[0].Path]
	dependency.Transitive[0] ^= 0xff
	moved.Packages[entry.Deps[0].Path] = dependency

	valid, reason = entry.Valid(&moved)
	require.False(t, valid, "a moved dependency did not invalidate the entry")
	require.Contains(t, reason, entry.Deps[0].Path)

	// And the key digest moves with it, so the two never share a file.
	after, err := NewFunctionCacheEntry("strconv.Itoa", "strconv", &moved)
	require.NoError(t, err)
	require.NotEqual(t, before, packageCacheKeyDigest(after))
}

// TestPackageUnitRoundTrip holds the file format to its own promises: what goes in
// comes out, and a file that is not the file it says it is fails rather than
// decoding into something plausible.
func TestPackageUnitRoundTrip(t *testing.T) {
	t.Parallel()

	entry := &FunctionCacheEntry{
		Name: "", Package: "strconv", UnitVersion: FunctionCacheUnitVersion,
		Source: digestOf("source"), Target: "arm64", Optimize: false,
		TextLayout: "layout", Pipeline: "pipeline", Compiler: digestOf("compiler"),
		Deps: []PackageDependency{{Path: "errors", Transitive: digestOf("errors")}},
	}
	unit := newPackageCacheUnit(entry)
	unit.add(&cachedDeclaration{
		Symbol:   "strconv.Itoa",
		NewFiles: []string{"strconv/itoa.go"},
		Unit:     []byte{1, 2, 3, 4},
		Interns:  []internNote{{Kind: internTypeTag, Key: "int", Value: ".goc.runtime.type.aa"}},
	})
	encoded := unit.encode()

	decoded, err := decodePackageCacheUnit(encoded)
	require.NoError(t, err)
	require.Equal(t, entry, decoded.Entry)
	require.Equal(t, unit.order, decoded.order)
	require.Equal(t, unit.Decls["strconv.Itoa"], decoded.Decls["strconv.Itoa"])
	require.Equal(t, packageCacheKeyDigest(entry), packageCacheKeyDigest(decoded.Entry))

	// The encoding is deterministic, which is what lets one key name one file.
	require.Equal(t, encoded, unit.encode())

	// A flipped byte in the payload is refused, not decoded.
	corrupt := append([]byte(nil), encoded...)
	corrupt[len(corrupt)-1] ^= 0x01
	_, err = decodePackageCacheUnit(corrupt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "digest")

	// So is a truncation.
	_, err = decodePackageCacheUnit(encoded[:len(encoded)-3])
	require.Error(t, err)
}

// programCacheSmall is a program with enough shape to exercise the merge: string
// literals (gen.literalData), interface values and a method call
// (gen.interfaceItabs and the call wrappers), a closure, a map and a slice.
const programCacheSmall = `package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type boxed struct{ n int }

func (b boxed) Error() string  { return "boxed " + strconv.Itoa(b.n) }
func (b boxed) String() string { return "boxed" }

func main() {
	var err error = boxed{3}
	fmt.Println(err, errors.Unwrap(err), strings.ToUpper("x"))
	counts := map[string]int{"a": 1, "b": 2}
	total := 0
	add := func(n int) { total += n }
	for _, name := range []string{"a", "b"} {
		add(counts[name])
	}
	fmt.Println(total, strconv.Quote("hi"))
}
`
