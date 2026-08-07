package goc

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/evanphx/cg12/internal/cachefile"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/memo"
	"github.com/evanphx/cg12/opt"
)

// linkTimePasses are the passes that are not a package's business.
//
// deadfunc deletes a function nothing calls. Over one package that is wrong,
// not merely weaker: a package's exported functions are called from packages
// that are not in front of it, and it would delete them. Dropping dead code is
// what a linker does, and this runs once at the end in that role.
//
// inline-nosplit measures laid-out frames to decide what may be inlined into a
// function that emits no stack-growth check. Stack depth is a property of a
// call chain, and call chains cross packages. Its own comment requires it to
// run after everything else, which is what it still does.
var linkTimePasses = map[string]bool{
	"deadfunc":       true,
	"inline-nosplit": true,
}

// OptimizeModule optimises a Go module one package at a time.
//
// It replaces a whole-module opt.OptimizeModule on every path that compiles Go.
// The passes are the same passes; what changed is what each one is allowed to
// see. A pass runs over one package's functions, so the inliner inlines within
// a package and not across one, and the result of optimising a package depends
// on that package rather than on the program that happened to import it.
//
// That is the property the caches need. It is also strictly less optimisation
// than a whole-module pipeline does -- a call into another package is no longer
// an inlining candidate -- and that cost is real and accepted: compile time is
// worth more here than the last few percent of generated code, and the quality
// can come back later as a pass over the merged module without giving the
// property up.
//
// Two passes are not a package's business and still run once over everything;
// see linkTimePasses.
//
// The per-function memo underneath keys each function on what its optimisation
// actually read (opt/depset.go), so an unchanged package reuses every body.
// There is no flag. CG12_NOCACHE=1 turns the memo off along with every other
// cache; the per-package structure is not optional and has no switch.
func OptimizeModule(module *ir.Module, target Target) {
	pipeline := opt.ModulePipeline()
	perPackage, linkTime := splitLinkTimePasses(pipeline)

	groups := groupFunctionsByPackage(module)
	store, path := loadOptimiserMemo(target)
	before := int64(-1)
	if store != nil {
		before = store.Bytes()
	}

	original := module.Funcs
	optimised := make([]*ir.Func, 0, len(original))
	for _, group := range groups {
		module.Funcs = group.functions
		optimisePackage(module, perPackage, store, target)
		optimised = append(optimised, module.Funcs...)
	}
	// Restore the module's own function order rather than the order the packages
	// happened to be visited in. Text layout follows this slice, so leaving it
	// grouped would move every function in the image and make a diff against a
	// whole-module build unreadable for reasons that have nothing to do with
	// what the optimiser decided.
	module.Funcs = restoreFunctionOrder(original, optimised)

	// The link-time passes see the finished program, which is the only point at
	// which "nothing calls this" and "this call chain is this deep" are answerable.
	opt.Run(module, linkTime)

	saveOptimiserMemo(store, path, before)
}

// optimisePackage runs the per-package pipeline over whatever module.Funcs
// currently holds, through the memo when there is one.
func optimisePackage(module *ir.Module, pipeline []opt.Pass, store *memo.Store, target Target) {
	if store == nil || len(pipeline) <= opt.PerFunctionPrefixLen {
		opt.Run(module, pipeline)
		return
	}
	// The prefix has to run through the same session and the same pipeline slice
	// the stage is handed. Rebuilding the pipeline across the split gives jump
	// threading a second per-function budget and silently moves three functions
	// -- see memo.RunStage.
	session := opt.NewSession()
	session.Run(module, pipeline[:opt.PerFunctionPrefixLen])
	_, err := memo.RunStage(module, session, pipeline[opt.PerFunctionPrefixLen:], store, memo.Options{
		Pipeline: opt.PipelineIdentity(),
		Target:   string(target),
		Compiler: compilerIdentity(),
	})
	if err != nil {
		// A memo that cannot be applied is a cache failure, not a compile
		// failure, but it leaves the package half-optimised, so the pipeline is
		// run again from the top rather than emitting what the failed stage left.
		//
		// Reported, not swallowed: a cache that silently declines to work looks
		// exactly like a cache that is working and saving nothing, which is the
		// state this wiring shipped in for its first measurement.
		fmt.Fprintf(os.Stderr, "goc: optimiser memo unusable, optimising from nothing: %v\n", err)
		opt.Run(module, pipeline)
	}
}

// functionGroup is one package's functions, in the order the module declared
// them.
type functionGroup struct {
	path      string
	functions []*ir.Func
}

// groupFunctionsByPackage partitions a module's functions into the packages
// that declared them.
//
// The attribution is by symbol name, because ir.Func carries no package: goc
// names a function `<import path>.<rest>` and ModuleImportPaths recovers the
// paths from the module itself. The compiler's own `.goc.` symbols and anything
// else that matches no path attribute to "", which is a group like any other --
// they are generated, they belong to no package, and grouping them together
// means they are optimised together and not folded into whichever package
// sorted first.
func groupFunctionsByPackage(module *ir.Module) []functionGroup {
	paths := ModuleImportPaths(module)
	byPath := map[string][]*ir.Func{}
	for _, function := range module.Funcs {
		path := PackageOfFunction(function.Name, paths)
		byPath[path] = append(byPath[path], function)
	}
	ordered := make([]string, 0, len(byPath))
	for path := range byPath {
		ordered = append(ordered, path)
	}
	// Sorted, so the visit order is a property of the program and not of Go's
	// map iteration. Two builds of the same source have to make the same
	// decisions in the same order.
	sort.Strings(ordered)
	groups := make([]functionGroup, 0, len(ordered))
	for _, path := range ordered {
		groups = append(groups, functionGroup{path: path, functions: byPath[path]})
	}
	return groups
}

// restoreFunctionOrder puts the surviving functions back into the order the
// module had them in, and appends anything the pipeline created.
func restoreFunctionOrder(original, optimised []*ir.Func) []*ir.Func {
	position := make(map[*ir.Func]int, len(original))
	for index, function := range original {
		position[function] = index
	}
	survivors := make([]*ir.Func, 0, len(optimised))
	var created []*ir.Func
	for _, function := range optimised {
		if _, known := position[function]; known {
			survivors = append(survivors, function)
		} else {
			created = append(created, function)
		}
	}
	sort.SliceStable(survivors, func(i, j int) bool {
		return position[survivors[i]] < position[survivors[j]]
	})
	return append(survivors, created...)
}

// splitLinkTimePasses divides a pipeline into the passes a package may run and
// the passes that need the finished program. Only the top level is examined:
// the passes in question are top-level entries, and a pass nested inside a
// fixpoint is there because that fixpoint's convergence depends on it.
func splitLinkTimePasses(pipeline []opt.Pass) (perPackage, linkTime []opt.Pass) {
	for _, pass := range pipeline {
		if linkTimePasses[pass.Name()] {
			linkTime = append(linkTime, pass)
			continue
		}
		perPackage = append(perPackage, pass)
	}
	return perPackage, linkTime
}

// loadOptimiserMemo reads the memo, returning a nil store when there is no
// cache to use.
func loadOptimiserMemo(target Target) (*memo.Store, string) {
	directory := optimiserMemoDirectory()
	if directory == "" {
		return nil, ""
	}
	key := cachefile.Digest(opt.PipelineIdentity() + "\x00" + string(target) + "\x00" + compilerIdentity())
	path := cachefile.Path(directory, key, ".memo")
	if loaded, err := memo.Load(path); err == nil {
		cachefile.MarkUsed(path)
		return loaded, path
	}
	return memo.NewStore(), path
}

// saveOptimiserMemo writes the memo back if it grew. Writing an unchanged store
// would rewrite tens of megabytes on every compile that hit in full, which is
// the case this exists to make fast.
func saveOptimiserMemo(store *memo.Store, path string, before int64) {
	if store == nil || path == "" || store.Bytes() == before {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "goc: could not save the optimiser memo: %v\n", err)
		return
	}
	if err := store.Save(path); err != nil {
		fmt.Fprintf(os.Stderr, "goc: could not save the optimiser memo: %v\n", err)
	}
}

// optimiserMemoDirectory is where the optimiser memo lives: the default
// location always, with CG12_OPT_MEMO relocating it the way CG12_FUNC_CACHE
// relocates the function cache. There is no setting that turns it off on its
// own; CG12_NOCACHE=1 turns off every cache at once, checked in
// internal/cachefile so no cache can forget it.
//
// An empty string means no cache -- a box with no user cache directory
// optimises the long way rather than failing the build.
func optimiserMemoDirectory() string {
	return cachefile.Directory("CG12_OPT_MEMO", "optimiser-memo")
}

// compilerIdentity hashes the running binary. Every pass, every heuristic and
// every constant in the optimiser is part of what produced a memoised body, and
// none of them are otherwise in the key.
var compilerIdentity = sync.OnceValue(func() string {
	executable, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%x", sha256.Sum256(contents))[:32]
})
