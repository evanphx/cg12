package goc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"sort"
)

// PackageCacheEligibility classifies the packages of one program's loaded
// closure by whether a per-package cache may hold them.
//
// The unit a per-package cache holds is the IR for every function of one
// package, keyed on that package's source and its dependencies' units. That is
// only a well-defined thing to build if "which functions are in package P" is a
// property of P. For a package that declares generics it is not.
type PackageCacheEligibility struct {
	// Closure is every import path the program loaded, sorted, excluding the
	// program's own package.
	Closure []string
	// Generic is the subset of Closure that declares a generic function or a
	// generic type, and is therefore not cacheable at this stage.
	Generic []string
}

// Cacheable is Closure minus Generic.
func (e PackageCacheEligibility) Cacheable() []string {
	generic := make(map[string]bool, len(e.Generic))
	for _, path := range e.Generic {
		generic[path] = true
	}
	cacheable := make([]string, 0, len(e.Closure))
	for _, path := range e.Closure {
		if !generic[path] {
			cacheable = append(cacheable, path)
		}
	}
	return cacheable
}

// ClassifyPackageCacheEligibility loads the closure of one program exactly as a
// compile of it would, and reports which of its packages a per-package cache
// may hold.
//
// # Why a package that declares generics is excluded
//
// goc has no separate compilation of a generic function. An instantiation is
// discovered by the whole-program reachability walk, with the type arguments
// supplied by the *importer* (goc/reach.go, where a call in package Q supplies
// the arguments for an instantiation of a function declared in P), and the
// instantiation is then lowered into the module under a symbol that names those
// arguments. So `slices.Sort[[]int,int]` is a function *of package slices* that
// exists only because some other package asked for it, and a second program
// that asks for `slices.Sort[[]string,string]` instead needs a different set of
// functions from the same package.
//
// A cached unit for `slices` would therefore have to contain either every
// instantiation any program might ever ask for -- unbounded -- or the set one
// program asked for, which puts a whole-program fact in the key and makes the
// unit unreusable. Neither is this stage's problem to solve: Stage 1's job is to
// know which packages the question arises for, and it is exactly the packages
// that declare a generic function or a generic type.
//
// The test is syntactic and deliberately coarse: any `func F[T any]`, any
// `type S[T any]`. A method on a generic type is covered by its type's
// declaration. A package that merely *uses* generics declared elsewhere is not
// excluded -- the instantiation is a function of the package that declares it,
// not of the one that calls it.
func ClassifyPackageCacheEligibility(target Target, name string, src []byte) (PackageCacheEligibility, error) {
	resolved := target.resolve()
	if err := checkTargetTypeSizes(resolved); err != nil {
		return PackageCacheEligibility{}, err
	}
	world := sharedSourceWorld(resolved, false)
	fset := token.NewFileSet()
	if world != nil {
		fset = world.fset
	}
	file, err := parser.ParseFile(fset, name, src, parser.AllErrors|parser.ParseComments)
	if err != nil {
		return PackageCacheEligibility{}, err
	}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Implicits:  make(map[ast.Node]types.Object),
		Instances:  make(map[*ast.Ident]types.Instance),
	}
	loader := newSourceLoader(fset, resolved)
	if world != nil {
		world.adopt(loader)
	}
	conf := types.Config{Importer: loader}
	pkg, err := conf.Check(file.Name.Name, fset, []*ast.File{file}, info)
	if err != nil {
		return PackageCacheEligibility{}, err
	}
	if _, err := loader.Import("runtime"); err != nil {
		return PackageCacheEligibility{}, err
	}

	eligibility := PackageCacheEligibility{Closure: loadedPackagePaths(loader, pkg)}
	for _, path := range eligibility.Closure {
		unit := loader.units[path]
		if unit != nil && filesDeclareGenerics(unit.files) {
			eligibility.Generic = append(eligibility.Generic, path)
		}
	}
	sort.Strings(eligibility.Generic)
	return eligibility, nil
}

// filesDeclareGenerics reports whether any file declares a generic function or
// a generic type.
func filesDeclareGenerics(files []*ast.File) bool {
	for _, file := range files {
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Type.TypeParams != nil && len(declaration.Type.TypeParams.List) > 0 {
					return true
				}
			case *ast.GenDecl:
				if declaration.Tok != token.TYPE {
					continue
				}
				for _, specification := range declaration.Specs {
					typeSpec, isType := specification.(*ast.TypeSpec)
					if isType && typeSpec.TypeParams != nil && len(typeSpec.TypeParams.List) > 0 {
						return true
					}
				}
			}
		}
	}
	return false
}
