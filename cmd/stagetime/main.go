// stagetime attributes a goc whole-program compile across its stages.
//
// It is measurement scaffolding for the build-cache design: it runs exactly the
// stages cmd/goc's default path runs, in the same order, and reports wall time
// and bytes allocated for each. It changes nothing about the compiler.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

type stage struct {
	name  string
	wall  time.Duration
	alloc uint64
}

var stages []stage

func totalAlloc() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.TotalAlloc
}

func timed(name string, run func()) {
	startAlloc := totalAlloc()
	start := time.Now()
	run()
	elapsed := time.Since(start)
	endAlloc := totalAlloc()
	stages = append(stages, stage{name: name, wall: elapsed, alloc: endAlloc - startAlloc})
	fmt.Fprintf(os.Stderr, "  %-24s %8.2fs  %8.2f MiB\n", name, elapsed.Seconds(), float64(endAlloc-startAlloc)/(1<<20))
}

func moduleStats(label string, m *ir.Module) {
	blocks := 0
	instrs := 0
	withBody := 0
	for _, f := range m.Funcs {
		if f.Start == nil {
			continue
		}
		withBody++
		blocks += len(f.Blocks)
		for _, b := range f.Blocks {
			instrs += len(b.Instrs)
		}
	}
	fmt.Fprintf(os.Stderr, "  %s: %d funcs (%d with body), %d blocks, %d instrs, %d data, %d files\n",
		label, len(m.Funcs), withBody, blocks, instrs, len(m.Data), len(m.Files))
}

// packageOf guesses the Go package a goc function name belongs to. goc mangles
// names; this only has to be good enough to size the per-package unit.
func packageOf(name string) string {
	// Typical shapes: "runtime.mallocgc", "internal/abi.Name.Data",
	// "main.main", "fmt.(*pp).printArg".
	slash := strings.LastIndex(name, "/")
	head := name
	if slash >= 0 {
		head = name[slash+1:]
	}
	dot := strings.Index(head, ".")
	if dot < 0 {
		return "<none>"
	}
	if slash >= 0 {
		return name[:slash+1] + head[:dot]
	}
	return head[:dot]
}

func packageHistogram(m *ir.Module, top int) {
	counts := map[string]int{}
	instrCounts := map[string]int{}
	for _, f := range m.Funcs {
		pkg := packageOf(f.Name)
		counts[pkg]++
		for _, b := range f.Blocks {
			instrCounts[pkg] += len(b.Instrs)
		}
	}
	type row struct {
		pkg    string
		funcs  int
		instrs int
	}
	var rows []row
	for pkg, n := range counts {
		rows = append(rows, row{pkg, n, instrCounts[pkg]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].instrs > rows[j].instrs })
	fmt.Fprintf(os.Stderr, "  packages: %d\n", len(rows))
	shown := rows
	if len(shown) > top {
		shown = shown[:top]
	}
	for _, r := range shown {
		fmt.Fprintf(os.Stderr, "    %-40s %6d funcs %9d instrs\n", r.pkg, r.funcs, r.instrs)
	}
}

// perPackageStage is the prefix of opt.DefaultPipeline that touches one function
// at a time and never looks at another function: mem2reg plus the cleanup
// fixpoint. It is the candidate for a per-package cacheable stage.
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

// wholeModuleStage is the rest of opt.DefaultPipeline: everything from the first
// inline fixpoint onward. Reconstructed here rather than sliced off
// DefaultPipeline because the two halves have to be run by separate opt.Run
// calls to be timed apart.
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

func writeFuncHashes(path string, m *ir.Module) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer f.Close()
	names := make([]string, 0, len(m.Funcs))
	byName := make(map[string]*ir.Func, len(m.Funcs))
	for _, fn := range m.Funcs {
		names = append(names, fn.Name)
		byName[fn.Name] = fn
	}
	sort.Strings(names)
	for _, name := range names {
		sum := sha256.Sum256([]byte(byName[name].String()))
		fmt.Fprintf(f, "%x %s\n", sum[:8], name)
	}
}

func main() {
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile here")
	serialize := flag.Bool("serialize", true, "measure IR marshal/decode round trips")
	splitOpt := flag.Bool("split-opt", false, "time the per-package prefix and whole-module remainder of the pipeline apart")
	packPath := flag.String("runtime", "", "compile against this prebuilt runtime pack")
	preHashes := flag.String("prehash", "", "write per-function pre-opt IR hashes here")
	postHashes := flag.String("posthash", "", "write per-function post-opt IR hashes here")
	midHashes := flag.String("midhash", "", "write per-function hashes after the per-package stage here")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: stagetime [-cpuprofile f] program.go")
		os.Exit(2)
	}
	path := flag.Arg(0)
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer pprof.StopCPUProfile()
	}

	fmt.Fprintf(os.Stderr, "stagetime %s (GOMAXPROCS=%d)\n", path, runtime.GOMAXPROCS(0))

	var m *ir.Module
	if *packPath != "" {
		var manifest *runtimepack.Manifest
		timed("read-pack-manifest", func() {
			manifest, err = runtimepack.ReadManifest(*packPath)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "pack:", err)
			os.Exit(1)
		}
		timed("frontend", func() {
			var program *goc.ProgramModule
			program, err = goc.CompileExecutableAgainstRuntimeFor(goc.TargetARM64, filepath.Base(path), src, manifest)
			if err == nil {
				m = program.Module
			}
		})
	} else {
		timed("frontend", func() {
			m, err = goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(path), src)
		})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "frontend:", err)
		os.Exit(1)
	}
	moduleStats("pre-opt", m)
	packageHistogram(m, 20)

	var preOptEncoded []byte
	if *serialize {
		var verifyErr error
		timed("verify-preopt", func() {
			verifyErr = ir.VerifyModule(m)
		})
		fmt.Fprintf(os.Stderr, "  VerifyModule(pre-opt) = %v\n", verifyErr)
		failed := 0
		var firstFailures []string
		for _, fn := range m.Funcs {
			if verr := ir.Verify(fn); verr != nil {
				failed++
				if len(firstFailures) < 5 {
					firstFailures = append(firstFailures, verr.Error())
				}
			}
		}
		fmt.Fprintf(os.Stderr, "  ir.Verify fails on %d of %d functions\n", failed, len(m.Funcs))
		for _, message := range firstFailures {
			fmt.Fprintf(os.Stderr, "    !! %s\n", message)
		}
		timed("marshal-preopt", func() {
			preOptEncoded, err = m.MarshalBinary()
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "marshal:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "  pre-opt IR encodes to %.1f MiB\n", float64(len(preOptEncoded))/(1<<20))
		var decodeErr error
		timed("decode-preopt", func() {
			_, decodeErr = ir.DecodeModule(preOptEncoded)
		})
		fmt.Fprintf(os.Stderr, "  DecodeModule(pre-opt) = %v\n", decodeErr)
		preOptEncoded = nil
		runtime.GC()
	}

	if *preHashes != "" {
		writeFuncHashes(*preHashes, m)
	}

	if *splitOpt {
		timed("opt:per-package-stage", func() {
			opt.Run(m, perPackageStage())
		})
		moduleStats("after per-package stage", m)
		if *midHashes != "" {
			writeFuncHashes(*midHashes, m)
		}
		timed("opt:whole-module-stage", func() {
			opt.Run(m, wholeModuleStage())
		})
	} else {
		timed("opt.OptimizeModule", func() {
			opt.OptimizeModule(m)
		})
	}
	moduleStats("post-opt", m)
	if *postHashes != "" {
		writeFuncHashes(*postHashes, m)
	}

	if *serialize {
		var postOptEncoded []byte
		timed("marshal-postopt", func() {
			postOptEncoded, err = m.MarshalBinary()
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "marshal:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "  post-opt IR encodes to %.1f MiB\n", float64(len(postOptEncoded))/(1<<20))
		var decodeErr error
		timed("decode-postopt", func() {
			_, decodeErr = ir.DecodeModule(postOptEncoded)
		})
		fmt.Fprintf(os.Stderr, "  DecodeModule(post-opt) = %v\n", decodeErr)
		postOptEncoded = nil
		runtime.GC()
	}

	var objectBytes []byte
	timed("backend+object", func() {
		object, _, buildErr := arm64.CompileToObjectAndAssembly(m)
		if buildErr != nil {
			err = buildErr
			return
		}
		objectBytes, err = object.MarshalELF()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "backend:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "  object is %.1f MiB\n", float64(len(objectBytes))/(1<<20))

	var total time.Duration
	var totalAllocated uint64
	for _, s := range stages {
		if strings.HasPrefix(s.name, "marshal") || strings.HasPrefix(s.name, "decode") {
			continue
		}
		total += s.wall
		totalAllocated += s.alloc
	}
	fmt.Fprintf(os.Stderr, "\n  compile total (excluding serialization probes): %.2fs, %.2f GiB allocated\n",
		total.Seconds(), float64(totalAllocated)/(1<<30))
	fmt.Println("stage,seconds,mib_allocated")
	for _, s := range stages {
		fmt.Printf("%s,%.3f,%.1f\n", s.name, s.wall.Seconds(), float64(s.alloc)/(1<<20))
	}
}
