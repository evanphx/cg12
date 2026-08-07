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

// This file is the stage-3 gate's own pointing of goc/functioncachedelta_test.go's
// instrument, and it differs from TestStoredDeltasAcrossManyPrograms in the one
// way that decides what a sweep can find.
//
// That test compares every program against directories[0]. Its comment says the
// comparison is "each program against the first one that stored a given package",
// and for a package program 0 stored that is what happens -- but a package that
// programs 7 and 19 both store and program 0 does not is never compared at all.
// The first 24 programs of the corpus in name order are all small, so their union
// of closures is small, and a sweep anchored on the first of them leaves most of
// what the later ones store unexamined.
//
// This one indexes by unit key across the whole selection and compares each key
// against the FIRST program that stored it, which is what the property actually
// requires: any two programs that agreed on a key must agree on the delta under
// it.
//
//	CG12_GATE_DELTA_PROBE=a.go,b.go go test -run TestGateStoredDeltasByFirstStorer ./goc/
//	CG12_GATE_DELTA_PROBE=spread:32 go test -run TestGateStoredDeltasByFirstStorer ./goc/
//
// "spread:N" takes N programs evenly spaced through the corpus in name order,
// starting past the first 24 the branch's sweep already covered.
func TestGateStoredDeltasByFirstStorer(t *testing.T) {
	setting := os.Getenv("CG12_GATE_DELTA_PROBE")
	if setting == "" {
		t.Skip("set CG12_GATE_DELTA_PROBE to a program list, corpus:N or spread:N")
	}
	programs := gateProbePrograms(t, setting)
	t.Logf("gate delta probe over %d programs: %v", len(programs), programs)

	directories := make([]string, len(programs))
	for index, program := range programs {
		source, err := os.ReadFile(program)
		require.NoError(t, err)
		directories[index] = t.TempDir()
		_, stats := compileWithCache(t, directories[index], filepath.Base(program), string(source))
		t.Logf("%2d/%d %-52s %d packages stored", index+1, len(programs), filepath.Base(program), stats.Wrote)
	}

	// key -> the index of the first program that stored it, and its path there.
	type holder struct {
		program int
		path    string
	}
	first := map[string]holder{}
	files := make([]map[string]string, len(directories))
	for index, directory := range directories {
		files[index] = cachedUnitFiles(t, directory)
		for key, path := range files[index] {
			if _, seen := first[key]; !seen {
				first[key] = holder{program: index, path: path}
			}
		}
	}
	t.Logf("%d distinct package units across the selection", len(first))

	var differences []string
	declarations, artifacts, pairs, keysCompared := 0, 0, 0, map[string]bool{}
	for index := range directories {
		for key, path := range files[index] {
			owner := first[key]
			if owner.program == index {
				continue
			}
			pairs++
			keysCompared[key] = true
			left, right := readUnitFile(t, owner.path), readUnitFile(t, path)
			require.Equal(t, left.Entry.Package, right.Entry.Package,
				"two packages share a key digest, which the key is supposed to make impossible")
			pkg := left.Entry.Package
			where := fmt.Sprintf("%s vs %s", filepath.Base(programs[owner.program]), filepath.Base(programs[index]))

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
					differences = append(differences, fmt.Sprintf("%s: %s %s (%s): file table differs: %v against %v",
						where, pkg, name, leftDecl.Symbol, leftDecl.Files, rightDecl.Files))
					continue
				}
				if reason := sequenceDifference(leftDecl.cachedSequence, rightDecl.cachedSequence); reason != "" {
					differences = append(differences, fmt.Sprintf("%s: %s %s (%s): %s",
						where, pkg, name, leftDecl.Symbol, reason))
				}
			}
			for _, symbol := range left.artifactNames() {
				rightArtifact := right.Artifacts[symbol]
				if rightArtifact == nil {
					continue
				}
				artifacts++
				if reason := sequenceDifference(left.Artifacts[symbol].cachedSequence, rightArtifact.cachedSequence); reason != "" {
					differences = append(differences, fmt.Sprintf("%s: %s artifact %s: %s", where, pkg, symbol, reason))
				}
			}
		}
	}
	t.Logf("compared %d declarations and %d artifacts over %d shared-unit pairs covering %d distinct package units",
		declarations, artifacts, pairs, len(keysCompared))
	require.Greater(t, declarations, 200, "too few shared declarations for this to be a meaningful comparison")

	if len(differences) > 0 {
		sort.Strings(differences)
		shown := differences
		if len(shown) > 200 {
			shown = append(append([]string(nil), shown[:200]...), fmt.Sprintf("... and %d more", len(differences)-200))
		}
		t.Fatalf("%d program-dependent deltas:\n%s", len(differences), strings.Join(shown, "\n"))
	}
}

func gateProbePrograms(t *testing.T, setting string) []string {
	t.Helper()
	all, err := filepath.Glob("testdata/*.go")
	require.NoError(t, err)
	sort.Strings(all)
	if count, found := strings.CutPrefix(setting, "spread:"); found {
		limit, err := strconv.Atoi(count)
		require.NoError(t, err)
		require.Greater(t, limit, 0)
		// Past the first 24, which the branch's own sweep covered, and evenly
		// spaced through the rest so the selection is not one neighbourhood of the
		// alphabet.
		rest := all[24:]
		step := len(rest) / limit
		require.Greater(t, step, 0, "spread wider than the corpus")
		var chosen []string
		for index := 0; index < len(rest) && len(chosen) < limit; index += step {
			chosen = append(chosen, rest[index])
		}
		return chosen
	}
	if count, found := strings.CutPrefix(setting, "corpus:"); found {
		limit, err := strconv.Atoi(count)
		require.NoError(t, err)
		if limit < len(all) {
			all = all[:limit]
		}
		return all
	}
	return strings.Split(setting, ",")
}
