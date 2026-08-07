package goc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// The cacheable unit is one function, not one package.
//
// Stage 1 asked whether a *package* could be cached and answered "yes, except
// for a package that declares a generic", because an instantiation is a
// function of package P that exists only because some importer asked for it
// (goc/reach.go, where a call in Q supplies the type arguments for a generic
// declared in P). At package granularity that is fatal: `slices.Sort[[]int,int]`
// is a member of the `slices` unit, so the unit's contents depend on the
// program, so the unit is not reusable.
//
// The measurement that followed (goc/genericshape.go and its census) showed what
// that accounting cost. Functions that actually ARE an instantiation are 2.15%
// of lowered functions on a small program, 4.13% on an http one and 5.06% on a
// probe written to maximise them. Functions merely living in a package that
// declares a generic somewhere are 75.7%, 48.7% and 89.3%. The 44-74% gap is
// ordinary non-generic code, and `runtime` is most of it: one package, roughly a
// third of the module, excluded whole because `internal/runtime/atomic.Pointer`
// and `runtime.AddCleanup` exist.
//
// So the unit here is a function. A function is cacheable when it is not itself
// an instantiation and was not synthesized from the assembled program; its
// package may declare as many generics as it likes. `goc/functionlowering_test.go`
// is the property that licenses this: two programs that make `runtime` carry
// *disjoint* instantiation sets still lower every one of `runtime`'s other
// functions to identical bytes.
//
// This file is the classification and the key. It is not a cache: nothing here
// stores or loads anything, because a stored unit needs the per-package slice of
// ir.Module that BUILD_CACHE.md §3.3 says the binary format does not have yet.
// What it does have to be is honest about what a key must cover, which is why
// [FunctionCacheEntry.Valid] checks its clauses one at a time and says which one
// failed, the way memo.Entry.Valid does.

// ---------------------------------------------------------------------------
// What may be cached
// ---------------------------------------------------------------------------

// CacheUnitReason says whether one lowered function may be held in a
// function-granular cache, and when it may not, why not.
type CacheUnitReason int

const (
	// CacheUnitCacheable is a function whose lowering is a pure function of its
	// own package's source and its dependencies' identities.
	CacheUnitCacheable CacheUnitReason = iota
	// CacheUnitInstantiation is a generic instantiation, or a closure or method
	// value derived from one. Its symbol names the type arguments, which the
	// *importing* program supplied, so which of these exist is a whole-program
	// fact. Each is still a well-defined unit in its own right -- keyed on origin
	// plus type arguments, which is exactly what genericInstanceSymbol already
	// writes into the name -- but it is not part of its package's unit.
	CacheUnitInstantiation
	// CacheUnitInterfaceCallWrapper is an interface-call wrapper. Its body is
	// chosen from the assembled module by redirectUnavailableInterfaceCallWrappers,
	// which points it at runtime.unreachableMethod when the program did not lower
	// the method it wraps. A cache must regenerate these, not hold them.
	CacheUnitInterfaceCallWrapper
	// CacheUnitInterfaceMethodDispatcher is an interface-method dispatcher, which
	// addInterfaceMethodWrappers synthesizes with a switch over every concrete
	// type in the *program* that implements the interface. Also regenerated.
	CacheUnitInterfaceMethodDispatcher
)

func (r CacheUnitReason) String() string {
	switch r {
	case CacheUnitCacheable:
		return "cacheable"
	case CacheUnitInstantiation:
		return "generic instantiation"
	case CacheUnitInterfaceCallWrapper:
		return "interface-call wrapper"
	case CacheUnitInterfaceMethodDispatcher:
		return "interface-method dispatcher"
	}
	return "unknown"
}

// Cacheable is the one-bit form of the reason.
func (r CacheUnitReason) Cacheable() bool { return r == CacheUnitCacheable }

// ClassifyCacheUnit reports whether a function-granular cache may hold one
// lowered function.
//
// The order matters. An instantiation is tested first because the two generated
// families are recognised by shape and an instantiation could in principle wear
// the same shape; naming the more specific reason first keeps the census's
// categories disjoint.
func ClassifyCacheUnit(function *ir.Func) CacheUnitReason {
	if IsGenericInstanceSymbol(function.Name) {
		return CacheUnitInstantiation
	}
	if _, isCallWrapper := interfaceCallWrapperMethodSymbol(function.Name); isCallWrapper {
		return CacheUnitInterfaceCallWrapper
	}
	if synthesizedInterfaceDispatcher(function) {
		return CacheUnitInterfaceMethodDispatcher
	}
	return CacheUnitCacheable
}

// IsGenericInstanceSymbol reports that a lowered symbol is an instantiation, or
// is derived from one.
//
// The compiler builds an instantiation's symbol in genericInstanceSymbol as
// `<origin>[<type arguments>]`, and derives closure, method-value and defer
// wrapper symbols from it by appending a dotted suffix. So the question is
// whether the name carries a type-argument list, and the answer has to be
// careful about two things that are not one:
//
//   - A type argument may itself contain brackets. `slices.SortFunc[
//     []*main.tracked,*main.tracked]` nests one slice inside the argument list,
//     so the scan tracks bracket depth rather than stopping at the first `]`.
//   - A symbol may contain brackets that are NOT a type-argument list. An
//     interface-call wrapper is named after the interface type it dispatches on
//     and that type may be written out in full:
//     `errors.interface{Unwrap() []error}.Unwrap` has a `[]` inside a method
//     signature inside braces. Scanning at brace depth 0 only is what keeps that
//     from being read as an instantiation.
//
// The remaining direction is deliberate: where this is unsure it says "yes, an
// instantiation", because that excludes the function from the cache, and a
// function wrongly excluded is a slower build while a function wrongly included
// is a wrong binary. goc/functioncache_test.go holds the predicate against the
// compiler's own answer -- the type arguments reachableFunctions recorded -- on a
// real program, so the conservative direction is measured and not merely hoped
// for.
func IsGenericInstanceSymbol(symbol string) bool {
	_, _, ok := genericInstanceOrigin(symbol)
	return ok
}

// genericInstanceOrigin splits an instantiation symbol into the origin symbol
// and the rendered type-argument list. See [IsGenericInstanceSymbol].
func genericInstanceOrigin(symbol string) (origin, arguments string, ok bool) {
	braces := 0
	for index := 0; index < len(symbol); index++ {
		switch symbol[index] {
		case '{':
			braces++
		case '}':
			if braces > 0 {
				braces--
			}
		case '[':
			if braces > 0 {
				continue
			}
			end, closed := matchingBracket(symbol, index)
			if !closed {
				return "", "", false
			}
			// The list must end the symbol or be followed by a derived suffix,
			// which the compiler always introduces with a dot. Anything else is
			// not a shape genericInstanceSymbol produces.
			if end+1 != len(symbol) && symbol[end+1] != '.' {
				continue
			}
			return symbol[:index], symbol[index+1 : end], true
		}
	}
	return "", "", false
}

// matchingBracket returns the index of the `]` that closes the `[` at open.
func matchingBracket(symbol string, open int) (int, bool) {
	depth := 0
	braces := 0
	for index := open; index < len(symbol); index++ {
		switch symbol[index] {
		case '{':
			braces++
		case '}':
			if braces > 0 {
				braces--
			}
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return 0, false
}

// synthesizedInterfaceDispatcher recognises the interface-method dispatchers
// addInterfaceMethodWrappers builds from the assembled module: a body with no
// source position anywhere that ends by calling the dispatch-failure helper (or
// `abort`, in the freestanding subset) and halting.
//
// The position test is required and not decoration. runtime.goroutineReady is an
// ordinary lowered function that also ends in that shape, because a failed type
// assertion lowers to it; without the position test a cache would refuse to hold
// a function it may perfectly well hold.
func synthesizedInterfaceDispatcher(function *ir.Func) bool {
	halts := false
	for _, block := range function.Blocks {
		if block.Pos.Valid() {
			return false
		}
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Pos.Valid() {
				return false
			}
			if instruction.Op != ir.OCall || len(instruction.Args) == 0 || block.Jmp.Kind != ir.JmpHlt {
				continue
			}
			callee := instruction.Arg(0)
			if callee.Kind != ir.RefConst {
				continue
			}
			constant := function.Consts[callee.ID]
			if constant.Kind == ir.ConstSym &&
				(constant.Sym == "runtime_gocInterfaceDispatchFailure" || constant.Sym == "abort") {
				halts = true
			}
		}
	}
	return halts
}

// ---------------------------------------------------------------------------
// A census of one module, at function granularity
// ---------------------------------------------------------------------------

// FunctionCacheCensus is the function-granular answer to "how much of this
// program may a cache hold", against the same denominator Stage 1's
// package-granular figure used: the functions the front end lowered.
type FunctionCacheCensus struct {
	// Lowered is every function the module carried out of the front end, which
	// is the denominator.
	Lowered int
	// Cacheable, Instantiations, InterfaceCallWrappers and
	// InterfaceMethodDispatchers partition it.
	Cacheable                  int
	Instantiations             int
	InterfaceCallWrappers      int
	InterfaceMethodDispatchers int
	// ByPackage counts the same partition per import path, attributed by
	// [PackageOfFunction]. The program's own package is included under its own
	// path; a caller measuring what a *library* cache could hold should subtract
	// it, because the root package changes every build by definition.
	ByPackage map[string]*PackageFunctionCounts

	// The same partition weighted by IR instructions rather than by function.
	//
	// Counting functions answers "how many units may a cache hold", which is what
	// the classification is about. It does not answer "how much work does a hit
	// save", because the excluded families are not average-sized: an
	// interface-call wrapper is a handful of instructions that loads a receiver
	// and tail-calls, and there are thousands of them. Reporting only the
	// function count would make the exclusion look far more expensive than it is,
	// so both are measured.
	LoweredInstructions                   int
	CacheableInstructions                 int
	InstantiationInstructions             int
	InterfaceCallWrapperInstructions      int
	InterfaceMethodDispatcherInstructions int
}

// PackageFunctionCounts is one package's row of a [FunctionCacheCensus].
type PackageFunctionCounts struct {
	Path                       string
	Lowered                    int
	Cacheable                  int
	Instantiations             int
	InterfaceCallWrappers      int
	InterfaceMethodDispatchers int
}

// CacheableShare is the share of lowered functions a function-granular cache may
// hold, as a fraction.
func (c FunctionCacheCensus) CacheableShare() float64 {
	if c.Lowered == 0 {
		return 0
	}
	return float64(c.Cacheable) / float64(c.Lowered)
}

// CacheableInstructionShare is CacheableShare weighted by IR instructions.
func (c FunctionCacheCensus) CacheableInstructionShare() float64 {
	if c.LoweredInstructions == 0 {
		return 0
	}
	return float64(c.CacheableInstructions) / float64(c.LoweredInstructions)
}

// CensusFunctionCache classifies every function of a lowered module.
//
// paths is the set of import paths function names may be attributed to; pass
// [ModuleImportPaths] of the same module when the loader's list is not to hand.
func CensusFunctionCache(module *ir.Module, paths []string) FunctionCacheCensus {
	census := FunctionCacheCensus{ByPackage: map[string]*PackageFunctionCounts{}}
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		census.Lowered++
		path := PackageOfFunction(function.Name, paths)
		row := census.ByPackage[path]
		if row == nil {
			row = &PackageFunctionCounts{Path: path}
			census.ByPackage[path] = row
		}
		row.Lowered++
		instructions := loweredInstructionCount(function)
		census.LoweredInstructions += instructions
		switch ClassifyCacheUnit(function) {
		case CacheUnitCacheable:
			census.Cacheable++
			row.Cacheable++
			census.CacheableInstructions += instructions
		case CacheUnitInstantiation:
			census.Instantiations++
			row.Instantiations++
			census.InstantiationInstructions += instructions
		case CacheUnitInterfaceCallWrapper:
			census.InterfaceCallWrappers++
			row.InterfaceCallWrappers++
			census.InterfaceCallWrapperInstructions += instructions
		case CacheUnitInterfaceMethodDispatcher:
			census.InterfaceMethodDispatchers++
			row.InterfaceMethodDispatchers++
			census.InterfaceMethodDispatcherInstructions += instructions
		}
	}
	return census
}

func loweredInstructionCount(function *ir.Func) int {
	total := 0
	for _, block := range function.Blocks {
		total += len(block.Instrs)
	}
	return total
}

// PackageOfFunction attributes a lowered symbol to the import path that declared
// it. goc names a function `<import path>.<rest>` and an import path may itself
// contain dots, so the attribution is by longest match against the paths the
// compile loaded rather than by splitting on the first dot. A name that matches
// nothing -- the compiler's own `.goc.` symbols -- comes back as "".
func PackageOfFunction(name string, paths []string) string {
	best := ""
	for _, path := range paths {
		if len(path) > len(best) && strings.HasPrefix(name, path+".") {
			best = path
		}
	}
	return best
}

// ModuleImportPaths recovers the plausible import paths of a finished module
// from its symbol names, for a caller that has a module but not the loader that
// produced it.
func ModuleImportPaths(modules ...*ir.Module) []string {
	candidates := make(map[string]bool)
	for _, module := range modules {
		for _, function := range module.Funcs {
			name := function.Name
			if strings.HasPrefix(name, ".goc.") {
				continue
			}
			for index := len(name) - 1; index > 0; index-- {
				if name[index] == '.' {
					candidates[name[:index]] = true
				}
			}
		}
	}
	paths := make([]string, 0, len(candidates))
	for candidate := range candidates {
		paths = append(paths, candidate)
	}
	sort.Strings(paths)
	shortest := paths[:0:0]
	for _, candidate := range paths {
		covered := false
		for _, kept := range shortest {
			if strings.HasPrefix(candidate, kept+".") {
				covered = true
				break
			}
		}
		if !covered {
			shortest = append(shortest, candidate)
		}
	}
	return shortest
}

// ---------------------------------------------------------------------------
// The key
// ---------------------------------------------------------------------------

// FunctionCacheUnitVersion is BUILD_CACHE.md §3.2 clause 1: the format version
// of a cached unit, so that changing the shape of what is stored is a miss and
// not a crash. Bump it whenever the meaning of any clause below changes.
const FunctionCacheUnitVersion = 1

// CacheDigest is a content digest of one clause of a key.
type CacheDigest [32]byte

func (d CacheDigest) String() string { return hex.EncodeToString(d[:8]) }

// PackageIdentity is what a cache knows about one loaded package.
type PackageIdentity struct {
	// Path is the import path.
	Path string
	// Source is a digest of the package's own inputs and nothing else: every Go
	// file the loader parsed for it by trimmed path and content, every assembly
	// file and its includes, every native cg12 overlay file, and every overlay
	// decision that applied to it -- including a deletion, which changes the
	// package without changing the content of any file that remains.
	Source CacheDigest
	// Transitive is BUILD_CACHE.md §3.2 clause 4, the clause that makes the
	// scheme sound under cross-package inlining: this package's Source folded
	// with the Transitive identity of every package it imports. Two compiles that
	// agree on it agree on every byte of source that can reach this package.
	Transitive CacheDigest
	// Imports are the direct imports, sorted.
	Imports []string
	// FromSource records that the loader compiled this package from source. A
	// package resolved from export data alone contributes no lowered function, so
	// it has no unit of its own, but it still takes part in its importers' keys.
	FromSource bool
}

// CompileIdentity is everything about one compile that a cached function's key
// has to agree with. It is what a real cache would compute once per build and
// then check every candidate entry against.
type CompileIdentity struct {
	// Target is the resolved goc target.
	Target string
	// Optimize is -O. It is not derivable from anything else here: goc's library
	// entry points lower without optimising and the caller decides, so the caller
	// has to say.
	Optimize bool
	// TextLayout is arm64.TextLayoutIdentity(), the code placement policy, which
	// the environment can change without changing a byte of the compiler.
	TextLayout string
	// Pipeline is opt.PipelineIdentity(), for the same reason: GOC_OPT_PIPELINE
	// and GOC_OPT_SKIP select which passes run.
	Pipeline string
	// Compiler is the hash of the running binary.
	Compiler CacheDigest
	// Packages is every loaded package by import path.
	Packages map[string]PackageIdentity
}

// PackageDependency is one member of a cached function's recorded dependency
// set: an import path and the transitive source identity it had when the entry
// was written.
type PackageDependency struct {
	Path       string
	Transitive CacheDigest
}

// FunctionCacheEntry is one cached function's key. The body is not here; this
// stage does not store one.
//
// Every clause of BUILD_CACHE.md §3.2 appears as its own field rather than being
// folded into a single hash, because [FunctionCacheEntry.Valid] has to be able
// to say which one moved. That is not a nicety: the memoiser learned it the
// expensive way, where one over-broad clause invalidated 4607 of 4608 entries on
// a one-character edit and the only way to find out was that the reason was
// reported per clause.
type FunctionCacheEntry struct {
	// Name is the lowered symbol.
	Name string
	// Package is the import path that declared it.
	Package string
	// UnitVersion is [FunctionCacheUnitVersion] as of writing.
	UnitVersion int
	// Source is the owning package's own source identity.
	Source CacheDigest
	// Target, Optimize, TextLayout, Pipeline and Compiler are the compile-wide
	// clauses, held individually for the same reason as the rest.
	Target     string
	Optimize   bool
	TextLayout string
	Pipeline   string
	Compiler   CacheDigest
	// Deps is the transitive source identity of every package the owning package
	// imports, recorded at write time.
	Deps []PackageDependency
}

// NewFunctionCacheEntry writes the key for one function against the identity of
// the compile that produced it.
func NewFunctionCacheEntry(name, packagePath string, identity *CompileIdentity) (*FunctionCacheEntry, error) {
	owner, ok := identity.Packages[packagePath]
	if !ok {
		return nil, fmt.Errorf("goc: no identity for package %q", packagePath)
	}
	entry := &FunctionCacheEntry{
		Name:        name,
		Package:     packagePath,
		UnitVersion: FunctionCacheUnitVersion,
		Source:      owner.Source,
		Target:      identity.Target,
		Optimize:    identity.Optimize,
		TextLayout:  identity.TextLayout,
		Pipeline:    identity.Pipeline,
		Compiler:    identity.Compiler,
	}
	for _, path := range owner.Imports {
		dependency, ok := identity.Packages[path]
		if !ok {
			return nil, fmt.Errorf("goc: package %q imports %q, which has no identity", packagePath, path)
		}
		entry.Deps = append(entry.Deps, PackageDependency{Path: path, Transitive: dependency.Transitive})
	}
	return entry, nil
}

// Valid reports whether a recorded entry may be used against the current
// compile, and says which clause failed when it may not.
//
// This is memo.Entry.Valid's shape, and deliberately so: the correctness
// argument in executable form, one clause at a time, rather than a single opaque
// comparison that can only ever answer "no".
func (e *FunctionCacheEntry) Valid(identity *CompileIdentity) (bool, string) {
	if e.UnitVersion != FunctionCacheUnitVersion {
		return false, "unit format version moved"
	}
	if e.Target != identity.Target {
		return false, "target moved"
	}
	if e.Optimize != identity.Optimize {
		return false, "-O moved"
	}
	if e.TextLayout != identity.TextLayout {
		return false, "text layout policy moved"
	}
	if e.Pipeline != identity.Pipeline {
		return false, "optimiser pipeline moved"
	}
	if e.Compiler != identity.Compiler {
		return false, "compiler binary moved"
	}
	owner, ok := identity.Packages[e.Package]
	if !ok {
		return false, "package " + e.Package + " is not loaded"
	}
	if e.Source != owner.Source {
		return false, "package " + e.Package + " source moved"
	}
	if len(e.Deps) != len(owner.Imports) {
		return false, "package " + e.Package + " changed its import set"
	}
	for index, dependency := range e.Deps {
		if owner.Imports[index] != dependency.Path {
			return false, "package " + e.Package + " changed its import set"
		}
		current, ok := identity.Packages[dependency.Path]
		if !ok {
			return false, "dependency " + dependency.Path + " left the compile"
		}
		if current.Transitive != dependency.Transitive {
			return false, "dependency " + dependency.Path + " moved"
		}
	}
	return true, ""
}

// ---------------------------------------------------------------------------
// Computing a CompileIdentity
// ---------------------------------------------------------------------------

// ProgramCompileIdentity loads the closure of one program exactly as a compile
// of it would, and computes the identity every cached function of that closure
// would be keyed against.
//
// It does not lower anything: the key is a function of the source the loader
// read and of the compile's own configuration, both of which are known once the
// closure is type-checked. That is the point -- a cache has to be able to decide
// a hit before it does the work the hit would save.
func ProgramCompileIdentity(target Target, optimize bool, name string, src []byte) (*CompileIdentity, error) {
	resolved := target.resolve()
	if err := checkTargetTypeSizes(resolved); err != nil {
		return nil, err
	}
	world := sharedSourceWorld(resolved, false)
	fset := token.NewFileSet()
	if world != nil {
		fset = world.fset
	}
	file, err := parser.ParseFile(fset, name, src, parser.AllErrors|parser.ParseComments)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if _, err := loader.Import("runtime"); err != nil {
		return nil, err
	}

	compiler, err := compilerBinaryDigest()
	if err != nil {
		return nil, err
	}
	identity := &CompileIdentity{
		Target:     string(resolved),
		Optimize:   optimize,
		TextLayout: arm64.TextLayoutIdentity(),
		Pipeline:   opt.PipelineIdentity(),
		Compiler:   compiler,
		Packages:   map[string]PackageIdentity{},
	}
	if err := loaderPackageIdentities(loader, fset, identity.Packages); err != nil {
		return nil, err
	}
	// The program's own package is a unit like any other; a cache would never get
	// a hit on it, but the key has to exist for the functions it declares.
	if err := rootPackageIdentity(pkg, name, src, identity.Packages); err != nil {
		return nil, err
	}
	return identity, nil
}

// loaderPackageIdentities fills in one identity per source-loaded package, then
// closes the transitive clause over the import graph.
func loaderPackageIdentities(loader *sourceLoader, fset *token.FileSet, into map[string]PackageIdentity) error {
	for path, unit := range loader.units {
		if unit == nil {
			continue
		}
		source, err := unitSourceDigest(path, unit, fset)
		if err != nil {
			return err
		}
		into[path] = PackageIdentity{
			Path:       path,
			Source:     source,
			Imports:    importPathsOf(unit.pkg),
			FromSource: true,
		}
	}
	// A package the loader resolved from export data rather than source has no
	// files here, but its importers' keys still have to name it. Give it an
	// identity derived from its path alone and record that it did not come from
	// source, so a reader can see the difference rather than guess at it.
	for _, identity := range copyIdentities(into) {
		for _, path := range identity.Imports {
			if _, known := into[path]; known {
				continue
			}
			into[path] = PackageIdentity{
				Path:   path,
				Source: digestOf("export-data-only\x00" + path + "\x00"),
			}
		}
	}
	return closeTransitiveIdentities(into)
}

// rootPackageIdentity gives the program's own package the same treatment.
//
// Its source is hashed from the bytes the caller handed in rather than read back
// from the named file, because the program being compiled need not be a file at
// all -- goc's library entry points take a name and a []byte, and every test in
// this package passes a string literal.
func rootPackageIdentity(pkg *types.Package, name string, src []byte, into map[string]PackageIdentity) error {
	digest := sha256.New()
	fmt.Fprintf(digest, "package\x00%s\x00", pkg.Path())
	fmt.Fprintf(digest, "file\x00%s\x00%x\x00", TrimPath(name), sha256.Sum256(src))
	var source CacheDigest
	copy(source[:], digest.Sum(nil))
	into[pkg.Path()] = PackageIdentity{
		Path:       pkg.Path(),
		Source:     source,
		Imports:    importPathsOf(pkg),
		FromSource: true,
	}
	return closeTransitiveIdentities(into)
}

// unitSourceDigest is clause 3 of BUILD_CACHE.md §3.2 for one package: every
// input the loader read for it, by trimmed path and by content.
func unitSourceDigest(path string, unit *sourceUnit, fset *token.FileSet) (CacheDigest, error) {
	digest := sha256.New()
	fmt.Fprintf(digest, "package\x00%s\x00", path)
	if err := hashParsedFiles(digest, fset, unit.files); err != nil {
		return CacheDigest{}, err
	}
	for _, assembly := range unit.assembly {
		fmt.Fprintf(digest, "asm\x00%s\x00%x\x00", assembly.path, sha256.Sum256([]byte(assembly.source)))
		names := make([]string, 0, len(assembly.includes))
		for name := range assembly.includes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(digest, "asminclude\x00%s\x00%x\x00", name, sha256.Sum256([]byte(assembly.includes[name])))
		}
	}
	for _, native := range unit.native {
		fmt.Fprintf(digest, "native\x00%s\x00%x\x00", TrimPath(native.path), sha256.Sum256([]byte(native.source)))
	}
	// The overlay decisions, including deletions. A deleted file leaves no parsed
	// file behind, so without this clause removing one from the manifest would not
	// move the key.
	for _, use := range unit.overlays {
		fmt.Fprintf(digest, "overlay\x00%s\x00%s\x00%s\x00", use.Path, use.Mode, TrimPath(use.File))
	}
	var out CacheDigest
	copy(out[:], digest.Sum(nil))
	return out, nil
}

// hashParsedFiles folds each parsed file into digest by trimmed origin path and
// content, in the order the loader parsed them -- which is the order the front
// end will walk them in, so it is part of what the package is.
//
// The content is read back from the origin rather than kept from the parse. The
// loader releases the bytes once the AST exists, and holding a copy of every
// source file of the closure to compute a key would cost more than the key can
// ever save. A file edited between the parse and this read would be hashed in its
// new form, which makes the key too *new* rather than too old: the entry written
// under it is refused by the next compile, which is the safe direction.
func hashParsedFiles(digest interface{ Write([]byte) (int, error) }, fset *token.FileSet, files []*ast.File) error {
	for _, file := range files {
		position := fset.Position(file.Package)
		content, err := os.ReadFile(position.Filename)
		if err != nil {
			return fmt.Errorf("hash source of %s: %w", position.Filename, err)
		}
		fmt.Fprintf(digest, "file\x00%s\x00%x\x00", TrimPath(position.Filename), sha256.Sum256(content))
	}
	return nil
}

// closeTransitiveIdentities computes each package's Transitive clause: its own
// Source folded with the Transitive identity of everything it imports.
//
// Go forbids import cycles, so a depth-first fold terminates; the visiting set is
// kept anyway, because a cycle here would otherwise be a hang rather than an
// error and the loader is not the only thing that can put entries in this map.
func closeTransitiveIdentities(identities map[string]PackageIdentity) error {
	resolved := make(map[string]CacheDigest, len(identities))
	visiting := make(map[string]bool, len(identities))
	var fold func(path string) (CacheDigest, error)
	fold = func(path string) (CacheDigest, error) {
		if done, ok := resolved[path]; ok {
			return done, nil
		}
		if visiting[path] {
			return CacheDigest{}, fmt.Errorf("goc: import cycle through %q", path)
		}
		visiting[path] = true
		defer delete(visiting, path)

		identity := identities[path]
		digest := sha256.New()
		fmt.Fprintf(digest, "unit\x00%d\x00%s\x00%x\x00", FunctionCacheUnitVersion, path, identity.Source)
		for _, imported := range identity.Imports {
			inner, err := fold(imported)
			if err != nil {
				return CacheDigest{}, err
			}
			fmt.Fprintf(digest, "import\x00%s\x00%x\x00", imported, inner)
		}
		var out CacheDigest
		copy(out[:], digest.Sum(nil))
		resolved[path] = out
		return out, nil
	}
	paths := make([]string, 0, len(identities))
	for path := range identities {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		transitive, err := fold(path)
		if err != nil {
			return err
		}
		identity := identities[path]
		identity.Transitive = transitive
		identities[path] = identity
	}
	return nil
}

func importPathsOf(pkg *types.Package) []string {
	if pkg == nil {
		return nil
	}
	imported := pkg.Imports()
	paths := make([]string, 0, len(imported))
	for _, each := range imported {
		paths = append(paths, each.Path())
	}
	sort.Strings(paths)
	return paths
}

func copyIdentities(from map[string]PackageIdentity) []PackageIdentity {
	out := make([]PackageIdentity, 0, len(from))
	for _, identity := range from {
		out = append(out, identity)
	}
	return out
}

func digestOf(text string) CacheDigest {
	return CacheDigest(sha256.Sum256([]byte(text)))
}

// compilerBinaryDigest is BUILD_CACHE.md §3.2 clause 9, and it is the same
// clause cmd/goc/packcache.go already writes with os.Executable: the bytes of
// the binary doing the compiling, which covers every compiler change without
// anyone having to remember to bump a version.
//
// Hashed once per process. It is a hundred megabytes of test binary in a test
// run, and a key that costs a hundred megabytes of I/O per function would be a
// key nobody could afford.
var compilerBinaryDigest = sync.OnceValues(func() (CacheDigest, error) {
	path, err := os.Executable()
	if err != nil {
		return CacheDigest{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return CacheDigest{}, err
	}
	return CacheDigest(sha256.Sum256(content)), nil
})
