package goc

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"runtime"
)

// sourceUnit is the unchanged, build-selected source for an imported package.
type sourceUnit struct {
	path  string
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

// sourceLoader imports selected packages from source while retaining their AST
// for cg12 lowering. Dependencies use export data until they too are selected
// for source compilation.
type sourceLoader struct {
	fset    *token.FileSet
	units   map[string]*sourceUnit
	loading map[string]bool
	sources map[string]bool
	base    types.Importer
	root    string
}

func newSourceLoader(fset *token.FileSet) *sourceLoader {
	return &sourceLoader{
		fset:    fset,
		units:   make(map[string]*sourceUnit),
		loading: make(map[string]bool),
		sources: map[string]bool{
			"crypto/sha256":                         true,
			"crypto/internal/fips140":               true,
			"crypto/internal/fips140/sha256":        true,
			"crypto/internal/fips140/sha512":        true,
			"crypto/internal/fips140deps/byteorder": true,
			"crypto/md5":                            true,
			"crypto/sha1":                           true,
			"crypto/sha512":                         true,
			"encoding/binary":                       true,
			"encoding/hex":                          true,
			"hash/adler32":                          true,
			"hash/crc32":                            true,
			"hash/fnv":                              true,
			"internal/byteorder":                    true,
			"math/bits":                             true,
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
	ctx.BuildTags = append(append([]string{}, ctx.BuildTags...), "purego")
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
		},
	}
	for _, name := range bp.GoFiles {
		full := filepath.Join(bp.Dir, name)
		file, err := parser.ParseFile(l.fset, full, nil, parser.ParseComments|parser.AllErrors)
		if err != nil {
			return nil, err
		}
		u.files = append(u.files, file)
	}
	conf := types.Config{Importer: l}
	u.pkg, err = conf.Check(path, l.fset, u.files, u.info)
	if err != nil {
		return nil, fmt.Errorf("type-check %s: %w", path, err)
	}
	l.units[path] = u
	return u.pkg, nil
}
