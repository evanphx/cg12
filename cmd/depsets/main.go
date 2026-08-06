// depsets measures whether Option C's memoisation key can exist.
//
// BUILD_CACHE.md §3.4 costs Option C -- a memoised whole-module optimiser stage
// and backend, keyed per function on the set of functions the inliner consulted
// -- at an 85%/86% ceiling, and says in the same paragraph that "nothing here is
// measured as an implementation; only the ceiling is". The ceiling comes from
// §2.4 and §2.4b, which measure that the OUTPUT is stable under an edit. That is
// not the same claim as the key being able to say so, and the gap between them
// is what this command measures.
//
// It runs goc's front end, then the per-function prefix of the optimiser
// (mem2reg + clean) that Option B would cache, and dumps a digest per function:
// that is the memoiser's INPUT. It then runs the whole-module remainder with
// opt.Record on and dumps a second digest per function -- the OUTPUT -- together
// with the dependency sets the inliner actually read to produce it.
//
// Two runs over two source trees are all the invalidation measurement needs: the
// changed inputs are the functions whose mid digest moved, and a memo for f is
// invalid exactly when f's recorded set meets that population.
//
// It changes no compiler behaviour: opt.Record installs a recorder that the
// inliner writes to and restores nil afterwards.
package main

import (
	"bufio"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"

	_ "github.com/evanphx/cg12/arm64" // registers opt.NoSplitFrameBudgetFor
)

// perPackageStage is the prefix of opt.DefaultPipeline that touches one function
// at a time and never looks at another function. It is what Option B caches, and
// therefore what the memoised stage is handed.
func perPackageStage() []opt.Pass {
	return []opt.Pass{opt.FuncPass("mem2reg", opt.Mem2Reg), cleanFixpoint()}
}

func cleanFixpoint() opt.Pass {
	return opt.Fixpoint("clean",
		opt.FuncPass("fold", opt.Fold),
		opt.FuncPass("copy", opt.Copy),
		opt.FuncPass("loadelim", opt.LoadElim),
		opt.FuncPass("deadalloc", opt.DeadAlloc),
		opt.FuncPass("gvn", opt.GVN),
		opt.JumpThreadPass(),
		opt.FuncPass("simplifycfg", opt.SimplifyCFG),
		opt.FuncPass("dce", opt.DCE),
	)
}

// wholeModuleStage is the rest of opt.DefaultPipeline, the stage Option C
// memoises. It is reconstructed here rather than sliced off DefaultPipeline
// because the two halves have to be separate opt.Run calls for the mid-stage
// digest to exist at all; -unsplit checks that reconstructing it changes nothing.
func wholeModuleStage() []opt.Pass {
	clean := cleanFixpoint()
	inline := opt.InlinePass()
	return []opt.Pass{
		opt.Fixpoint("inline-fixpoint", inline, clean),
		opt.FuncPass("mem2reg", opt.Mem2Reg),
		clean,
		opt.ModulePass("unroll", opt.UnrollRecursion),
		opt.Fixpoint("inline-fixpoint", inline, clean),
		opt.FuncPass("constantp", opt.ResolveConstantP),
		opt.FuncPass("ifconvert", opt.IfConvert),
		clean,
		opt.FuncPass("tailmerge", opt.TailMerge),
		opt.FuncPass("simplifycfg", opt.SimplifyCFG),
		opt.FuncPass("dce", opt.DCE),
		opt.ModulePass("deadfunc", opt.DeadFuncElim),
		opt.FuncPass("gcm", opt.GCM),
		opt.FuncPass("dce", opt.DCE),
		opt.ModulePass("inline-nosplit", opt.InlineIntoNoSplitCallers),
	}
}

func digest(f *ir.Func) string {
	sum := sha256.Sum256([]byte(f.String()))
	return fmt.Sprintf("%x", sum[:12])
}

func writeDigests(path string, m *ir.Module) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()
	names := make([]string, 0, len(m.Funcs))
	byName := make(map[string]*ir.Func, len(m.Funcs))
	for _, f := range m.Funcs {
		names = append(names, f.Name)
		byName[f.Name] = f
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "%s %s\n", digest(byName[name]), name)
	}
	return nil
}

// writeDeps emits the dependency file: a function-name table, then each
// function's consulted and spliced sets as indices into it, then the three
// module-wide facts that no per-function set expresses.
func writeDeps(path string, m *ir.Module, deps *opt.InlineDeps, ids map[*ir.Func]int) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()

	order := make([]*ir.Func, 0, len(ids))
	for f := range ids {
		order = append(order, f)
	}
	sort.Slice(order, func(i, j int) bool { return ids[order[i]] < ids[order[j]] })

	fmt.Fprintf(w, "V 1\nN %d\n", len(order))
	for _, f := range order {
		fmt.Fprintf(w, "%s\n", f.Name)
	}
	emit := func(tag string, f *ir.Func, set []*ir.Func) {
		if len(set) == 0 {
			return
		}
		fmt.Fprintf(w, "%s %d %d", tag, ids[f], len(set))
		for _, dep := range set {
			fmt.Fprintf(w, " %d", ids[dep])
		}
		fmt.Fprintln(w)
	}
	for _, f := range order {
		emit("C", f, deps.Consulted(f))
	}
	for _, f := range order {
		emit("S", f, deps.Spliced(f))
	}
	for _, f := range deps.NoSplitMeasured() {
		fmt.Fprintf(w, "X %d\n", ids[f])
	}
	for _, f := range deps.CostInlineSelected() {
		fmt.Fprintf(w, "K %d\n", ids[f])
	}
	counted := make([]*ir.Func, 0, len(deps.SiteCounts()))
	for f := range deps.SiteCounts() {
		counted = append(counted, f)
	}
	sort.Slice(counted, func(i, j int) bool { return counted[i].Name < counted[j].Name })
	for _, f := range counted {
		fmt.Fprintf(w, "T %d %d\n", ids[f], deps.SiteCount(f))
	}
	// The surviving population: which of the table's functions the whole-module
	// stage left in the module for the backend to compile.
	for _, f := range m.Funcs {
		if id, ok := ids[f]; ok {
			fmt.Fprintf(w, "L %d\n", id)
		}
	}
	return nil
}

func main() {
	outPrefix := flag.String("out", "", "write <prefix>.mid, <prefix>.post and <prefix>.deps")
	unsplit := flag.Bool("unsplit", false, "run opt.OptimizeModule whole instead of splitting the pipeline (control for the split)")
	flag.Parse()
	if flag.NArg() != 1 || *outPrefix == "" {
		fmt.Fprintln(os.Stderr, "usage: depsets -out prefix program.go")
		os.Exit(2)
	}
	path := flag.Arg(0)
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	start := time.Now()
	module, err := goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(path), src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "frontend:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "depsets %s: front end %.2fs, %d funcs (GOMAXPROCS=%d)\n",
		path, time.Since(start).Seconds(), len(module.Funcs), runtime.GOMAXPROCS(0))

	if *unsplit {
		opt.OptimizeModule(module)
		if err := writeDigests(*outPrefix+".post", module); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "  unsplit: %d funcs post-opt\n", len(module.Funcs))
		return
	}

	prefixStart := time.Now()
	opt.Run(module, perPackageStage())
	fmt.Fprintf(os.Stderr, "  per-package prefix %.2fs\n", time.Since(prefixStart).Seconds())
	if err := writeDigests(*outPrefix+".mid", module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Every function the memoised stage is handed gets an identity now, before
	// the stage runs, so a function it drops still has one in the dependency file.
	ids := make(map[*ir.Func]int, len(module.Funcs))
	for _, f := range module.Funcs {
		ids[f] = len(ids)
	}

	stageStart := time.Now()
	deps := opt.Record(func() {
		opt.Run(module, wholeModuleStage())
	})
	fmt.Fprintf(os.Stderr, "  whole-module stage %.2fs, %d funcs survive\n",
		time.Since(stageStart).Seconds(), len(module.Funcs))

	// A function the stage created (none are expected: inlining clones bodies
	// into existing functions) would have no identity, which the dependency file
	// must not silently drop.
	for _, f := range module.Funcs {
		if _, ok := ids[f]; !ok {
			fmt.Fprintf(os.Stderr, "  !! %s appeared during the whole-module stage\n", f.Name)
			ids[f] = len(ids)
		}
	}

	if err := writeDigests(*outPrefix+".post", module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := writeDeps(*outPrefix+".deps", module, deps, ids); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "  total %.2fs\n", time.Since(start).Seconds())
}
