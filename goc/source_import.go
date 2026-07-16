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

// sourceUnit is the unchanged, build-selected source for an imported package.
type sourceUnit struct {
	path     string
	files    []*ast.File
	assembly []sourceAssemblyFile
	info     *types.Info
	pkg      *types.Package
}

type sourceAssemblyFile struct {
	path     string
	source   string
	includes map[string]string
}

// sourceLoader imports selected packages from source while retaining their AST
// for cg12 lowering. Dependencies use export data until they too are selected
// for source compilation.
type sourceLoader struct {
	fset         *token.FileSet
	units        map[string]*sourceUnit
	loading      map[string]bool
	sources      map[string]bool
	testPackages map[string]bool
	base         types.Importer
	root         string
	forcePureGo  bool
}

func newSourceLoader(fset *token.FileSet) *sourceLoader {
	return &sourceLoader{
		fset:         fset,
		units:        make(map[string]*sourceUnit),
		loading:      make(map[string]bool),
		testPackages: make(map[string]bool),
		sources: map[string]bool{
			"bufio":                                 true,
			"bytes":                                 true,
			"cmp":                                   true,
			"container/heap":                        true,
			"container/list":                        true,
			"container/ring":                        true,
			"context":                               true,
			"crypto":                                true,
			"encoding":                              true,
			"errors":                                true,
			"flag":                                  true,
			"fmt":                                   true,
			"io":                                    true,
			"io/fs":                                 true,
			"crypto/sha256":                         true,
			"crypto/internal/fips140":               true,
			"crypto/internal/fips140/sha256":        true,
			"crypto/internal/fips140/sha512":        true,
			"crypto/internal/fips140deps/byteorder": true,
			"crypto/internal/fips140deps/cpu":       true,
			"crypto/internal/fips140deps/godebug":   true,
			"crypto/internal/fips140deps/time":      true,
			"crypto/internal/impl":                  true,
			"crypto/md5":                            true,
			"crypto/sha1":                           true,
			"crypto/sha512":                         true,
			"encoding/ascii85":                      true,
			"encoding/base32":                       true,
			"encoding/base64":                       true,
			"encoding/binary":                       true,
			"encoding/csv":                          true,
			"encoding/hex":                          true,
			"hash/adler32":                          true,
			"hash/crc32":                            true,
			"hash/fnv":                              true,
			"internal/abi":                          true,
			"internal/asan":                         true,
			"internal/bytealg":                      true,
			"internal/bisect":                       true,
			"internal/byteorder":                    true,
			"internal/chacha8rand":                  true,
			"internal/coverage/rtcov":               true,
			"internal/cpu":                          true,
			"internal/filepathlite":                 true,
			"internal/fmtsort":                      true,
			"internal/goarch":                       true,
			"internal/godebug":                      true,
			"internal/godebugs":                     true,
			"internal/goexperiment":                 true,
			"internal/goos":                         true,
			"internal/msan":                         true,
			"internal/oserror":                      true,
			"internal/poll":                         true,
			"internal/profilerecord":                true,
			"internal/race":                         true,
			"internal/reflectlite":                  true,
			"internal/runtime/atomic":               true,
			"internal/runtime/cgroup":               true,
			"internal/runtime/exithook":             true,
			"internal/runtime/gc":                   true,
			"internal/runtime/gc/scan":              true,
			"internal/runtime/maps":                 true,
			"internal/runtime/math":                 true,
			"internal/runtime/pprof/label":          true,
			"internal/runtime/sys":                  true,
			"internal/runtime/syscall/linux":        true,
			"internal/strconv":                      true,
			"internal/stringslite":                  true,
			"internal/sync":                         true,
			"internal/synctest":                     true,
			"internal/sysinfo":                      true,
			"internal/syscall/execenv":              true,
			"internal/syscall/unix":                 true,
			"internal/testlog":                      true,
			"internal/trace/tracev2":                true,
			"internal/unsafeheader":                 true,
			"iter":                                  true,
			"maps":                                  true,
			"math":                                  true,
			"math/bits":                             true,
			"math/rand":                             true,
			"os":                                    true,
			"path":                                  true,
			"path/filepath":                         true,
			"reflect":                               true,
			"regexp":                                true,
			"regexp/syntax":                         true,
			"runtime":                               true,
			"runtime/debug":                         true,
			"runtime/trace":                         true,
			"slices":                                true,
			"sort":                                  true,
			"strconv":                               true,
			"strings":                               true,
			"sync":                                  true,
			"sync/atomic":                           true,
			"syscall":                               true,
			"testing":                               true,
			"time":                                  true,
			"unicode":                               true,
			"unicode/utf8":                          true,
			"unicode/utf16":                         true,
		},
		base: importer.Default(),
		root: repositoryStdlibRoot(),
	}
}

func repositoryStdlibRoot() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("goc: cannot locate repository standard library")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "stdlib"))
}

func (l *sourceLoader) Import(path string) (*types.Package, error) {
	if u := l.units[path]; u != nil {
		return u.pkg, nil
	}
	if !l.sources[path] {
		return l.base.Import(path)
	}
	if l.loading[path] {
		return nil, fmt.Errorf("source import cycle involving %s", path)
	}
	l.loading[path] = true
	defer delete(l.loading, path)
	ctx := build.Default
	ctx.BuildTags = append([]string{}, ctx.BuildTags...)
	useAssembly := !l.forcePureGo && runtime.GOARCH == "arm64" && plan9asm.SupportsARM64Package(path)
	if !useAssembly {
		ctx.BuildTags = append(ctx.BuildTags, "purego")
	}
	ctx.GOROOT = l.root
	bp, err := ctx.Import(path, "", 0)
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
	goFiles := append([]string(nil), bp.GoFiles...)
	if l.testPackages[path] {
		goFiles = append(goFiles, bp.TestGoFiles...)
	}
	for _, name := range goFiles {
		full := filepath.Join(bp.Dir, name)
		file, err := parser.ParseFile(l.fset, full, nil, parser.ParseComments|parser.AllErrors)
		if err != nil {
			return nil, err
		}
		u.files = append(u.files, file)
	}
	for _, name := range bp.SFiles {
		if !useAssembly {
			continue
		}
		if !plan9asm.SupportsARM64File(path, name) {
			return nil, fmt.Errorf("translate %s: unsupported build-selected assembly file %s", path, name)
		}
		full := filepath.Join(bp.Dir, name)
		source, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		u.assembly = append(u.assembly, sourceAssemblyFile{
			path:     filepath.ToSlash(filepath.Join(path, name)),
			source:   string(source),
			includes: l.assemblyIncludes(bp.Dir, string(source)),
		})
	}
	conf := types.Config{Importer: l}
	u.pkg, err = conf.Check(path, l.fset, u.files, u.info)
	if err != nil {
		return nil, fmt.Errorf("type-check %s: %w", path, err)
	}
	l.units[path] = u
	return u.pkg, nil
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
