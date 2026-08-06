// Command phasesplit measures where a whole-program goc compile spends its time,
// split at the boundaries a build cache would have to cut on.
//
// It exists for BUILD_CACHE.md, which has to decide whether goc can cache
// per-package the way Go's build cache does. That decision turns on one number:
// how much of a compile is front end (parse, type check, IR generation -- work
// that is a function of one package's source and is therefore cacheable under any
// scheme) against how much is whole-module optimization and code generation --
// work that reads the entire program at once and is not.
//
// The three phases are the three calls a driver already makes in this order, so
// nothing here is instrumented and nothing in the compiler is changed:
//
//	goc.CompileExecutableFor  front end: parse + type check + IR generation
//	opt.OptimizeModule        whole-module optimization, thirteen passes
//	arm64.CompileToObject     lowering, register allocation, encoding, ELF
//
// The front end phase is one call and cannot be subdivided from outside the
// package; BUILD_CACHE.md splits it further using a CPU profile of the same
// compile, which separates go/parser and go/types from goc's own IR generation.
//
// A second compile in the same process is timed too. goc/source_world.go shares
// one parsed and type-checked copy of the runtime closure between compiles in a
// process, so the difference between the first front end and the second is
// exactly what an in-process front-end cache is already worth -- the closest
// thing in the tree to a measurement of a per-package front-end cache's ceiling.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

func main() {
	optimize := flag.Bool("O", true, "run the whole-module optimizer")
	repeats := flag.Int("n", 2, "compile the program this many times in one process")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: phasesplit [-O] [-n runs] file.go")
		os.Exit(2)
	}
	path := flag.Arg(0)
	source, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phasesplit: %v\n", err)
		os.Exit(1)
	}
	for run := 1; run <= *repeats; run++ {
		if err := measure(run, path, source, *optimize); err != nil {
			fmt.Fprintf(os.Stderr, "phasesplit: %v\n", err)
			os.Exit(1)
		}
	}
}

func measure(run int, path string, source []byte, optimize bool) error {
	var module *ir.Module
	frontEnd, frontEndBytes, err := phase(func() error {
		var err error
		module, err = goc.CompileExecutableFor(goc.TargetARM64, path, source)
		return err
	})
	if err != nil {
		return err
	}
	functions, blocks := shape(module)

	var optimizeElapsed time.Duration
	var optimizeBytes uint64
	if optimize {
		optimizeElapsed, optimizeBytes, err = phase(func() error {
			opt.OptimizeModule(module)
			return nil
		})
		if err != nil {
			return err
		}
	}
	optimizedFunctions, optimizedBlocks := shape(module)

	var objectBytes int
	backEnd, backEndBytes, err := phase(func() error {
		object, err := arm64.CompileToObject(module)
		if err != nil {
			return err
		}
		encoded, err := object.MarshalELF()
		if err != nil {
			return err
		}
		objectBytes = len(encoded)
		return nil
	})
	if err != nil {
		return err
	}

	total := frontEnd + optimizeElapsed + backEnd
	fmt.Printf("run %d %s -O=%v\n", run, path, optimize)
	fmt.Printf("  frontend  %8.2fs %5.1f%%  %8.2f GiB  %d funcs %d blocks\n",
		frontEnd.Seconds(), percent(frontEnd, total), gib(frontEndBytes), functions, blocks)
	fmt.Printf("  optimize  %8.2fs %5.1f%%  %8.2f GiB  %d funcs %d blocks after\n",
		optimizeElapsed.Seconds(), percent(optimizeElapsed, total), gib(optimizeBytes), optimizedFunctions, optimizedBlocks)
	fmt.Printf("  backend   %8.2fs %5.1f%%  %8.2f GiB  %d object bytes\n",
		backEnd.Seconds(), percent(backEnd, total), gib(backEndBytes), objectBytes)
	fmt.Printf("  total     %8.2fs         %8.2f GiB\n",
		total.Seconds(), gib(frontEndBytes+optimizeBytes+backEndBytes))
	return nil
}

// phase runs one step and reports its wall time and the bytes it allocated.
func phase(step func() error) (time.Duration, uint64, error) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := step()
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	return elapsed, after.TotalAlloc - before.TotalAlloc, err
}

func shape(module *ir.Module) (functions, blocks int) {
	for _, function := range module.Funcs {
		functions++
		blocks += len(function.Blocks)
	}
	return functions, blocks
}

func percent(part, whole time.Duration) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

func gib(bytes uint64) float64 { return float64(bytes) / (1 << 30) }
