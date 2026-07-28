package goc

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadExactStandardSHA256Source(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
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
	t.Setenv(stdlibOverlayEnvironment, "off")

	loader := newSourceLoader(token.NewFileSet(), HostTarget())
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
	if len(unit.native) != 0 || len(unit.overlays) != 0 {
		t.Fatalf("exact runtime load applied %d native and %d source overlays", len(unit.native), len(unit.overlays))
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
	for _, name := range []string{
		"asm.s",
		"asm_arm64.s",
		"atomic_arm64.s",
		"ints.s",
		"memclr_arm64.s",
		"memmove_arm64.s",
		"preempt_arm64.s",
		"rt0_linux_arm64.s",
		"secret_arm64.s",
		"sys_linux_arm64.s",
		"tls_arm64.s",
	} {
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
	for _, file := range unit.assembly {
		if filepath.Base(file.path) != "tls_arm64.s" {
			continue
		}
		for _, name := range []string{"textflag.h", "go_tls.h", "funcdata.h", "tls_arm64.h"} {
			want, err := os.ReadFile(filepath.Join(loader.root, "src", "runtime", name))
			if err != nil {
				t.Fatal(err)
			}
			if file.includes[name] != string(want) {
				t.Errorf("runtime include %s was not retained unchanged", name)
			}
		}
		return
	}
	t.Fatal("runtime tls_arm64.s assembly unit was not retained")
}

func TestLoadStandardRuntimeOverlay(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("the initial runtime overlay targets linux/arm64")
	}

	loader := newSourceLoader(token.NewFileSet(), HostTarget())
	pkg, err := loader.Import("runtime")
	if err != nil {
		t.Fatal(err)
	}
	unit := loader.units[pkg.Path()]
	if unit == nil {
		t.Fatal("runtime source unit was not retained")
	}

	foundReplacement := false
	foundAddition := false
	for _, file := range unit.files {
		filename := loader.fset.Position(file.Package).Filename
		switch filepath.Base(filename) {
		case "set_vma_name_linux.go":
			if !strings.Contains(filename, filepath.Join("stdlib", "overlays")) {
				t.Fatalf("set_vma_name_linux.go loaded from %q, want overlay", filename)
			}
			foundReplacement = true
		case "cg12_overlay_linux_arm64.go":
			foundAddition = true
		}
	}
	if !foundReplacement {
		t.Fatal("runtime VMA replacement was not loaded")
	}
	if !foundAddition {
		t.Fatal("runtime Go overlay addition was not loaded")
	}
	if len(unit.native) != 1 {
		t.Fatalf("runtime native overlays = %d, want 1", len(unit.native))
	}
	if filepath.Base(unit.native[0].path) != "cg12_overlay.ssa" {
		t.Fatalf("runtime native overlay = %q, want cg12_overlay.ssa", unit.native[0].path)
	}
	if len(unit.overlays) != 3 {
		t.Fatalf("runtime applied overlays = %d, want 3", len(unit.overlays))
	}
}

func TestLoadExactStandardTestingSource(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
	pkg, err := loader.Import("testing")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Path() != "testing" {
		t.Fatalf("path = %q", pkg.Path())
	}

	unit := loader.units[pkg.Path()]
	if unit == nil || len(unit.files) == 0 {
		t.Fatal("testing source AST was not retained")
	}
	for _, file := range unit.files {
		position := loader.fset.Position(file.Package)
		stdlibSource := filepath.Join("stdlib", "src", "testing")
		if !strings.Contains(position.Filename, stdlibSource) {
			t.Errorf("loaded testing from %q, want repository stdlib", position.Filename)
		}
	}
	for _, path := range []string{
		"context",
		"flag",
		"internal/synctest",
		"internal/sysinfo",
		"internal/unsafeheader",
		"math/rand",
		"path/filepath",
		"runtime/debug",
		"runtime/trace",
	} {
		if loader.units[path] == nil {
			t.Errorf("testing source dependency %q was not retained", path)
		}
	}
}

func TestLoadVendoredDNSMessageSource(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
	const packagePath = "golang.org/x/net/dns/dnsmessage"
	loaded, err := loader.Import(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Path() != packagePath {
		t.Fatalf("package path = %q, want %q", loaded.Path(), packagePath)
	}
	unit := loader.units[packagePath]
	if unit == nil || len(unit.files) == 0 {
		t.Fatal("vendored DNS package source AST was not retained")
	}
	wantDirectory := filepath.Join("stdlib", "src", "vendor", "golang.org", "x", "net", "dns", "dnsmessage")
	for _, file := range unit.files {
		position := loader.fset.Position(file.Package)
		if !strings.Contains(position.Filename, wantDirectory) {
			t.Fatalf("loaded vendored DNS source from %q, want repository stdlib", position.Filename)
		}
	}
}

func TestLoadStandardNetWithoutCgo(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
	if _, err := loader.Import("net"); err != nil {
		t.Fatal(err)
	}
	unit := loader.units["net"]
	if unit == nil {
		t.Fatal("net source unit was not retained")
	}
	foundStub := false
	for _, file := range unit.files {
		filename := filepath.Base(loader.fset.Position(file.Package).Filename)
		if filename == "cgo_unix.go" {
			t.Fatal("net/cgo_unix.go was selected for the non-cgo cg12 target")
		}
		if filename == "cgo_stub.go" {
			foundStub = true
		}
	}
	if !foundStub {
		t.Fatal("net/cgo_stub.go was not selected for the non-cgo cg12 target")
	}
}

func TestLoadExactStandardBytealgAssembly(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
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
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
	tests := []struct {
		path string
		file string
	}{
		{path: "internal/cpu", file: "cpu_arm64.s"},
		{path: "internal/cpu", file: "cpu.s"},
		{path: "internal/abi", file: "abi_test.s"},
		{path: "internal/abi", file: "stub.s"},
		{path: "internal/chacha8rand", file: "chacha8_arm64.s"},
		{path: "internal/reflectlite", file: "asm.s"},
		{path: "internal/runtime/sys", file: "dit_arm64.s"},
		{path: "internal/runtime/sys", file: "empty.s"},
		{path: "internal/runtime/syscall/linux", file: "asm_linux_arm64.s"},
		{path: "internal/runtime/atomic", file: "atomic_arm64.s"},
		{path: "crypto/internal/fips140/sha256", file: "sha256block_arm64.s"},
		{path: "syscall", file: "asm_linux_arm64.s"},
		{path: "sync/atomic", file: "asm.s"},
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
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
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
		"encoding/json",
		"hash/adler32",
		"hash/crc32",
		"hash/fnv",
		"crypto/md5",
		"crypto/sha1",
		"crypto/sha512",
		"unicode/utf16",
		"path",
		"internal/testhash",
		"container/list",
		"container/ring",
		"sort",
		"container/heap",
		"maps",
		"bufio",
		"encoding/ascii85",
		"encoding/base32",
		"encoding/base64",
		"encoding/csv",
		"testing/iotest",
		"testing/synctest",
		"compress/flate",
		"compress/gzip",
		"crypto/tls",
		"crypto/x509",
		"encoding/asn1",
		"encoding/pem",
		"math/big",
		"log/internal",
		"log",
		"vendor/golang.org/x/net/dns/dnsmessage",
		"vendor/golang.org/x/net/http/httpguts",
		"vendor/golang.org/x/net/http/httpproxy",
		"vendor/golang.org/x/net/http2/hpack",
		"vendor/golang.org/x/net/idna",
		"vendor/golang.org/x/crypto/chacha20poly1305",
		"vendor/golang.org/x/crypto/cryptobyte",
		"vendor/golang.org/x/text/secure/bidirule",
		"vendor/golang.org/x/text/transform",
		"vendor/golang.org/x/text/unicode/bidi",
		"vendor/golang.org/x/text/unicode/norm",
		"internal/nettrace",
		"internal/singleflight",
		"net",
		"net/http",
		"net/http/httptest",
		"net/http/httptrace",
		"net/http/internal",
		"net/http/internal/ascii",
		"net/http/internal/httpcommon",
		"net/http/internal/testcert",
		"net/netip",
		"net/textproto",
		"net/url",
		"mime",
		"mime/multipart",
		"unique",
		"weak",
		"runtime",
	}
	for _, path := range packages {
		directory := filepath.Join(loader.root, "src", filepath.FromSlash(path))
		if _, err := os.Stat(directory); err != nil {
			t.Errorf("standard-library package %q: %v", path, err)
		}
	}
}
