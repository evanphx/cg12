package main

import (
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// The compile queue dispatches the expensive programs first, and to do that it
// has to guess what a program will cost before compiling it.
//
// The guess is the total size of the Go and assembly sources in the program's
// transitive import closure, counting each package once, resolved against the
// vendored standard library that goc actually compiles. Two facts make that the
// right shape of guess:
//
//   - The capability sources themselves say nothing. Their sizes cluster around
//     575 bytes, and the most expensive program in the matrix
//     (stdlib_http_tls_client_server.go, 167 s) is 1303 bytes against a 6553-byte
//     maximum. Ranking by file size ranks these programs at random.
//
//   - The cost is the import closure. Compile time is sharply bimodal: eleven
//     net/http, net/smtp and crypto programs cost 125-167 s each and account for
//     54% of the matrix's compile CPU, while the other 327 average 4.2 s. What
//     separates them is how much standard library they pull in, and this measure
//     picks out exactly those eleven as its eleven largest.
//
// A wrong guess costs wall clock and nothing else -- the queue compiles the same
// set of programs whatever order it picks -- so the model resolves what it can
// and treats anything it cannot resolve as free.
type runtimeCapabilityCostModel struct {
	context      build.Context
	stdlibRoot   string
	testdataRoot string

	// mutex guards the memo tables. The queue builds its order on one goroutine,
	// but the model is a process-wide singleton and the tests use it too.
	mutex   sync.Mutex
	sizes   map[string]int64
	imports map[string][]string
}

// runtimeCapabilityCostModelOnce keeps one model per process, so the standard
// library's import graph is walked once however many times an order is wanted.
var (
	runtimeCapabilityCostModelOnce  sync.Once
	runtimeCapabilityCostModelValue *runtimeCapabilityCostModel
)

func sharedRuntimeCapabilityCostModel() *runtimeCapabilityCostModel {
	runtimeCapabilityCostModelOnce.Do(func() {
		runtimeCapabilityCostModelValue = newRuntimeCapabilityCostModel()
	})
	return runtimeCapabilityCostModelValue
}

// newRuntimeCapabilityCostModel mirrors the build context goc uses to select
// standard library files: the vendored tree rather than the host GOROOT, the
// target architecture rather than the host's, and the purego tag, because goc
// compiles most packages from their portable Go files. The mirror only has to be
// close enough to rank programs, so it deliberately does not reproduce goc's
// per-package assembly decisions.
func newRuntimeCapabilityCostModel() *runtimeCapabilityCostModel {
	context := build.Default
	context.GOOS = "linux"
	context.GOARCH = "arm64"
	context.CgoEnabled = false
	context.BuildTags = append(append([]string{}, context.BuildTags...), "purego")

	stdlibRoot, err := filepath.Abs(filepath.Join("..", "..", "stdlib"))
	if err != nil {
		stdlibRoot = filepath.Join("..", "..", "stdlib")
	}
	context.GOROOT = stdlibRoot

	return &runtimeCapabilityCostModel{
		context:      context,
		stdlibRoot:   stdlibRoot,
		testdataRoot: filepath.Join("..", "..", "goc", "testdata"),
		sizes:        make(map[string]int64),
		imports:      make(map[string][]string),
	}
}

// estimate reports the modelled compile cost of one capability, in bytes of
// source the compiler has to read. It is a rank, not a prediction: nothing
// interprets the number except a sort.
func (model *runtimeCapabilityCostModel) estimate(capability runtimeCapability) int64 {
	source := filepath.Join(model.testdataRoot, capability.source)

	var total int64
	if info, err := os.Stat(source); err == nil {
		total = info.Size()
	}
	importPaths := programImportPaths(source)

	model.mutex.Lock()
	defer model.mutex.Unlock()

	// One visited set for the whole program, so a package that two of its
	// imports both reach is counted once.
	visited := make(map[string]bool)
	for _, path := range importPaths {
		total += model.collectLocked(path, visited)
	}
	return total
}

// closureSize is the source size of one package's own closure. It exists for the
// model's tests; the queue always asks about a whole program.
func (model *runtimeCapabilityCostModel) closureSize(path string) int64 {
	model.mutex.Lock()
	defer model.mutex.Unlock()

	return model.collectLocked(path, make(map[string]bool))
}

// collectLocked adds a package and everything it imports to visited, and returns
// the source size of whatever was not already there. The visited set is also what
// terminates the walk, so an import cycle cannot recurse forever.
func (model *runtimeCapabilityCostModel) collectLocked(path string, visited map[string]bool) int64 {
	if visited[path] {
		return 0
	}
	visited[path] = true

	size, imports := model.describeLocked(path)
	for _, imported := range imports {
		size += model.collectLocked(imported, visited)
	}
	return size
}

// describeLocked returns one package's own source size and its direct imports,
// memoized. A package that does not resolve is memoized as empty: the model is a
// hint, and ranking a program low because one of its imports moved is a better
// failure than refusing to rank it.
func (model *runtimeCapabilityCostModel) describeLocked(path string) (int64, []string) {
	if size, known := model.sizes[path]; known {
		return size, model.imports[path]
	}

	pkg, err := model.importPackage(path)
	if err != nil {
		model.sizes[path] = 0
		model.imports[path] = nil
		return 0, nil
	}

	size := packageSourceSize(pkg)
	model.sizes[path] = size
	model.imports[path] = pkg.Imports
	return size, pkg.Imports
}

// importPackage resolves one import path the way goc's loader does, including the
// vendored directory, which holds the packages the standard library reaches
// through its own vendor tree.
func (model *runtimeCapabilityCostModel) importPackage(path string) (*build.Package, error) {
	vendored := filepath.Join(model.stdlibRoot, "src", "vendor", filepath.FromSlash(path))
	if info, err := os.Stat(vendored); err == nil && info.IsDir() {
		return model.context.ImportDir(vendored, 0)
	}
	return model.context.Import(path, "", 0)
}

func packageSourceSize(pkg *build.Package) int64 {
	var size int64
	for _, group := range [][]string{pkg.GoFiles, pkg.SFiles} {
		for _, name := range group {
			info, err := os.Stat(filepath.Join(pkg.Dir, name))
			if err != nil {
				continue
			}
			size += info.Size()
		}
	}
	return size
}

// programImportPaths returns the import paths a capability source names. It
// parses imports only, so it does not depend on the program type-checking.
func programImportPaths(source string) []string {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, source, nil, parser.ImportsOnly)
	if err != nil {
		return nil
	}

	paths := make([]string, 0, len(parsed.Imports))
	for _, specification := range parsed.Imports {
		path, err := strconv.Unquote(specification.Path.Value)
		if err != nil {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// runtimeCapabilitiesByDescendingCompileCost returns the capabilities ordered
// most expensive first. Ties keep matrix order, so the result is deterministic.
func runtimeCapabilitiesByDescendingCompileCost(capabilities []runtimeCapability) []runtimeCapability {
	model := sharedRuntimeCapabilityCostModel()

	type rankedCapability struct {
		capability runtimeCapability
		cost       int64
		index      int
	}
	ranked := make([]rankedCapability, 0, len(capabilities))
	for index, capability := range capabilities {
		ranked = append(ranked, rankedCapability{
			capability: capability,
			cost:       model.estimate(capability),
			index:      index,
		})
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		if ranked[left].cost != ranked[right].cost {
			return ranked[left].cost > ranked[right].cost
		}
		return ranked[left].index < ranked[right].index
	})

	ordered := make([]runtimeCapability, 0, len(ranked))
	for _, entry := range ranked {
		ordered = append(ordered, entry.capability)
	}
	return ordered
}
