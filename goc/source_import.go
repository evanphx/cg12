package goc

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/evanphx/cg12/plan9asm"
)

// sourceUnit is the build-selected source for an imported package, including
// any explicit standard-library overlay files and their provenance.
type sourceUnit struct {
	path     string
	files    []*ast.File
	assembly []sourceAssemblyFile
	native   []sourceNativeFile
	overlays []sourceOverlayUse
	info     *types.Info
	pkg      *types.Package
}

type sourceAssemblyFile struct {
	path     string
	source   string
	includes map[string]string
}

type sourceNativeFile struct {
	path   string
	source string
	entry  stdlibOverlayEntry
}

// sourceLoader imports selected packages from source while retaining their AST
// for cg12 lowering. Dependencies use export data until they too are selected
// for source compilation.
type sourceLoader struct {
	fset                 *token.FileSet
	units                map[string]*sourceUnit
	loading              map[string]bool
	sources              map[string]bool
	testPackages         map[string]bool
	externalTestPackages map[string]string
	base                 types.Importer
	root                 string
	forcePureGo          bool
	overlay              *stdlibOverlay
	overlayErr           error
}

func newSourceLoader(fset *token.FileSet) *sourceLoader {
	loader := &sourceLoader{
		fset:                 fset,
		units:                make(map[string]*sourceUnit),
		loading:              make(map[string]bool),
		testPackages:         make(map[string]bool),
		externalTestPackages: make(map[string]string),
		sources: map[string]bool{
			"bufio":                                      true,
			"bytes":                                      true,
			"cmp":                                        true,
			"container/heap":                             true,
			"container/list":                             true,
			"container/ring":                             true,
			"context":                                    true,
			"compress/flate":                             true,
			"compress/gzip":                              true,
			"crypto":                                     true,
			"crypto/aes":                                 true,
			"crypto/cipher":                              true,
			"crypto/des":                                 true,
			"crypto/dsa":                                 true,
			"crypto/ecdh":                                true,
			"crypto/ecdsa":                               true,
			"crypto/ed25519":                             true,
			"crypto/elliptic":                            true,
			"crypto/fips140":                             true,
			"crypto/hkdf":                                true,
			"crypto/hmac":                                true,
			"crypto/hpke":                                true,
			"crypto/internal/boring":                     true,
			"crypto/internal/boring/bbig":                true,
			"crypto/internal/boring/sig":                 true,
			"crypto/internal/constanttime":               true,
			"crypto/internal/entropy/v1.0.0":             true,
			"crypto/internal/fips140/aes":                true,
			"crypto/internal/fips140/aes/gcm":            true,
			"crypto/internal/fips140/alias":              true,
			"crypto/internal/fips140/bigmod":             true,
			"crypto/internal/fips140/check":              true,
			"crypto/internal/fips140/drbg":               true,
			"crypto/internal/fips140/ecdh":               true,
			"crypto/internal/fips140/ecdsa":              true,
			"crypto/internal/fips140/ed25519":            true,
			"crypto/internal/fips140/edwards25519":       true,
			"crypto/internal/fips140/edwards25519/field": true,
			"crypto/internal/fips140/hkdf":               true,
			"crypto/internal/fips140/hmac":               true,
			"crypto/internal/fips140/mlkem":              true,
			"crypto/internal/fips140/nistec":             true,
			"crypto/internal/fips140/nistec/fiat":        true,
			"crypto/internal/fips140/rsa":                true,
			"crypto/internal/fips140/sha3":               true,
			"crypto/internal/fips140/subtle":             true,
			"crypto/internal/fips140/tls12":              true,
			"crypto/internal/fips140/tls13":              true,
			"crypto/internal/fips140cache":               true,
			"crypto/internal/fips140hash":                true,
			"crypto/internal/fips140only":                true,
			"crypto/internal/rand":                       true,
			"crypto/internal/randutil":                   true,
			"crypto/internal/sysrand":                    true,
			"crypto/mlkem":                               true,
			"crypto/rand":                                true,
			"crypto/rc4":                                 true,
			"crypto/rsa":                                 true,
			"crypto/sha3":                                true,
			"crypto/subtle":                              true,
			"crypto/tls":                                 true,
			"crypto/tls/internal/fips140tls":             true,
			"crypto/x509":                                true,
			"crypto/x509/pkix":                           true,
			"encoding":                                   true,
			"encoding/asn1":                              true,
			"encoding/pem":                               true,
			"errors":                                     true,
			"flag":                                       true,
			"fmt":                                        true,
			"golang.org/x/net/dns/dnsmessage":            true,
			"golang.org/x/net/http/httpguts":             true,
			"golang.org/x/net/http/httpproxy":            true,
			"golang.org/x/net/http2/hpack":               true,
			"golang.org/x/net/idna":                      true,
			"golang.org/x/crypto/chacha20":               true,
			"golang.org/x/crypto/chacha20poly1305":       true,
			"golang.org/x/crypto/cryptobyte":             true,
			"golang.org/x/crypto/cryptobyte/asn1":        true,
			"golang.org/x/crypto/internal/alias":         true,
			"golang.org/x/crypto/internal/poly1305":      true,
			"golang.org/x/text/secure/bidirule":          true,
			"golang.org/x/text/transform":                true,
			"golang.org/x/text/unicode/bidi":             true,
			"golang.org/x/text/unicode/norm":             true,
			"io":                                         true,
			"io/fs":                                      true,
			"crypto/sha256":                              true,
			"crypto/internal/fips140":                    true,
			"crypto/internal/fips140/sha256":             true,
			"crypto/internal/fips140/sha512":             true,
			"crypto/internal/fips140deps/byteorder":      true,
			"crypto/internal/fips140deps/cpu":            true,
			"crypto/internal/fips140deps/godebug":        true,
			"crypto/internal/fips140deps/time":           true,
			"crypto/internal/impl":                       true,
			"crypto/md5":                                 true,
			"crypto/sha1":                                true,
			"crypto/sha512":                              true,
			"encoding/ascii85":                           true,
			"encoding/base32":                            true,
			"encoding/base64":                            true,
			"encoding/binary":                            true,
			"encoding/csv":                               true,
			"encoding/hex":                               true,
			"encoding/json":                              true,
			"hash/adler32":                               true,
			"hash/crc32":                                 true,
			"hash/fnv":                                   true,
			"internal/abi":                               true,
			"internal/asan":                              true,
			"internal/bytealg":                           true,
			"internal/bisect":                            true,
			"internal/byteorder":                         true,
			"internal/chacha8rand":                       true,
			"internal/cfg":                               true,
			"internal/coverage/rtcov":                    true,
			"internal/cpu":                               true,
			"internal/filepathlite":                      true,
			"internal/fmtsort":                           true,
			"internal/goarch":                            true,
			"internal/godebug":                           true,
			"internal/godebugs":                          true,
			"internal/goexperiment":                      true,
			"internal/goos":                              true,
			"internal/msan":                              true,
			"internal/nettrace":                          true,
			"internal/oserror":                           true,
			"internal/platform":                          true,
			"internal/poll":                              true,
			"internal/profilerecord":                     true,
			"internal/race":                              true,
			"internal/reflectlite":                       true,
			"internal/saferio":                           true,
			"internal/runtime/atomic":                    true,
			"internal/runtime/cgroup":                    true,
			"internal/runtime/exithook":                  true,
			"internal/runtime/gc":                        true,
			"internal/runtime/gc/scan":                   true,
			"internal/runtime/maps":                      true,
			"internal/runtime/math":                      true,
			"internal/runtime/pprof/label":               true,
			"internal/runtime/sys":                       true,
			"internal/runtime/syscall/linux":             true,
			"internal/singleflight":                      true,
			"internal/strconv":                           true,
			"internal/stringslite":                       true,
			"internal/sync":                              true,
			"internal/synctest":                          true,
			"internal/sysinfo":                           true,
			"internal/syscall/execenv":                   true,
			"internal/syscall/unix":                      true,
			"internal/testenv":                           true,
			"internal/testhash":                          true,
			"internal/testlog":                           true,
			"internal/trace/tracev2":                     true,
			"internal/unsafeheader":                      true,
			"iter":                                       true,
			"log":                                        true,
			"log/internal":                               true,
			"maps":                                       true,
			"mime":                                       true,
			"mime/multipart":                             true,
			"mime/quotedprintable":                       true,
			"math":                                       true,
			"math/big":                                   true,
			"math/bits":                                  true,
			"math/rand":                                  true,
			"math/rand/v2":                               true,
			"net":                                        true,
			"net/http":                                   true,
			"net/http/httptest":                          true,
			"net/http/httptrace":                         true,
			"net/http/internal":                          true,
			"net/http/internal/ascii":                    true,
			"net/http/internal/httpcommon":               true,
			"net/http/internal/testcert":                 true,
			"net/netip":                                  true,
			"net/textproto":                              true,
			"net/url":                                    true,
			"os":                                         true,
			"os/exec":                                    true,
			"path":                                       true,
			"path/filepath":                              true,
			"reflect":                                    true,
			"regexp":                                     true,
			"regexp/syntax":                              true,
			"runtime":                                    true,
			"runtime/debug":                              true,
			"runtime/trace":                              true,
			"slices":                                     true,
			"sort":                                       true,
			"strconv":                                    true,
			"strings":                                    true,
			"sync":                                       true,
			"sync/atomic":                                true,
			"syscall":                                    true,
			"testing":                                    true,
			"testing/iotest":                             true,
			"testing/synctest":                           true,
			"text/template":                              true,
			"text/template/parse":                        true,
			"time":                                       true,
			"unicode":                                    true,
			"unicode/utf8":                               true,
			"unicode/utf16":                              true,
			"unique":                                     true,
			"weak":                                       true,
		},
		base: importer.Default(),
		root: repositoryStdlibRoot(),
	}
	loader.overlay, loader.overlayErr = loadStdlibOverlay(loader.root)
	return loader
}

func repositoryStdlibRoot() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("goc: cannot locate repository standard library")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "stdlib"))
}

func (l *sourceLoader) Import(path string) (*types.Package, error) {
	if l.overlayErr != nil {
		return nil, l.overlayErr
	}
	if u := l.units[path]; u != nil {
		return u.pkg, nil
	}
	packagePath := path
	externalTestPackage := false
	if testedPackage := l.externalTestPackages[path]; testedPackage != "" {
		packagePath = testedPackage
		externalTestPackage = true
	}
	if !externalTestPackage && !l.sources[path] {
		return l.base.Import(path)
	}
	if l.loading[path] {
		return nil, fmt.Errorf("source import cycle involving %s", path)
	}
	l.loading[path] = true
	defer delete(l.loading, path)
	ctx := build.Default
	ctx.BuildTags = append([]string{}, ctx.BuildTags...)
	ctx.CgoEnabled = false
	useAssembly := !externalTestPackage && !l.forcePureGo && runtime.GOARCH == "arm64" && plan9asm.SupportsARM64Package(path)
	if !useAssembly {
		ctx.BuildTags = append(ctx.BuildTags, "purego")
	}
	ctx.GOROOT = l.root
	var bp *build.Package
	var err error
	vendorDirectory := filepath.Join(l.root, "src", "vendor", filepath.FromSlash(packagePath))
	if vendorInfo, statError := os.Stat(vendorDirectory); statError == nil && vendorInfo.IsDir() {
		bp, err = ctx.ImportDir(vendorDirectory, 0)
	} else {
		bp, err = ctx.Import(packagePath, "", 0)
	}
	if err != nil {
		return nil, err
	}
	u := &sourceUnit{
		path: path,
		info: &types.Info{
			Types:      make(map[ast.Expr]types.TypeAndValue),
			Defs:       make(map[*ast.Ident]types.Object),
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
			Implicits:  make(map[ast.Node]types.Object),
			Instances:  make(map[*ast.Ident]types.Instance),
		},
	}
	var goFiles []string
	if externalTestPackage {
		goFiles = append(goFiles, bp.XTestGoFiles...)
	} else {
		goFiles = append(goFiles, bp.GoFiles...)
	}
	if !externalTestPackage && l.testPackages[path] {
		goFiles = append(goFiles, bp.TestGoFiles...)
	}
	for _, name := range goFiles {
		full := filepath.Join(bp.Dir, name)
		origin, source, overlayEntry, deleted, err := l.overlay.resolve(full)
		if err != nil {
			return nil, err
		}
		if deleted {
			u.overlays = append(u.overlays, overlayUse(overlayEntry))
			continue
		}
		file, err := parser.ParseFile(l.fset, origin, source, parser.ParseComments|parser.AllErrors)
		if err != nil {
			return nil, err
		}
		u.files = append(u.files, file)
		if overlayEntry != nil {
			u.overlays = append(u.overlays, overlayUse(overlayEntry))
		}
	}
	if !externalTestPackage {
		if err := l.addOverlayFiles(&ctx, u); err != nil {
			return nil, err
		}
	}
	for _, name := range bp.SFiles {
		if !useAssembly {
			continue
		}
		if !plan9asm.SupportsARM64File(path, name) {
			return nil, fmt.Errorf("translate %s: unsupported build-selected assembly file %s", path, name)
		}
		full := filepath.Join(bp.Dir, name)
		origin, source, overlayEntry, deleted, err := l.overlay.resolve(full)
		if err != nil {
			return nil, err
		}
		if deleted {
			u.overlays = append(u.overlays, overlayUse(overlayEntry))
			continue
		}
		u.assembly = append(u.assembly, sourceAssemblyFile{
			path:     filepath.ToSlash(filepath.Join(path, name)),
			source:   string(source),
			includes: l.assemblyIncludes(filepath.Dir(origin), string(source)),
		})
		if overlayEntry != nil {
			u.overlays = append(u.overlays, overlayUse(overlayEntry))
		}
	}
	conf := types.Config{Importer: l}
	u.pkg, err = conf.Check(path, l.fset, u.files, u.info)
	if err != nil {
		return nil, fmt.Errorf("type-check %s: %w", path, err)
	}
	l.units[path] = u
	return u.pkg, nil
}

func (l *sourceLoader) addOverlayFiles(ctx *build.Context, unit *sourceUnit) error {
	if l.overlay == nil {
		return nil
	}
	for _, entry := range l.overlay.additions[unit.path] {
		extension := strings.ToLower(filepath.Ext(entry.origin))
		switch extension {
		case ".go":
			name := filepath.Base(entry.origin)
			if strings.HasSuffix(name, "_test.go") && !l.testPackages[unit.path] {
				continue
			}
			matches, err := ctx.MatchFile(filepath.Dir(entry.origin), name)
			if err != nil {
				return fmt.Errorf("select standard library overlay %s: %w", entry.Path, err)
			}
			if !matches {
				continue
			}
			source, err := os.ReadFile(entry.origin)
			if err != nil {
				return err
			}
			file, err := parser.ParseFile(l.fset, entry.origin, source, parser.ParseComments|parser.AllErrors)
			if err != nil {
				return err
			}
			unit.files = append(unit.files, file)
		case ".ssa":
			source, err := os.ReadFile(entry.origin)
			if err != nil {
				return err
			}
			unit.native = append(unit.native, sourceNativeFile{
				path:   entry.origin,
				source: string(source),
				entry:  entry,
			})
		default:
			return fmt.Errorf("standard library overlay %s is neither Go nor native cg12 text", entry.Path)
		}
		unit.overlays = append(unit.overlays, overlayUse(&entry))
	}
	return nil
}

func (l *sourceLoader) assemblyIncludes(packageDirectory, source string) map[string]string {
	includes := make(map[string]string)
	loading := make(map[string]bool)
	var load func(directory, source string)
	load = func(directory, source string) {
		scanner := bufio.NewScanner(strings.NewReader(source))
		for scanner.Scan() {
			name, ok := assemblyIncludeName(scanner.Text())
			if !ok || includes[name] != "" || loading[name] {
				continue
			}
			candidates := []string{
				filepath.Join(directory, filepath.FromSlash(name)),
				filepath.Join(l.root, "src", "runtime", filepath.FromSlash(name)),
			}
			var content []byte
			var path string
			for _, candidate := range candidates {
				read, err := os.ReadFile(candidate)
				if err == nil {
					content = read
					path = candidate
					break
				}
			}
			if content == nil {
				continue
			}
			loading[name] = true
			includes[name] = string(content)
			load(filepath.Dir(path), string(content))
			delete(loading, name)
		}
	}
	load(packageDirectory, source)
	return includes
}

func assemblyIncludeName(source string) (string, bool) {
	source = strings.TrimSpace(source)
	const prefix = "#include"
	if !strings.HasPrefix(source, prefix) {
		return "", false
	}
	source = strings.TrimSpace(strings.TrimPrefix(source, prefix))
	if !strings.HasPrefix(source, "\"") {
		return "", false
	}
	name, err := strconv.Unquote(source)
	if err != nil || name == "" {
		return "", false
	}
	return name, true
}
