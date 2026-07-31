package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/prebuilt"
	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pack may carry standard library packages beyond the Go runtime, and then it
// is usable only by a program that loads all of them: the pack leaves its type
// region, its interface dispatchers and its degraded itabs for the program
// module, and a program that never loaded net/http cannot generate net/http's.
//
// So a build holds several packs and each program takes the richest it can. These
// tests drive that end to end. They use a pack rooted at `fmt` rather than at
// net/http because the mechanism is the same and the pack costs 9 s instead of
// 157 s; the capability matrix builds the expensive ones.

var (
	fmtPackOnce sync.Once
	fmtPackData *runtimepack.Pack
	fmtPackErr  error
)

func sharedFmtPack(t *testing.T) *runtimepack.Pack {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("linux/arm64 Go runtime image")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc is required to assemble the Go runtime's Plan 9 sidecar")
	}
	fmtPackOnce.Do(func() {
		fmtPackData, fmtPackErr = prebuilt.BuildRuntime(goc.TargetARM64, prebuilt.Options{Packages: []string{"fmt"}})
	})
	require.NoError(t, fmtPackErr)
	return fmtPackData
}

// writePacks writes the packs to a directory and returns the value goc's
// -runtime flag takes.
func writePacks(t *testing.T, directory string, packs ...*runtimepack.Pack) []string {
	t.Helper()
	paths := make([]string, 0, len(packs))
	for index, pack := range packs {
		path := filepath.Join(directory, "runtime"+string(rune('0'+index))+".gocrt")
		require.NoError(t, pack.Write(path))
		paths = append(paths, path)
	}
	return paths
}

// linkAgainstPacksForTest reads the pack set from paths and compiles one program
// against it, which is exactly what the command line does.
func linkAgainstPacksForTest(t *testing.T, paths []string, source string, contents []byte, executable string, optimize bool) error {
	t.Helper()
	packs, err := readPackSet(paths)
	require.NoError(t, err)
	return linkAgainstPrebuiltRuntime(goc.TargetARM64, packs, source, contents, executable, optimize, os.Stderr)
}

// buildAndRunAgainstPacks compiles source against the packs, links it and runs
// it, returning its combined output.
func buildAndRunAgainstPacks(t *testing.T, source string, packs ...*runtimepack.Pack) string {
	t.Helper()
	work := t.TempDir()
	paths := writePacks(t, work, packs...)
	program := filepath.Join(work, "program.go")
	require.NoError(t, os.WriteFile(program, []byte(source), 0o644))
	executable := filepath.Join(work, "program")

	require.NoError(t, linkAgainstPacksForTest(t, paths, program, []byte(source), executable, false))

	output, err := exec.Command(executable).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return string(output)
}

// packStdlibProgram uses fmt, so the fmt pack is usable by it, and it reports
// enough to tell whether each half of the image initialized exactly once.
const packStdlibProgram = `package main

import "fmt"

var initialized []string

func init() { initialized = append(initialized, "main") }

type shape interface{ area() int }

type square struct{ side int }

func (s square) area() int { return s.side * s.side }

func (s square) String() string { return fmt.Sprintf("square(%d)", s.side) }

func main() {
	fmt.Println("inits", len(initialized), initialized)
	var value shape = square{side: 4}
	fmt.Println("area", value.area())
	var stringer fmt.Stringer = square{side: 5}
	fmt.Println("stringer", stringer)
	if _, ok := value.(fmt.Stringer); ok {
		fmt.Println("also a Stringer")
	}
}
`

// packRuntimeOnlyProgram loads nothing beyond the runtime, so a pack carrying fmt
// is not usable by it.
const packRuntimeOnlyProgram = `package main

var initialized int

func init() { initialized = 7 }

func main() {
	println("init")
	println(initialized)
}
`

func TestAProgramTakesTheRichestPackItCanUse(t *testing.T) {
	plain := sharedPrebuiltRuntime(t)
	rich := sharedFmtPack(t)

	output := buildAndRunAgainstPacks(t, packStdlibProgram, plain, rich)

	// One init entry, not two: the program's own package init has to run exactly
	// once, and it is the module chain's tail that decides that.
	assert.Equal(t, "inits 1 [main]\narea 16\nstringer square(5)\nalso a Stringer\n", output)
}

// TestAPackCarryingMoreThanTheProgramLoadsIsNotUsed is the safety property the
// whole arrangement rests on. Offering a program a pack whose closure it does not
// contain must not produce an image; it must fall back to one it can use.
func TestAPackCarryingMoreThanTheProgramLoadsIsNotUsed(t *testing.T) {
	plain := sharedPrebuiltRuntime(t)
	rich := sharedFmtPack(t)

	output := buildAndRunAgainstPacks(t, packRuntimeOnlyProgram, plain, rich)

	assert.Equal(t, "init\n7\n", output)
}

// TestOnlyTheRichPackLeavesAProgramWithNothingToFallBackOn checks the failure is
// reported rather than mislinked when no offered pack is usable.
func TestOnlyTheRichPackLeavesAProgramWithNothingToFallBackOn(t *testing.T) {
	rich := sharedFmtPack(t)
	work := t.TempDir()
	paths := writePacks(t, work, rich)
	program := filepath.Join(work, "program.go")
	require.NoError(t, os.WriteFile(program, []byte(packRuntimeOnlyProgram), 0o644))

	err := linkAgainstPacksForTest(t, paths, program, []byte(packRuntimeOnlyProgram), filepath.Join(work, "program"), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "usable")
}

// TestAPackRecordsTheClosureItWasBuiltFrom keeps the manifest honest: the
// applicability test reads Closure, so a pack that under-reports it would be
// handed to a program that cannot supply what it left behind.
func TestAPackRecordsTheClosureItWasBuiltFrom(t *testing.T) {
	plain := sharedPrebuiltRuntime(t)
	rich := sharedFmtPack(t)

	assert.Equal(t, []string{"fmt"}, rich.Manifest.Packages)
	assert.Contains(t, rich.Manifest.Closure, "fmt")
	assert.Contains(t, rich.Manifest.Closure, "runtime")
	assert.NotContains(t, rich.Manifest.Closure, "main",
		"the root's own package is not part of what a program must load")
	assert.Empty(t, plain.Manifest.Packages)
	assert.NotContains(t, plain.Manifest.Closure, "fmt")

	// Every executable compiles the whole runtime closure, so the plain pack is
	// usable by every program and the rich one is a strict superset of it.
	for _, path := range plain.Manifest.Closure {
		assert.Contains(t, rich.Manifest.Closure, path)
	}
}

// TestAPackCacheKeyCoversItsInputs checks the key moves when any input does,
// because a stale hit is a wrong image rather than a slow build.
func TestAPackCacheKeyCoversItsInputs(t *testing.T) {
	stdlib := goc.StdlibRoot()
	base, err := packCacheKey(runtimepack.Version, "arm64", false, nil, stdlib)
	require.NoError(t, err)

	same, err := packCacheKey(runtimepack.Version, "arm64", false, nil, stdlib)
	require.NoError(t, err)
	assert.Equal(t, base, same, "the same inputs have to give the same key")

	for name, key := range map[string]func() (string, error){
		"the pack format version": func() (string, error) {
			return packCacheKey(runtimepack.Version+1, "arm64", false, nil, stdlib)
		},
		"the target": func() (string, error) {
			return packCacheKey(runtimepack.Version, "amd64", false, nil, stdlib)
		},
		"optimization": func() (string, error) {
			return packCacheKey(runtimepack.Version, "arm64", true, nil, stdlib)
		},
		"the carried packages": func() (string, error) {
			return packCacheKey(runtimepack.Version, "arm64", false, []string{"fmt"}, stdlib)
		},
	} {
		other, err := key()
		require.NoError(t, err, name)
		assert.NotEqual(t, base, other, "the key has to move when %s does", name)
	}

	// The package list is a set: order must not change the key, or the same pack
	// would be built twice.
	one, err := packCacheKey(runtimepack.Version, "arm64", false, []string{"fmt", "net/http"}, stdlib)
	require.NoError(t, err)
	other, err := packCacheKey(runtimepack.Version, "arm64", false, []string{"net/http", "fmt"}, stdlib)
	require.NoError(t, err)
	assert.Equal(t, one, other)

	// A change anywhere in the standard library tree has to move it.
	scratch := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "a.go"), []byte("package a\n"), 0o644))
	before, err := packCacheKey(runtimepack.Version, "arm64", false, nil, scratch)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "a.go"), []byte("package b\n"), 0o644))
	after, err := packCacheKey(runtimepack.Version, "arm64", false, nil, scratch)
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
}

func TestThePackCacheIsOffWhenTheBuildAsksForNoCache(t *testing.T) {
	t.Setenv("CG12_NOCACHE", "1")
	assert.Empty(t, packCacheDirectory())

	t.Setenv("CG12_NOCACHE", "")
	t.Setenv("CG12_PACK_CACHE", filepath.Join(t.TempDir(), "packs"))
	assert.NotEmpty(t, packCacheDirectory())
	assert.True(t, strings.HasSuffix(packCacheDirectory(), "packs"))
}
