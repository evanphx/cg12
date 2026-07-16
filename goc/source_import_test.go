package goc

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExactStandardSHA256Source(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet())
	pkg, err := loader.Import("crypto/sha256")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Path() != "crypto/sha256" {
		t.Fatalf("path = %q", pkg.Path())
	}
	unit := loader.units[pkg.Path()]
	if unit == nil || len(unit.files) == 0 {
		t.Fatal("source AST was not retained")
	}
	found := false
	for _, file := range unit.files {
		position := loader.fset.Position(file.Package)
		stdlibSource := filepath.Join("stdlib", "src", "crypto", "sha256")
		if !strings.Contains(position.Filename, stdlibSource) {
			t.Errorf("loaded SHA-256 from %q, want repository stdlib", position.Filename)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Name.Name == "Sum256" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("build-selected standard library source has no Sum256")
	}

	for _, path := range []string{
		"crypto/internal/fips140/sha256",
		"crypto/internal/fips140deps/byteorder",
		"math/bits",
	} {
		if loader.units[path] == nil {
			t.Errorf("source dependency %q was not retained", path)
		}
	}
}

func TestLoadExactStandardRuntimeSource(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet())
	pkg, err := loader.Import("runtime")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Path() != "runtime" {
		t.Fatalf("path = %q", pkg.Path())
	}

	unit := loader.units[pkg.Path()]
	if unit == nil || len(unit.files) == 0 {
		t.Fatal("source AST was not retained")
	}
	for _, file := range unit.files {
		position := loader.fset.Position(file.Package)
		stdlibSource := filepath.Join("stdlib", "src", "runtime")
		if !strings.Contains(position.Filename, stdlibSource) {
			t.Errorf("loaded runtime from %q, want repository stdlib", position.Filename)
		}
	}
	assembly := make(map[string]string)
	for _, file := range unit.assembly {
		assembly[filepath.Base(file.path)] = file.source
	}
	for _, name := range []string{"atomic_arm64.s", "memclr_arm64.s", "memmove_arm64.s", "preempt_arm64.s", "secret_arm64.s"} {
		got, ok := assembly[name]
		if !ok {
			t.Errorf("runtime assembly %s was not retained", name)
			continue
		}
		path := filepath.Join(loader.root, "src", "runtime", name)
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("runtime assembly %s was modified while loading", name)
		}
	}
}

func TestLoadExactStandardBytealgAssembly(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet())
	_, err := loader.Import("internal/bytealg")
	if err != nil {
		t.Fatal(err)
	}

	unit := loader.units["internal/bytealg"]
	if unit == nil {
		t.Fatal("internal/bytealg source unit was not retained")
	}
	wantNames := map[string]bool{
		"compare_arm64.s":   true,
		"count_arm64.s":     true,
		"equal_arm64.s":     true,
		"index_arm64.s":     true,
		"indexbyte_arm64.s": true,
	}
	if len(unit.assembly) != len(wantNames) {
		t.Fatalf("internal/bytealg assembly files = %d, want %d", len(unit.assembly), len(wantNames))
	}
	for _, assembly := range unit.assembly {
		name := filepath.Base(assembly.path)
		if !wantNames[name] {
			t.Errorf("unexpected internal/bytealg assembly path %q", assembly.path)
			continue
		}
		want, err := os.ReadFile(filepath.Join(loader.root, "src", "internal", "bytealg", name))
		if err != nil {
			t.Fatal(err)
		}
		if assembly.source != string(want) {
			t.Errorf("internal/bytealg %s was modified while loading", name)
		}
		delete(wantNames, name)
	}
	for name := range wantNames {
		t.Errorf("internal/bytealg assembly is missing %s", name)
	}
}

func TestLoadExactAdditionalStandardAssembly(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet())
	tests := []struct {
		path string
		file string
	}{
		{path: "internal/cpu", file: "cpu_arm64.s"},
		{path: "internal/chacha8rand", file: "chacha8_arm64.s"},
		{path: "internal/runtime/sys", file: "dit_arm64.s"},
		{path: "internal/runtime/syscall/linux", file: "asm_linux_arm64.s"},
		{path: "internal/runtime/atomic", file: "atomic_arm64.s"},
		{path: "syscall", file: "asm_linux_arm64.s"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			_, err := loader.Import(test.path)
			if err != nil {
				t.Fatal(err)
			}

			unit := loader.units[test.path]
			if unit == nil {
				t.Fatalf("%s source unit was not retained", test.path)
			}

			var got string
			for _, assembly := range unit.assembly {
				if filepath.Base(assembly.path) == test.file {
					got = assembly.source
					break
				}
			}
			if got == "" {
				t.Fatalf("%s was not retained for %s", test.file, test.path)
			}

			want, err := os.ReadFile(filepath.Join(loader.root, "src", filepath.FromSlash(test.path), test.file))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s was modified while loading", test.file)
			}
		})
	}
}

func TestRepositoryStandardLibraryInventory(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet())
	packages := []string{
		"internal/byteorder",
		"math/bits",
		"cmp",
		"unicode/utf8",
		"hash",
		"errors",
		"crypto/internal/fips140deps/byteorder",
		"crypto/internal/fips140",
		"crypto/internal/fips140/sha256",
		"crypto/sha256",
		"encoding/binary",
		"encoding/hex",
		"hash/adler32",
		"hash/crc32",
		"hash/fnv",
		"crypto/md5",
		"crypto/sha1",
		"crypto/sha512",
		"unicode/utf16",
		"path",
		"runtime",
	}
	for _, path := range packages {
		directory := filepath.Join(loader.root, "src", filepath.FromSlash(path))
		if _, err := os.Stat(directory); err != nil {
			t.Errorf("standard-library package %q: %v", path, err)
		}
	}
}
