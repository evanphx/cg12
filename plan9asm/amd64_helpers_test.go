package plan9asm

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// amd64SourceFile names one real Go 1.26.1 assembly file that go/build selects
// for GOOS=linux GOARCH=amd64.
type amd64SourceFile struct {
	packagePath string
	name        string
}

// amd64SelectedFiles is every file in the amd64 allowlist, in a stable order.
func amd64SelectedFiles() []amd64SourceFile {
	var files []amd64SourceFile
	for _, packagePath := range sortedKeys(supportedAMD64Files) {
		for _, name := range sortedKeys(supportedAMD64Files[packagePath]) {
			files = append(files, amd64SourceFile{packagePath: packagePath, name: name})
		}
	}
	return files
}

// amd64UnlistedFiles are the remaining assembly files go/build selects for
// linux/amd64. They stay off the allowlist, as their ARM64 counterparts are
// off the ARM64 one, so those packages keep building from their purego
// sources; but they are held to the same assemble-and-match checks, so that
// listing one is a one-line change backed by evidence.
var amd64UnlistedFiles = []amd64SourceFile{
	{packagePath: "crypto/internal/fips140/aes", name: "aes_amd64.s"},
	{packagePath: "crypto/internal/fips140/aes", name: "ctr_amd64.s"},
	{packagePath: "crypto/internal/fips140/aes/gcm", name: "gcm_amd64.s"},
	{packagePath: "crypto/internal/fips140/bigmod", name: "nat_amd64.s"},
	{packagePath: "crypto/internal/fips140/edwards25519/field", name: "fe_amd64.s"},
	{packagePath: "crypto/internal/fips140/nistec", name: "p256_asm_amd64.s"},
	{packagePath: "crypto/internal/fips140/sha3", name: "sha3_amd64.s"},
	{packagePath: "crypto/internal/fips140/subtle", name: "xor_amd64.s"},
	{packagePath: "vendor/golang.org/x/crypto/chacha20poly1305", name: "chacha20poly1305_amd64.s"},
	{packagePath: "vendor/golang.org/x/crypto/internal/poly1305", name: "sum_amd64.s"},
	{packagePath: "runtime/cgo", name: "asm_amd64.s"},
}

// amd64VerifiedFiles is the allowlist plus amd64UnlistedFiles.
func amd64VerifiedFiles() []amd64SourceFile {
	return append(amd64SelectedFiles(), amd64UnlistedFiles...)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// amd64ParseOptions mirrors what the Go toolchain gives cmd/asm for a
// linux/amd64 build: the runtime's shared headers (the contents of
// $GOROOT/pkg/include plus go_tls.h and cgo/abi_amd64.h), any headers that sit
// next to the package's own sources, and the GOOS/GOARCH/GOAMD64 macros.
func amd64ParseOptions(t testing.TB, packagePath string) ParseOptions {
	t.Helper()
	runtimeDirectory := filepath.Join("..", "stdlib", "src", "runtime")
	includes := make(map[string]string)
	for _, name := range []string{"textflag.h", "go_tls.h", "funcdata.h", "asm_amd64.h", "cgo/abi_amd64.h"} {
		source, err := os.ReadFile(filepath.Join(runtimeDirectory, filepath.FromSlash(name)))
		require.NoError(t, err)
		includes[name] = string(source)
	}
	packageDirectory := filepath.Join("..", "stdlib", "src", filepath.FromSlash(packagePath))
	headers, err := filepath.Glob(filepath.Join(packageDirectory, "*.h"))
	require.NoError(t, err)
	for _, header := range headers {
		source, err := os.ReadFile(header)
		require.NoError(t, err)
		includes[filepath.Base(header)] = string(source)
	}
	return ParseOptions{
		Defines: map[string]string{
			"GOARCH_amd64": "1",
			"GOOS_linux":   "1",
			"GOAMD64_v1":   "1",
		},
		Includes: includes,
	}
}

// amd64AssemblyDefines returns the go_asm.h constants a package's assembly
// needs, as the frontend would supply them in AMD64Options.Defines. The values
// come from the go_asm.h Go 1.26.1 generates for linux/amd64, filtered to the
// names the assembly mentions (testdata/amd64/go_asm).
func amd64AssemblyDefines(t testing.TB, packagePath string) map[string]int64 {
	t.Helper()
	defines := make(map[string]int64)
	name := strings.ReplaceAll(packagePath, "/", "_") + ".h"
	source, err := os.ReadFile(filepath.Join("testdata", "amd64", "go_asm", name))
	if os.IsNotExist(err) {
		return defines
	}
	require.NoError(t, err)
	scanner := bufio.NewScanner(bytes.NewReader(source))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] != "#define" {
			continue
		}
		value, err := strconv.ParseInt(fields[2], 0, 64)
		require.NoError(t, err, scanner.Text())
		defines[fields[1]] = value
	}
	require.NoError(t, scanner.Err())
	return defines
}

func parseAMD64SourceFile(t testing.TB, file amd64SourceFile) *File {
	t.Helper()
	path := filepath.Join("..", "stdlib", "src", filepath.FromSlash(file.packagePath), file.name)
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	parsed, err := ParseWithOptions(bytes.NewReader(source), amd64ParseOptions(t, file.packagePath))
	require.NoError(t, err, "%s/%s", file.packagePath, file.name)
	return parsed
}

func translateAMD64SourceFile(t testing.TB, file amd64SourceFile) AMD64Translation {
	t.Helper()
	parsed := parseAMD64SourceFile(t, file)
	translation, err := CompileAMD64(parsed, AMD64Options{
		PackagePath: file.packagePath,
		Filename:    file.name,
		Defines:     amd64AssemblyDefines(t, file.packagePath),
	})
	require.NoError(t, err, "%s/%s", file.packagePath, file.name)
	return translation
}
