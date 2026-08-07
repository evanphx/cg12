package goc

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file is the instrument, not a scenario.
//
// Every cross-program defect this cache has had is the same shape: a
// whole-program fact recorded inside a per-declaration delta. The first one cost
// 357 of 408 corpus programs their link (an interned artifact's definition
// belonged to whichever declaration minted it first); the second produced the
// wrong itabs in 89 of 406 (materialiseInterfaceImplementations walking the
// program's implementation set); the third was one datum in 42130 whose Equal
// field was a function pointer cold and nil warm (internTypeEqualTarget).
//
// Each was found by a scenario that happened to expose it, and each took a
// whole-program comparison to localise. The property underneath them all can be
// checked directly, and does not need a program that exposes it:
//
//	A stored declaration's delta is a function of its package, not of the
//	program that was being compiled when it was stored.
//
// The check follows from the key. A unit's file name is a content address of
// every clause of its key -- package source, transitive dependency identities,
// target, layout, pipeline, compiler -- so two programs that write the same file
// name agreed on all of it. If those two files then disagree about what one
// declaration contributed, the disagreement came from the program, and the cache
// is one unlucky compile away from serving one program's fact to another.
//
// This is a delta comparison rather than an image comparison on purpose. An image
// comparison can only see a leak that the two programs happen to make visible;
// this sees the leak itself, at the declaration it is recorded in, whether or not
// any program yet turns it into a wrong binary. Both of the leaks fixed alongside
// this test -- the file table and the pointer key -- were of that kind: latent,
// with no program in the corpus turning either into a different image.

// cachedUnitFiles is every unit file in a cache directory, by its key digest.
func cachedUnitFiles(t *testing.T, directory string) map[string]string {
	t.Helper()
	files := map[string]string{}
	fanouts, err := os.ReadDir(directory)
	require.NoError(t, err)
	for _, fanout := range fanouts {
		if !fanout.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(directory, fanout.Name()))
		require.NoError(t, err)
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".gocfn") {
				continue
			}
			files[strings.TrimSuffix(name, ".gocfn")] = filepath.Join(directory, fanout.Name(), name)
		}
	}
	return files
}

func readUnitFile(t *testing.T, path string) *packageCacheUnit {
	t.Helper()
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	unit, err := decodePackageCacheUnit(contents)
	require.NoError(t, err)
	return unit
}

// sequenceDifference describes the first way two recordings of one delta differ,
// in the terms the store is written in, or "" when they are the same delta.
//
// The components are separated because they fail for different reasons and a
// bare "these bytes differ" would send a reader to the wrong place: Unit is the
// IR the declaration appended, Refs is where it reached an interned artifact,
// Interns is what it put in the generator's tables.
func sequenceDifference(left, right cachedSequence) string {
	if string(left.Unit) != string(right.Unit) {
		return fmt.Sprintf("the encoded IR differs (%d bytes against %d)", len(left.Unit), len(right.Unit))
	}
	if len(left.Refs) != len(right.Refs) {
		return fmt.Sprintf("%d artifact references against %d", len(left.Refs), len(right.Refs))
	}
	for index := range left.Refs {
		if left.Refs[index] != right.Refs[index] {
			return fmt.Sprintf("artifact reference %d is %+v against %+v", index, left.Refs[index], right.Refs[index])
		}
	}
	if len(left.Interns) != len(right.Interns) {
		return fmt.Sprintf("%d intern notes against %d:\n%s", len(left.Interns), len(right.Interns),
			internNoteDifference(left.Interns, right.Interns))
	}
	for index := range left.Interns {
		if left.Interns[index] != right.Interns[index] {
			return fmt.Sprintf("intern note %d is %+v against %+v", index, left.Interns[index], right.Interns[index])
		}
	}
	return ""
}

// internNoteDifference names the notes one recording has and the other does not,
// which is what a leak in a table entry looks like from here.
func internNoteDifference(left, right []internNote) string {
	spell := func(note internNote) string {
		return fmt.Sprintf("kind=%d key=%q value=%q", note.Kind, note.Key, note.Value)
	}
	present := map[string]bool{}
	for _, note := range right {
		present[spell(note)] = true
	}
	var lines []string
	for _, note := range left {
		if !present[spell(note)] {
			lines = append(lines, "  only in the first: "+spell(note))
		}
	}
	present = map[string]bool{}
	for _, note := range left {
		present[spell(note)] = true
	}
	for _, note := range right {
		if !present[spell(note)] {
			lines = append(lines, "  only in the second: "+spell(note))
		}
	}
	sort.Strings(lines)
	if len(lines) > 8 {
		lines = append(lines[:8], fmt.Sprintf("  ... and %d more", len(lines)-8))
	}
	return strings.Join(lines, "\n")
}

// compareStoredDeltas reports every declaration and every artifact the two
// directories both hold and disagree about.
func compareStoredDeltas(t *testing.T, first, second string) []string {
	t.Helper()
	firstFiles := cachedUnitFiles(t, first)
	secondFiles := cachedUnitFiles(t, second)

	var shared []string
	for key := range firstFiles {
		if secondFiles[key] != "" {
			shared = append(shared, key)
		}
	}
	sort.Strings(shared)
	require.NotEmpty(t, shared, "the two programs stored no package under the same key, so nothing is being compared")

	var differences []string
	declarations, artifacts := 0, 0
	for _, key := range shared {
		left := readUnitFile(t, firstFiles[key])
		right := readUnitFile(t, secondFiles[key])
		require.Equal(t, left.Entry.Package, right.Entry.Package,
			"two packages share a key digest, which the key is supposed to make impossible")
		pkg := left.Entry.Package

		names := make([]string, 0, len(left.Decls))
		for name := range left.Decls {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			leftDecl, rightDecl := left.Decls[name], right.Decls[name]
			if rightDecl == nil {
				continue
			}
			declarations++
			if strings.Join(leftDecl.Files, "\x00") != strings.Join(rightDecl.Files, "\x00") {
				differences = append(differences, fmt.Sprintf("%s %s (%s): file table differs: %v against %v",
					pkg, name, leftDecl.Symbol, leftDecl.Files, rightDecl.Files))
				continue
			}
			if reason := sequenceDifference(leftDecl.cachedSequence, rightDecl.cachedSequence); reason != "" {
				differences = append(differences, fmt.Sprintf("%s %s (%s): %s", pkg, name, leftDecl.Symbol, reason))
			}
		}

		for _, symbol := range left.artifactNames() {
			rightArtifact := right.Artifacts[symbol]
			if rightArtifact == nil {
				continue
			}
			artifacts++
			if reason := sequenceDifference(left.Artifacts[symbol].cachedSequence, rightArtifact.cachedSequence); reason != "" {
				differences = append(differences, fmt.Sprintf("%s artifact %s: %s", pkg, symbol, reason))
			}
		}
	}
	t.Logf("compared %d declarations and %d artifacts across %d packages both programs stored",
		declarations, artifacts, len(shared))
	require.Greater(t, declarations, 200, "too few shared declarations for this to be a meaningful comparison")
	sort.Strings(differences)
	return differences
}

// TestStoredDeltasDoNotDependOnTheProgram is the property stated at the top of
// this file, run over the widest pair of closures the suite has.
//
// It is the guard for two leaks that were live when it was written, and it is
// deliberately not written as a regression test for either of them. A fourth of
// the same shape appears here as a named declaration and a diff, at the moment it
// is introduced, rather than as a corpus program that fails to link a stage later.
func TestStoredDeltasDoNotDependOnTheProgram(t *testing.T) {
	reflectCache := t.TempDir()
	textCache := t.TempDir()
	_, reflectStats := compileWithCache(t, reflectCache, "disjointreflect.go", programDisjointReflect)
	require.Greater(t, reflectStats.Wrote, 10, "the reflect program stored nothing")
	_, textStats := compileWithCache(t, textCache, "disjointtext.go", programDisjointText)
	require.Greater(t, textStats.Wrote, 10, "the text program stored nothing")

	differences := compareStoredDeltas(t, reflectCache, textCache)
	if len(differences) == 0 {
		return
	}
	shown := differences
	if len(shown) > 200 {
		shown = append(append([]string(nil), shown[:200]...),
			fmt.Sprintf("... and %d more", len(differences)-20))
	}
	t.Fatalf("%d declarations or artifacts were stored differently by two programs that agreed on the key:\n%s",
		len(differences), strings.Join(shown, "\n"))
}

// TestStoredDeltasDoNotDependOnTheProgramAcrossASharedClosure runs the same
// comparison over two programs that share almost everything, which reaches
// packages the disjoint pair does not: a program's own root package, and the
// deeper reaches of fmt and reflect that only a formatting program pulls in.
func TestStoredDeltasDoNotDependOnTheProgramAcrossASharedClosure(t *testing.T) {
	firstCache := t.TempDir()
	secondCache := t.TempDir()
	compileWithCache(t, firstCache, "tracked.go", programCleanupTracked)
	compileWithCache(t, secondCache, "payload.go", programCleanupPayload)

	differences := compareStoredDeltas(t, firstCache, secondCache)
	if len(differences) == 0 {
		return
	}
	shown := differences
	if len(shown) > 200 {
		shown = append(append([]string(nil), shown[:200]...),
			fmt.Sprintf("... and %d more", len(differences)-20))
	}
	t.Fatalf("%d declarations or artifacts were stored differently by two programs that agreed on the key:\n%s",
		len(differences), strings.Join(shown, "\n"))
}

// TestStoredDeltasAcrossManyPrograms is the same instrument pointed at as much of
// the corpus as a caller asks for, and it is how a fifth leak of this shape would
// be found rather than guessed at.
//
// Off unless CG12_DELTA_PROBE names programs, because it compiles each one in
// process: the pair above is the guard that runs on every `go test`, and this is
// the sweep. The value is a comma-separated list of paths, or "corpus:N" for the
// first N of goc/testdata in name order.
//
//	CG12_DELTA_PROBE=corpus:24 go test -run TestStoredDeltasAcrossManyPrograms ./goc/
//
// Every ordered pair is not needed and is not run: the comparison is symmetric and
// transitive enough that comparing each program against the first one that stored
// a given package finds any disagreement about it. What is compared is every
// package any two of the programs stored under the same key.
func TestStoredDeltasAcrossManyPrograms(t *testing.T) {
	setting := os.Getenv("CG12_DELTA_PROBE")
	if setting == "" {
		t.Skip("set CG12_DELTA_PROBE to a program list or corpus:N")
	}
	programs := probePrograms(t, setting)
	t.Logf("delta probe over %d programs", len(programs))

	directories := make([]string, len(programs))
	for index, program := range programs {
		source, err := os.ReadFile(program)
		require.NoError(t, err)
		directories[index] = t.TempDir()
		_, stats := compileWithCache(t, directories[index], filepath.Base(program), string(source))
		t.Logf("%2d/%d %-52s %d packages stored", index+1, len(programs), filepath.Base(program), stats.Wrote)
	}

	var differences []string
	for index := 1; index < len(directories); index++ {
		for _, difference := range compareStoredDeltas(t, directories[0], directories[index]) {
			differences = append(differences, filepath.Base(programs[index])+": "+difference)
		}
	}
	if len(differences) > 0 {
		t.Fatalf("%d program-dependent deltas:\n%s", len(differences), strings.Join(differences, "\n"))
	}
}

func probePrograms(t *testing.T, setting string) []string {
	t.Helper()
	if count, found := strings.CutPrefix(setting, "corpus:"); found {
		limit, err := strconv.Atoi(count)
		require.NoError(t, err)
		all, err := filepath.Glob("testdata/*.go")
		require.NoError(t, err)
		sort.Strings(all)
		if limit < len(all) {
			all = all[:limit]
		}
		return all
	}
	return strings.Split(setting, ",")
}
