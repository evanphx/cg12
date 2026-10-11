package plan9asm

import (
	"debug/elf"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAMD64AllowlistLinksConsistently translates the whole allowlist the way
// a backend would (collecting <ABIInternal> definitions from every file
// first), combines the objects with a relocatable link, and checks the
// cross-file symbol contract: no symbol is defined twice, no file calls the
// _abi0 entry of a function another file defines as <ABIInternal> (there is
// none), and every symbol still undefined is one the backend is expected to
// provide -- something the assembly does not define at all.
func TestAMD64AllowlistLinksConsistently(t *testing.T) {
	cc := requireAMD64Assembler(t)
	files := amd64SelectedFiles()
	parsed := make([]*File, len(files))
	abiInternal := make(map[string]bool)
	for index, file := range files {
		parsed[index] = parseAMD64SourceFile(t, file)
		for _, name := range AMD64ABIInternalSymbols(parsed[index], file.packagePath) {
			abiInternal[name] = true
		}
	}

	defined := make(map[string]bool)
	abi0References := make(map[string]bool)
	var objects []string
	for index, file := range files {
		translation, err := CompileAMD64(parsed[index], AMD64Options{
			PackagePath:        file.packagePath,
			Filename:           file.name,
			Defines:            amd64AssemblyDefines(t, file.packagePath),
			ABIInternalSymbols: abiInternal,
		})
		require.NoError(t, err, "%s/%s", file.packagePath, file.name)
		for _, function := range translation.Functions {
			defined[function.Name] = true
		}
		for _, reference := range translation.ABI0References {
			abi0References[reference] = true
		}
		name := strings.ReplaceAll(file.packagePath, "/", "_") + "_" + strings.TrimSuffix(file.name, ".s")
		objects = append(objects, assembleAMD64(t, cc, name, translation.Assembly))
	}

	combined := filepath.Join(t.TempDir(), "combined.o")
	arguments := append([]string{"-r", "-nostdlib", "-o", combined}, objects...)
	output, err := exec.Command(cc, arguments...).CombinedOutput()
	require.NoError(t, err, "%s", output)

	object, err := elf.Open(combined)
	require.NoError(t, err)
	defer object.Close()
	symbols, err := object.Symbols()
	require.NoError(t, err)
	var undefined []string
	for _, symbol := range symbols {
		if symbol.Section != elf.SHN_UNDEF || symbol.Name == "" {
			continue
		}
		undefined = append(undefined, symbol.Name)
		base := strings.TrimSuffix(symbol.Name, "_abi0")
		require.False(t, abiInternal[base] && base != symbol.Name, "%s is called through an ABI0 entry but defined <ABIInternal>", symbol.Name)
		require.False(t, defined[symbol.Name], "%s is defined by a translation but unresolved", symbol.Name)
		if base != symbol.Name {
			require.True(t, abi0References[base], "%s is referenced but not reported in ABI0References", symbol.Name)
		}
	}
	sort.Strings(undefined)
	// GNU as adds an undefined _GLOBAL_OFFSET_TABLE_ to any object with a
	// TPOFF relocation; the final link always defines it.
	t.Logf("%d symbols left for the backend: %s", len(undefined), strings.Join(undefined, " "))
}
