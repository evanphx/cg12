// ccstat attributes a cg12 C compile across its stages.
//
// It is the C-side counterpart of cmd/stagetime, and it exists because every
// other instrument in the tree drives the Go front end: cmd/stagetime, cmd/memoc
// and goc's own -memprofile all go through goc.CompileExecutableFor, while
// cmd/cc has no profiling flags at all. Optimiser and backend work is shared
// between the two front ends, so a C compile is the cheap way to measure it.
//
// It runs exactly the stages cmd/cc runs, in the same order, and changes nothing
// about the compiler.
//
//	ccstat -O prog.c                 # per-stage wall, allocation and live heap
//	GOC_OPT_PASSTIME=1 ccstat -O p.c # ... and a per-pass breakdown of the optimiser
//	ccstat -O -repeat 10 prog.c      # is the allocation total reproducible?
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/evanphx/cg12/opt"
)

// stage is one measured phase of a compile.
//
// Allocated and Objects are cumulative counters read from runtime/metrics, so
// the delta across a stage is exactly what that stage allocated regardless of
// when the collector ran. LiveHeap is measured after a forced collection at the
// stage boundary, which is why it is only recorded per stage and never per pass:
// it is the honest answer to "what is still reachable", and forcing a collection
// often enough to answer it per pass would distort every time in the table.
type stage struct {
	Name      string
	Wall      time.Duration
	Allocated uint64
	Objects   uint64
	LiveHeap  uint64
}

const (
	allocBytesMetric   = "/gc/heap/allocs:bytes"
	allocObjectsMetric = "/gc/heap/allocs:objects"
	liveObjectsMetric  = "/memory/classes/heap/objects:bytes"
)

var allocSample = []metrics.Sample{
	{Name: allocBytesMetric},
	{Name: allocObjectsMetric},
}

// allocated reads the cumulative allocation counters. runtime/metrics is used
// rather than runtime.ReadMemStats because ReadMemStats stops the world, and
// this is read at every stage boundary and (with GOC_OPT_PASSTIME) far more
// often than that inside the optimiser.
func allocated() (bytes, objects uint64) {
	metrics.Read(allocSample)
	return allocSample[0].Value.Uint64(), allocSample[1].Value.Uint64()
}

// liveHeap forces a collection and reports what survived it.
func liveHeap() uint64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// peakSampler polls the live heap on an interval and keeps the largest reading.
//
// It reports a floor, not a peak: a spike shorter than the sampling interval is
// invisible to it. That is worth saying plainly, because the number looks like a
// measurement and is really a lower bound. The reliable memory figures in this
// tool are the allocation counters and the per-stage live heap.
type peakSampler struct {
	stop   chan struct{}
	done   chan struct{}
	mutex  sync.Mutex
	peak   uint64
	sample []metrics.Sample
}

func startPeakSampler(interval time.Duration) *peakSampler {
	sampler := &peakSampler{
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		sample: []metrics.Sample{{Name: liveObjectsMetric}},
	}
	go sampler.run(interval)
	return sampler
}

func (s *peakSampler) run(interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			metrics.Read(s.sample)
			live := s.sample[0].Value.Uint64()
			s.mutex.Lock()
			if live > s.peak {
				s.peak = live
			}
			s.mutex.Unlock()
		}
	}
}

func (s *peakSampler) finish() uint64 {
	close(s.stop)
	<-s.done
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.peak
}

// run is one compile's worth of measurement.
type run struct {
	module    *ir.Module
	stages    []stage
	objectSHA string
	preOpt    shape
	postOpt   shape
}

// shape is how big the module is at a stage boundary.
type shape struct {
	Funcs    int
	WithBody int
	Blocks   int
	Instrs   int
	Data     int
}

func shapeOf(m *ir.Module) shape {
	var result shape
	result.Funcs = len(m.Funcs)
	result.Data = len(m.Data)
	for _, function := range m.Funcs {
		if function.Start == nil {
			continue
		}
		result.WithBody++
		result.Blocks += len(function.Blocks)
		for _, block := range function.Blocks {
			result.Instrs += len(block.Instrs)
		}
	}
	return result
}

func (r *run) timed(name string, work func()) {
	startBytes, startObjects := allocated()
	start := time.Now()
	work()
	elapsed := time.Since(start)
	endBytes, endObjects := allocated()
	r.stages = append(r.stages, stage{
		Name:      name,
		Wall:      elapsed,
		Allocated: endBytes - startBytes,
		Objects:   endObjects - startObjects,
		LiveHeap:  liveHeap(),
	})
}

func main() {
	optimize := flag.Bool("O", false, "run the cg12 optimizer before code generation")
	repeat := flag.Int("repeat", 1, "compile this many times and report each run")
	workers := flag.String("backend-workers", "1", "GOC_BACKEND_WORKERS for the compile; \"\" leaves it alone")
	sampleEvery := flag.Duration("sample", 2*time.Millisecond, "live-heap sampling interval for the peak floor")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile of the compile to this file")
	memProfile := flag.String("memprofile", "", "write a heap profile of the compile to this file")
	census := flag.Bool("instr-census", false, "count how often each optional ir.Instr field is set")
	var includeDirs stringList
	var defines stringList
	flag.Var(&includeDirs, "I", "add an include search `directory` (repeatable)")
	flag.Var(&defines, "D", "define a preprocessor `macro`, name[=value] (repeatable)")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	input := flag.Arg(0)

	// The backend is the one parallel phase, and concurrent allocation lands in
	// whichever stage happens to be running. Serialising it is what makes the
	// backend row attributable; the emitted object does not depend on the count
	// (see arm64.backendWorkers), so this changes what is measured and not what
	// is produced.
	if *workers != "" {
		if err := os.Setenv("GOC_BACKEND_WORKERS", *workers); err != nil {
			fatalf("set GOC_BACKEND_WORKERS: %v", err)
		}
	}

	source, err := os.ReadFile(input)
	if err != nil {
		fatalf("%v", err)
	}

	if *cpuProfile != "" {
		file, err := os.Create(*cpuProfile)
		if err != nil {
			fatalf("create cpu profile: %v", err)
		}
		defer file.Close()
		if err := pprof.StartCPUProfile(file); err != nil {
			fatalf("start cpu profile: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	// The input's own directory is on the include path so a benchmark built as
	// one translation unit out of relative includes resolves them wherever it is
	// run from.
	options := cc.Options{
		IncludeDirs: append([]string{filepath.Dir(input)}, includeDirs...),
		Defines:     defines,
	}

	sampler := startPeakSampler(*sampleEvery)
	runs := make([]*run, 0, *repeat)
	for index := 0; index < *repeat; index++ {
		opt.ResetPassTiming()
		result := compile(input, string(source), options, *optimize)
		runs = append(runs, result)
		reportRun(index, result, *repeat)
	}
	peak := sampler.finish()

	fmt.Fprintf(os.Stderr, "\n  peak live heap (floor, %v sampling): %.1f MiB\n", *sampleEvery, mib(peak))
	fmt.Fprintf(os.Stderr, "  peak RSS: %.1f MiB\n", mib(peakRSS()))
	if *repeat > 1 {
		reportReproducibility(runs)
	}
	reportPasses()
	if *census && len(runs) > 0 {
		reportInstrCensus(runs[len(runs)-1].module)
	}
	writeCSV(runs)

	if *memProfile != "" {
		file, err := os.Create(*memProfile)
		if err != nil {
			fatalf("create heap profile: %v", err)
		}
		defer file.Close()
		runtime.GC()
		if err := pprof.WriteHeapProfile(file); err != nil {
			fatalf("write heap profile: %v", err)
		}
	}
}

// compile runs the stages cmd/cc runs, measuring each.
//
// It goes through cc.CompileWith rather than cc.Compile so that name is the
// input's real path: the front end resolves #include "..." relative to it, and
// the single-translation-unit benchmark inputs are built out of exactly that
// (cc/testdata/bench/lua/onelua.c is 32 relative includes and nothing else).
func compile(path, source string, options cc.Options, optimize bool) *run {
	result := &run{}
	var module *ir.Module
	result.timed("frontend", func() {
		var err error
		module, err = cc.CompileWith(path, source, options)
		if err != nil {
			fatalf("%v", err)
		}
	})
	result.preOpt = shapeOf(module)

	if optimize {
		result.timed("optimise", func() {
			opt.OptimizeModule(module)
		})
	}
	result.postOpt = shapeOf(module)

	var object *obj.Object
	result.timed("backend", func() {
		var err error
		object, err = arm64.CompileToObject(module)
		if err != nil {
			fatalf("backend: %v", err)
		}
	})

	var encoded []byte
	result.timed("elf-encode", func() {
		var err error
		encoded, err = object.MarshalELF()
		if err != nil {
			fatalf("elf encode: %v", err)
		}
	})
	result.objectSHA = fmt.Sprintf("%x", sha256.Sum256(encoded))
	result.module = module
	return result
}

func reportRun(index int, result *run, total int) {
	if total > 1 {
		fmt.Fprintf(os.Stderr, "run %d\n", index+1)
	}
	fmt.Fprintf(os.Stderr, "  %-12s %9s %14s %13s %13s\n", "stage", "wall", "allocated", "objects", "live heap")
	var wall time.Duration
	var allocatedBytes uint64
	for _, s := range result.stages {
		fmt.Fprintf(os.Stderr, "  %-12s %8.3fs %11.1f MiB %13d %9.1f MiB\n",
			s.Name, s.Wall.Seconds(), mib(s.Allocated), s.Objects, mib(s.LiveHeap))
		wall += s.Wall
		allocatedBytes += s.Allocated
	}
	fmt.Fprintf(os.Stderr, "  %-12s %8.3fs %11.1f MiB\n", "total", wall.Seconds(), mib(allocatedBytes))
	fmt.Fprintf(os.Stderr, "  pre-opt:  %d funcs (%d with body), %d blocks, %d instrs, %d data\n",
		result.preOpt.Funcs, result.preOpt.WithBody, result.preOpt.Blocks, result.preOpt.Instrs, result.preOpt.Data)
	fmt.Fprintf(os.Stderr, "  post-opt: %d funcs (%d with body), %d blocks, %d instrs, %d data\n",
		result.postOpt.Funcs, result.postOpt.WithBody, result.postOpt.Blocks, result.postOpt.Instrs, result.postOpt.Data)
	fmt.Fprintf(os.Stderr, "  object:   sha256 %s\n", result.objectSHA[:16])
}

// reportReproducibility answers the question -repeat exists for: is the
// allocation total stable enough to be a committed baseline, or does it wobble?
func reportReproducibility(runs []*run) {
	fmt.Fprintf(os.Stderr, "\n  reproducibility over %d runs\n", len(runs))
	stable := true
	for index := range runs[0].stages {
		name := runs[0].stages[index].Name
		low := runs[0].stages[index].Allocated
		high := low
		for _, r := range runs[1:] {
			value := r.stages[index].Allocated
			if value < low {
				low = value
			}
			if value > high {
				high = value
			}
		}
		spread := float64(high-low) / float64(low) * 100
		if high != low {
			stable = false
		}
		fmt.Fprintf(os.Stderr, "  %-12s %11.1f MiB .. %.1f MiB  spread %.3f%%\n", name, mib(low), mib(high), spread)
	}
	for _, r := range runs[1:] {
		if r.objectSHA != runs[0].objectSHA {
			fmt.Fprintf(os.Stderr, "  OBJECT DIFFERS between runs -- the compile is not deterministic\n")
			return
		}
	}
	fmt.Fprintf(os.Stderr, "  object sha256 identical across all runs\n")
	if stable {
		fmt.Fprintf(os.Stderr, "  allocation byte-exact across all runs\n")
	}
}

// reportInstrCensus counts how often each of ir.Instr's optional fields is
// actually set.
//
// It exists to price moving a field out of the struct and behind InstrExtra.
// That trade is not free: the field stops costing its width on every
// instruction, and starts costing an InstrExtra -- 96 bytes -- on every
// instruction that needs one and did not already have one. So a field is worth
// moving roughly when it is set on fewer than (its width / 96) of instructions,
// and the "already has Extra" column is what says how much of that is already
// paid for.
func reportInstrCensus(module *ir.Module) {
	var total, withExtra int
	counts := map[string]int{}
	alone := map[string]int{}
	for _, function := range module.Funcs {
		for _, block := range function.Blocks {
			for index := range block.Instrs {
				in := &block.Instrs[index]
				total++
				extra := in.Extra != nil
				if extra {
					withExtra++
				}
				note := func(name string, set bool) {
					if !set {
						return
					}
					counts[name]++
					if !extra {
						alone[name]++
					}
				}
				note("Blk", in.Blk != nil)
				note("Asm", in.Asm != nil)
				note("Intrin", in.Intrin != nil)
				note("RetAgg", in.RetAgg != nil)
				note("Inl", in.Inl != nil)
				note("ClosureContext", !in.ClosureContext.IsNone())
				note("Aux", in.Aux != 0)
				note("Amode", in.Amode != 0)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "\n  instruction census: %d instructions, %d (%.1f%%) already carry an InstrExtra\n",
		total, withExtra, percent(withExtra, total))
	fmt.Fprintf(os.Stderr, "  %-16s %10s %8s %14s %8s\n", "field", "set", "of all", "set w/o extra", "of all")
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return counts[names[i]] > counts[names[j]] })
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "  %-16s %10d %7.2f%% %14d %7.2f%%\n",
			name, counts[name], percent(counts[name], total), alone[name], percent(alone[name], total))
	}
}

func percent(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

func reportPasses() {
	if !opt.PassTimingEnabled() {
		fmt.Fprintf(os.Stderr, "\n  (set GOC_OPT_PASSTIME=1 for a per-pass breakdown)\n")
		return
	}
	fixpoints := opt.FixpointTiming()
	if len(fixpoints) > 0 {
		fmt.Fprintf(os.Stderr, "\n  %-18s %8s %8s\n", "fixpoint", "enters", "rounds")
		for _, f := range fixpoints {
			fmt.Fprintf(os.Stderr, "  %-18s %8d %8d\n", f.Name, f.Enters, f.Rounds)
		}
	}
	passes := opt.PassTiming()
	if len(passes) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n  %-14s %4s %7s %9s %9s %9s %9s %11s\n",
		"pass", "id", "runs", "visits", "skipped", "changed", "wall", "allocated")
	var totalNanos int64
	for _, p := range passes {
		totalNanos += p.Nanos
	}
	for _, p := range passes {
		share := float64(p.Nanos) / float64(totalNanos) * 100
		fmt.Fprintf(os.Stderr, "  %-14s %4d %7d %9d %9d %9d %7.3fs %7.1f MiB  %5.1f%%\n",
			p.Name, p.ID, p.Invocations, p.Visits, p.Skipped, p.Changed,
			time.Duration(p.Nanos).Seconds(), mib(p.Bytes), share)
	}
}

// writeCSV puts the machine-readable form on stdout, leaving stderr for the
// tables, so `ccstat ... > runs.csv` keeps both.
func writeCSV(runs []*run) {
	fmt.Println("run,stage,seconds,bytes_allocated,objects_allocated,live_heap_bytes")
	for index, r := range runs {
		for _, s := range r.stages {
			fmt.Printf("%d,%s,%.6f,%d,%d,%d\n",
				index+1, s.Name, s.Wall.Seconds(), s.Allocated, s.Objects, s.LiveHeap)
		}
	}
}

func peakRSS() uint64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	// Linux reports Maxrss in kilobytes.
	return uint64(usage.Maxrss) * 1024
}

func mib(bytes uint64) float64 {
	return float64(bytes) / (1 << 20)
}

// stringList collects a repeatable string flag, the way -I and -D behave for a
// real compiler driver.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimSpace(`
usage: ccstat [flags] file.c

Attributes a cg12 C compile across frontend, optimiser, backend and ELF encode.
The table goes to stderr and CSV to stdout.

flags:
`)+"\n")
	flag.PrintDefaults()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ccstat: "+format+"\n", args...)
	os.Exit(1)
}
