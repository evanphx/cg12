package goc

// Gate probe for the stage-2 cross-program property, item 3: is anything in a
// stored declaration's delta a function of the PROGRAM rather than of its
// package?
//
// The branch names two such leaks and refuses the declarations that carry them:
// a reference to an artifact minted before the journal existed, and a
// declaration whose lowering read the program's implementation set. Both were
// found the same way -- something whose value came from the whole program ended
// up inside a per-declaration delta. This asks the question mechanically rather
// than by reading: fill two caches from two DIFFERENT programs, and compare, for
// every package file whose key digest is the same in both, every declaration and
// every artifact the two have in common.
//
// The key digest already covers the package's source, its imports' identities,
// the target, -O, the layout, the pipeline and the compiler. So two files with
// the same digest were written under identical conditions in every respect
// except which program was being compiled. If a delta is a function of its
// package alone, the two must be byte-identical. Any difference is a leak, and
// it names the package, the declaration and the field it is in.
//
// Off unless GATE2_CACHE_A and GATE2_CACHE_B name two filled cache directories.

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestGate2DeltaIsAFunctionOfItsPackage(t *testing.T) {
	t.Parallel()

	first, second := os.Getenv("GATE2_CACHE_A"), os.Getenv("GATE2_CACHE_B")
	if first == "" || second == "" {
		t.Skip("set GATE2_CACHE_A and GATE2_CACHE_B to two cache directories filled by two different programs")
	}

	left := gate2ReadUnits(t, first)
	right := gate2ReadUnits(t, second)
	t.Logf("A: %d package files in %s", len(left), first)
	t.Logf("B: %d package files in %s", len(right), second)

	shared := make([]string, 0, len(left))
	for digest := range left {
		if right[digest] != nil {
			shared = append(shared, digest)
		}
	}
	sort.Strings(shared)
	if len(shared) == 0 {
		t.Fatal("no package file has the same key digest in both caches; the two compiles share nothing to compare")
	}

	declarations, artifacts, mismatches := 0, 0, 0
	report := func(format string, arguments ...any) {
		mismatches++
		if mismatches <= 40 {
			t.Errorf(format, arguments...)
		}
	}

	sharedDecls, sharedArtifacts := 0, 0
	for _, digest := range shared {
		a, b := left[digest], right[digest]
		if a.Entry.Package != b.Entry.Package {
			report("digest %s: package %q in A, %q in B", digest[:12], a.Entry.Package, b.Entry.Package)
			continue
		}
		declarations += len(a.Decls)
		artifacts += len(a.Artifacts)
		for key, declarationA := range a.Decls {
			declarationB := b.Decls[key]
			if declarationB == nil {
				continue // reachability differs between the two programs; expected.
			}
			sharedDecls++
			where := a.Entry.Package + " " + key
			if declarationA.Symbol != declarationB.Symbol {
				report("%s: symbol %q in A, %q in B", where, declarationA.Symbol, declarationB.Symbol)
			}
			if strings.Join(declarationA.NewFiles, "\x00") != strings.Join(declarationB.NewFiles, "\x00") {
				report("%s: NewFiles %v in A, %v in B", where, declarationA.NewFiles, declarationB.NewFiles)
			}
			gate2CompareSequences(report, where, declarationA.cachedSequence, declarationB.cachedSequence)
		}
		for symbol, artifactA := range a.Artifacts {
			artifactB := b.Artifacts[symbol]
			if artifactB == nil {
				continue
			}
			sharedArtifacts++
			gate2CompareSequences(report, a.Entry.Package+" artifact "+symbol, artifactA.cachedSequence, artifactB.cachedSequence)
		}
	}

	t.Logf("%d package files with a common key digest; %d declarations and %d artifacts stored in A's copies of them",
		len(shared), declarations, artifacts)
	t.Logf("compared %d declarations and %d artifacts present in both; %d mismatches", sharedDecls, sharedArtifacts, mismatches)
	if sharedDecls == 0 {
		t.Fatal("no declaration is present in both caches; nothing was actually compared")
	}
}

// TestGate2ListCachePackages prints the packages a filled cache directory holds
// a unit for, which is how the gate picked two fillers with disjoint closures
// rather than assuming it from the source's import list.
func TestGate2ListCachePackages(t *testing.T) {
	t.Parallel()

	directory := os.Getenv("GATE2_CACHE_LIST")
	if directory == "" {
		t.Skip("set GATE2_CACHE_LIST to a filled cache directory")
	}
	units := gate2ReadUnits(t, directory)
	packages := make([]string, 0, len(units))
	for _, unit := range units {
		packages = append(packages, unit.Entry.Package)
	}
	sort.Strings(packages)
	for _, name := range packages {
		t.Logf("PKG %s", name)
	}
	t.Logf("%d unit files", len(units))
}

func gate2CompareSequences(report func(string, ...any), where string, a, b cachedSequence) {
	if !bytes.Equal(a.Unit, b.Unit) {
		report("%s: encoded delta differs (%d bytes in A, %d in B)", where, len(a.Unit), len(b.Unit))
	}
	if len(a.Refs) != len(b.Refs) {
		report("%s: %d artifact references in A, %d in B", where, len(a.Refs), len(b.Refs))
	} else {
		for index := range a.Refs {
			if a.Refs[index] != b.Refs[index] {
				report("%s: reference %d is %+v in A and %+v in B", where, index, a.Refs[index], b.Refs[index])
				break
			}
		}
	}
	if len(a.Interns) != len(b.Interns) {
		report("%s: %d intern notes in A, %d in B", where, len(a.Interns), len(b.Interns))
		return
	}
	for index := range a.Interns {
		if a.Interns[index] != b.Interns[index] {
			report("%s: intern note %d is %+v in A and %+v in B", where, index, a.Interns[index], b.Interns[index])
			break
		}
	}
}

// gate2ReadUnits decodes every unit file under a cache directory, keyed by the
// key digest its filename is.
func gate2ReadUnits(t *testing.T, directory string) map[string]*packageCacheUnit {
	t.Helper()
	units := map[string]*packageCacheUnit{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".gocfn" {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		unit, err := decodePackageCacheUnit(contents)
		if err != nil {
			t.Errorf("decode %s: %v", path, err)
			return nil
		}
		units[strings.TrimSuffix(filepath.Base(path), ".gocfn")] = unit
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", directory, err)
	}
	return units
}
