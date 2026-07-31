package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about the one place where batch compilation and multi-pack
// selection meet.
//
// A batch worker exists to do once what a one-shot compile does every time, and
// the prebuilt runtimes are the largest thing it could hoist. It may not hoist
// the *choice*: which pack a program gets is decided by what that program
// loaded, and is not known until the front end has run (RUNTIME_PLAN 19). So a
// worker holds every candidate's manifest, which is all the choice needs, and
// reads a pack's objects the first time one of its programs picks that pack.
//
// The risk that creates did not exist on either branch alone: a process that has
// read one pack now compiles a program that wants a different one. The tests
// below pin both halves -- that the set reads what it should and no more, and
// that a worker fed programs choosing different packs, interleaved, produces the
// same executables as compiling each alone.

// packUserOfFmt loads fmt, so a pack rooted at fmt is usable by it.
const packUserOfFmt = `package main

import "fmt"

func main() { fmt.Println("fmt user", 41+1) }
`

// packSecondUserOfFmt is a different program with the same closure, so it
// chooses the same pack and must share the read of it.
const packSecondUserOfFmt = `package main

import "fmt"

type point struct{ x, y int }

func (p point) String() string { return fmt.Sprintf("(%d,%d)", p.x, p.y) }

func main() { fmt.Println("second fmt user", point{x: 3, y: 4}) }
`

// packUserOfNothing loads nothing beyond the runtime, so the fmt pack is not
// usable by it and it must fall back to the runtime-only pack.
const packUserOfNothing = `package main

func main() { println("runtime only", 7) }
`

// packSecondUserOfNothing is a second such program, so the batch has two of each
// kind to interleave.
const packSecondUserOfNothing = `package main

func total(values []int) int {
	sum := 0
	for _, value := range values {
		sum += value
	}
	return sum
}

func main() { println("second runtime only", total([]int{1, 2, 3})) }
`

// TestAPackSetReadsOnlyTheChosenPackAndReadsItOnce is the direct test of the
// reconciliation.
//
// It asserts three things in order, and each one is a way the two designs could
// have been merged wrongly:
//
//   - building the set reads no pack's objects, so multi-pack selection is not
//     paying to read packs it will not use -- the property the hoisted read on
//     `ccwork/goc-batch-b` would have destroyed;
//   - a program's compile reads exactly the pack that program chose, and the
//     other pack is still unread -- so the choice is still the program's;
//   - a second program choosing the same pack gets the same objects in memory
//     rather than a second read -- which is the saving the batch exists for.
func TestAPackSetReadsOnlyTheChosenPackAndReadsItOnce(t *testing.T) {
	plain := sharedPrebuiltRuntime(t)
	rich := sharedFmtPack(t)
	work := t.TempDir()
	paths := writePacks(t, work, plain, rich)
	plainPath, richPath := paths[0], paths[1]

	set, err := readPackSet(paths)
	require.NoError(t, err)
	require.Len(t, set.manifests, 2)
	assert.Empty(t, loadedPackPaths(set),
		"reading a pack set must read manifests only; a pack carrying net/http is tens of megabytes")

	compileAgainstPackSet(t, set, work, "first_fmt", packUserOfFmt)
	assert.Equal(t, []string{richPath}, loadedPackPaths(set),
		"a program that loads fmt takes the fmt pack, and nothing should have read the other one")

	compileAgainstPackSet(t, set, work, "first_plain", packUserOfNothing)
	assert.Equal(t, []string{plainPath, richPath}, loadedPackPaths(set),
		"a program that loads nothing beyond the runtime falls back to the runtime-only pack")

	alreadyRead := set.packs[richPath]
	compileAgainstPackSet(t, set, work, "second_fmt", packSecondUserOfFmt)
	assert.Same(t, alreadyRead, set.packs[richPath],
		"a second program choosing the same pack must share the first program's read")
	assert.Len(t, loadedPackPaths(set), 2, "no third pack exists to be read")
}

// TestBatchCompilesAgainstDifferentPacksMatchOneShotCompiles is the safety
// property re-established for the case the batch branch never had.
//
// `ccwork/goc-batch-b` proved that a worker's history cannot change what it
// compiles, with every program in one worker compiled against one pack. With a
// pack set, a worker's history now includes *which packs it has read*, and a
// program that chooses a pack an earlier program in the same worker already read
// is a state the old test could not reach.
//
// So the programs are interleaved on purpose: fmt user, runtime-only user, fmt
// user, runtime-only user. Every worker ordering therefore contains a pack
// change, and the reversed batch contains the opposite one.
func TestBatchCompilesAgainstDifferentPacksMatchOneShotCompiles(t *testing.T) {
	compiler, packs, plainOnly, directory := batchMultiPackEnvironment(t)

	sources := writeSourcesForTest(t, directory, []struct{ name, source string }{
		{"batch_pack_fmt_first.go", packUserOfFmt},
		{"batch_pack_plain_first.go", packUserOfNothing},
		{"batch_pack_fmt_second.go", packSecondUserOfFmt},
		{"batch_pack_plain_second.go", packSecondUserOfNothing},
	})

	// The premise of the test: these programs do not all choose the same pack.
	// Without this the interleaving would prove nothing, because a set whose
	// selection had silently collapsed to one pack would still pass everything
	// below.
	requireProgramsChooseDifferentPacks(t, compiler, packs, plainOnly, directory, sources[0], sources[1])

	firstAlone := compileEachAloneForTest(t, compiler, packs, directory, ".alone1", sources)
	secondAlone := compileEachAloneForTest(t, compiler, packs, directory, ".alone2", sources)
	forward := compileBatchForTest(t, compiler, packs, directory, ".forward", sources)
	reversed := compileBatchForTest(t, compiler, packs, directory, ".reversed", reverseStrings(sources))

	compared := 0
	for _, source := range sources {
		name := filepath.Base(source)
		if !bytes.Equal(firstAlone[name], secondAlone[name]) {
			t.Logf("%s does not compile deterministically on its own, so its bytes cannot be compared", name)
			continue
		}
		compared++
		require.Equal(t, firstAlone[name], forward[name],
			"%s compiled in an interleaved batch differs from %s compiled alone", name, name)
		require.Equal(t, firstAlone[name], reversed[name],
			"%s compiled in a reversed interleaved batch differs from %s compiled alone", name, name)
	}
	require.GreaterOrEqual(t, compared, 2,
		"too few programs compiled deterministically alone for this test to mean anything")

	// The executables are also run, because bytes cannot speak for a program
	// whose compile is not deterministic and this corpus has some.
	for _, source := range sources {
		name := filepath.Base(source)
		alone := runBuiltProgram(t, filepath.Join(directory, name+".alone1"))
		assert.Equal(t, alone, runBuiltProgram(t, filepath.Join(directory, name+".forward")),
			"%s behaves differently when compiled in an interleaved batch", name)
		assert.Equal(t, alone, runBuiltProgram(t, filepath.Join(directory, name+".reversed")),
			"%s behaves differently when compiled in a reversed interleaved batch", name)
	}
}

// batchMultiPackEnvironment writes a runtime-only pack and a pack rooted at fmt,
// and returns the compiler, goc's -runtime value naming both, the -runtime value
// naming the runtime-only one alone, and a scratch directory.
//
// The rich pack is rooted at fmt rather than at net/http because the mechanism
// is identical and the pack costs 9 s instead of 157 s, which is the same trade
// the pack tests in packstdlib_test.go make.
func batchMultiPackEnvironment(t *testing.T) (compiler, packs, plainOnly, directory string) {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("the batch compiler shares the Go runtime's source world, which only arm64 compiles")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc is required to link")
	}

	directory = t.TempDir()
	paths := writePacks(t, directory, sharedPrebuiltRuntime(t), sharedFmtPack(t))
	return sharedGOCBinary(t), paths[0] + "," + paths[1], paths[0], directory
}

// requireProgramsChooseDifferentPacks establishes that one program takes the
// rich pack and the other cannot.
//
// It is checked through the compiler rather than asserted from the sources,
// because "which pack did this program get" is a property of the front end's
// closure, and a test that assumed it would keep passing if the selection
// stopped working.
func requireProgramsChooseDifferentPacks(t *testing.T, compiler, packs, plainOnly, directory, richUser, plainUser string) {
	t.Helper()

	againstBoth := filepath.Join(directory, "premise-both.bin")
	againstPlain := filepath.Join(directory, "premise-plain.bin")
	output, err := exec.Command(compiler, "-runtime", packs, "-o", againstBoth, richUser).CombinedOutput()
	require.NoError(t, err, "%s", output)
	output, err = exec.Command(compiler, "-runtime", plainOnly, "-o", againstPlain, richUser).CombinedOutput()
	require.NoError(t, err, "%s", output)

	both, err := os.ReadFile(againstBoth)
	require.NoError(t, err)
	plainly, err := os.ReadFile(againstPlain)
	require.NoError(t, err)
	require.NotEqual(t, plainly, both,
		"%s built against both packs is the same image as one built against the runtime-only pack, so it did not take the fmt pack",
		filepath.Base(richUser))

	// And the other program cannot take the fmt pack at all: offered that one
	// alone, it has nothing to fall back on and the build is refused.
	richOnly := filepath.Join(directory, "premise-rich-only.gocrt")
	require.NoError(t, sharedFmtPack(t).Write(richOnly))
	output, err = exec.Command(compiler, "-runtime", richOnly, "-o", filepath.Join(directory, "premise-refused.bin"), plainUser).CombinedOutput()
	require.Error(t, err, "%s", output)
	require.Contains(t, string(output), "usable")
}

// writeSourcesForTest writes the programs in the order given and returns their
// paths, so the batch order and its reverse are both meaningful.
func writeSourcesForTest(t *testing.T, directory string, programs []struct{ name, source string }) []string {
	t.Helper()
	sources := make([]string, 0, len(programs))
	for _, program := range programs {
		path := filepath.Join(directory, program.name)
		require.NoError(t, os.WriteFile(path, []byte(program.source), 0o644))
		sources = append(sources, path)
	}
	return sources
}

// compileAgainstPackSet compiles one program through the same call the command
// line makes, so the set is exercised the way goc exercises it.
func compileAgainstPackSet(t *testing.T, set *packSet, directory, name, source string) {
	t.Helper()
	program := filepath.Join(directory, name+".go")
	require.NoError(t, os.WriteFile(program, []byte(source), 0o644))
	require.NoError(t, linkAgainstPrebuiltRuntime(
		goc.TargetARM64, set, program, []byte(source), filepath.Join(directory, name+".bin"), false, os.Stderr))
}

// loadedPackPaths is which packs the set has read the objects of, sorted.
func loadedPackPaths(set *packSet) []string {
	set.mutex.Lock()
	defer set.mutex.Unlock()
	paths := make([]string, 0, len(set.packs))
	for path := range set.packs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// runBuiltProgram runs an executable and returns its output and exit status as
// one comparable string.
func runBuiltProgram(t *testing.T, path string) string {
	t.Helper()
	output, err := exec.Command(path).CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", path, err)
	}
	return fmt.Sprintf("rc=%d\n%s", code, output)
}
