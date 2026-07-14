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
	}
	for _, path := range packages {
		directory := filepath.Join(loader.root, "src", filepath.FromSlash(path))
		if _, err := os.Stat(directory); err != nil {
			t.Errorf("standard-library package %q: %v", path, err)
		}
	}
}
