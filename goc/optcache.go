package goc

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/evanphx/cg12/internal/cachefile"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/memo"
	"github.com/evanphx/cg12/opt"
)

// OptimizeModule runs the whole-module optimiser, reusing the finished body of
// every function whose optimisation would reach the same answer it reached last
// time.
//
// It replaces a bare opt.OptimizeModule on every path that compiles Go. The
// optimiser is the largest phase of a goc compile -- 9.80s of 16.16s on the
// http program -- and it is the phase the function cache does not touch: that
// cache holds *lowered* IR, which is the optimiser's input, so a cache hit
// there still leaves the whole module to optimise.
//
// The memo underneath is not a package-granularity cache and does not need the
// module to be package-pure. It keys each function on what that function's
// optimisation actually read -- opt/depset.go records it at the choke points --
// so a function whose inlined callees and read attributes are unchanged reuses
// its body even though the pass that produced it ran over the whole module.
// That is why it stays byte-identical across a whole-module inliner, and it is
// why this needed no rearchitecture to switch on.
//
// There is no flag. CG12_NOCACHE=1 turns it off along with every other cache,
// for gates that need a compile from nothing.
func OptimizeModule(module *ir.Module, target Target) {
	pipeline := opt.ModulePipeline()
	if cachefile.Disabled() || len(pipeline) <= opt.PerFunctionPrefixLen {
		opt.OptimizeModule(module)
		return
	}

	// The prefix has to run through the same session and the same pipeline slice
	// the stage is handed. Rebuilding the pipeline across the split gives jump
	// threading a second per-function budget and silently moves three functions
	// -- see memo.RunStage.
	session := opt.NewSession()
	session.Run(module, pipeline[:opt.PerFunctionPrefixLen])

	directory := optimiserMemoDirectory()
	if directory == "" {
		session.Run(module, pipeline[opt.PerFunctionPrefixLen:])
		return
	}
	key := cachefile.Digest(opt.PipelineIdentity() + "\x00" + string(target) + "\x00" + compilerIdentity())
	path := cachefile.Path(directory, key, ".memo")

	store := memo.NewStore()
	if loaded, err := memo.Load(path); err == nil {
		store = loaded
		cachefile.MarkUsed(path)
	}
	before := store.Bytes()

	result, err := memo.RunStage(module, session, pipeline[opt.PerFunctionPrefixLen:], store, memo.Options{
		Pipeline: opt.PipelineIdentity(),
		Target:   string(target),
		Compiler: compilerIdentity(),
	})
	if err != nil {
		// A memo that cannot be applied is a cache failure, not a compile
		// failure, and the module is left half-optimised by the attempt. Rebuild
		// it the long way rather than emitting what the failed stage left.
		//
		// Reported, not swallowed: a cache that silently declines to work looks
		// exactly like a cache that is working and saving nothing, which is the
		// state this wiring shipped in for its first measurement.
		fmt.Fprintf(os.Stderr, "goc: optimiser memo unusable, optimising from nothing: %v\n", err)
		opt.OptimizeModule(module)
		return
	}
	if os.Getenv("GOC_MEMO_STATS") != "" {
		fmt.Fprintf(os.Stderr, "opt memo: %d funcs, %d hits, %d misses, %d excluded, %.1fMB\n",
			result.Funcs, result.Hits, result.Misses, result.Excluded, float64(store.Bytes())/(1<<20))
	}
	// Writing an unchanged store would rewrite tens of megabytes on every
	// compile that hit in full, which is the case this exists to make fast.
	if store.Bytes() == before {
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
