package arm64

import (
	"fmt"
	"os"
	"runtime"
	"strconv"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// The back end is per-function work: lowering, register allocation and emission
// read one function and read-only module facts, and produce a self-contained
// result whose addresses are all relative to the function's own start. That makes
// the functions of a module independent, and on a large module they are most of
// the compile -- compiling goc/testdata/stdlib_http_tls_client_server.go was one
// process at 115% of one core for over two minutes.
//
// Determinism is the constraint. Nothing here may depend on which worker finishes
// first: results are merged strictly in function order, and no name, number or
// address comes from anything shared between workers. The worker count is
// therefore only a throughput knob -- the object is byte-identical at any setting,
// which TestParallelBackendIsByteIdenticalToSerial checks directly.

// functionCode is one function's finished machine code together with the
// per-function facts the object driver merges into the module. It deliberately
// holds no IR: once a function is compiled its IR is dead, and on a large
// standard-library module that IR is most of the compiler's live heap.
type functionCode struct {
	name       string
	export     bool
	code       []byte
	relocs     []obj.Reloc
	blockSyms  []blockSym
	hasLoop    bool
	rows       []obj.LineRow
	dwarf      obj.DwarfFunc
	goFunction goFunctionInfo
	stackMap   *stackMapFunc
	stack      stackFacts
}

// compileFunction lowers, allocates and emits one function. Every offset it
// records is relative to the function's own start, so the caller can place the
// function anywhere in the module's text.
func compileFunction(f *ir.Func, opts Options, conventions calleeConventions, bundle assemblyBundle, goRuntime bool) (*functionCode, error) {
	applyAssemblyCallConventions(f, bundle.callConventions)
	name := sanitize(f.Name)
	params := dwarfParams(f)
	paramTemps := paramTempIDs(f)
	ir.LowerPointers(f, ptrCls)
	if err := lower(f, conventions, opts.TLSModel); err != nil {
		return nil, fmt.Errorf("function %s: %w", f.Name, err)
	}
	alloc, err := regAlloc(f)
	if err != nil {
		return nil, fmt.Errorf("function %s: %w", f.Name, err)
	}
	mc, err := emitMachine(f, alloc, conventions, opts.GC, opts.TLSModel)
	if err != nil {
		return nil, fmt.Errorf("function %s: %w", f.Name, err)
	}
	goFunction, err := goFunctionInfoFor(f, name, mc)
	if err != nil {
		return nil, fmt.Errorf("function %s: %w", f.Name, err)
	}
	// Attach each parameter's DWARF location, computed from its binding.
	for i := range params {
		params[i].Loc = mc.m.varLoc(paramTemps[i])
	}
	dwarfFunction := obj.DwarfFunc{
		Name: name, Sym: name, Size: uint64(len(mc.code)),
		HasRet: f.HasRet, RetType: dwarfRetType(f), Params: params, External: f.Linkage.Export,
		Inlines: buildInlineTree(mc.inl, uint64(len(mc.code))),
	}
	if len(mc.rows) > 0 {
		dwarfFunction.DeclFile, dwarfFunction.DeclLine = mc.rows[0].File, mc.rows[0].Line
	}
	code := &functionCode{
		name:       name,
		export:     f.Linkage.Export,
		code:       mc.code,
		relocs:     mc.relocs,
		blockSyms:  mc.blockSyms,
		hasLoop:    mc.hasLoop,
		rows:       mc.rows,
		dwarf:      dwarfFunction,
		goFunction: goFunction,
		// The nosplit frame budget's facts are gathered here, in the worker,
		// because a large module drops each function's IR as soon as it is merged
		// (see releaseFunctionIR) and the call edges would be gone by the time the
		// module-wide walk runs.
		stack: stackFactsFor(f, name, mc.m.frameLayout, mc.m.frameless),
	}
	// A Go-runtime module carries the runtime's own stack-map metadata in
	// moduledata/pclntab, so the legacy cg12 section is not emitted for it and its
	// safepoints need not be kept alive until the merge.
	if !goRuntime && safepointsHaveRoots(mc.safepoints) {
		code.stackMap = &stackMapFunc{sym: name, points: mc.safepoints}
	}
	return code, nil
}

// compileFunctionsInOrder compiles every function concurrently and calls merge on
// the results in function order, one at a time. The first error in function order
// is returned -- the same error a serial compile would report -- after the
// compiles already in flight have been drained.
func compileFunctionsInOrder(
	functions []*ir.Func,
	opts Options,
	conventions calleeConventions,
	bundle assemblyBundle,
	goRuntime bool,
	merge func(index int, code *functionCode),
) error {
	type compileJob struct {
		done chan struct{}
		code *functionCode
		err  error
	}

	workers := backendWorkers()
	// Look-ahead bounds how many finished results wait for the merge, so the peak
	// follows the worker count rather than the module's function count. Merging is
	// a few appends, so the workers are never short of somewhere to put a result.
	lookahead := 2 * workers
	jobs := make([]*compileJob, len(functions))
	slots := make(chan struct{}, workers)
	started := 0

	start := func(index int) {
		job := &compileJob{done: make(chan struct{})}
		jobs[index] = job
		function := functions[index]
		go func() {
			slots <- struct{}{}
			defer func() {
				<-slots
				close(job.done)
			}()
			job.code, job.err = compileFunction(function, opts, conventions, bundle, goRuntime)
		}()
	}

	var firstError error
	for index := range functions {
		if firstError == nil {
			for started < len(functions) && started < index+lookahead {
				start(started)
				started++
			}
		}
		if index >= started {
			// A failure stopped the dispatch; everything in flight has been drained.
			break
		}
		job := jobs[index]
		jobs[index] = nil
		<-job.done
		if firstError != nil {
			continue
		}
		if job.err != nil {
			firstError = job.err
			continue
		}
		merge(index, job.code)
	}
	return firstError
}

// backendWorkers is how many functions are lowered, allocated and emitted at once.
// The default is GOMAXPROCS: a compile of a large module is otherwise a
// single-threaded phase, and the capability matrix's wall clock is bounded by one
// such compile. GOC_BACKEND_WORKERS overrides it. The compiler's output does not
// depend on the value, so it is a throughput knob and nothing else -- useful for
// measurement, and for a caller that is already running many compiles at once.
func backendWorkers() int {
	if setting := os.Getenv("GOC_BACKEND_WORKERS"); setting != "" {
		if workers, err := strconv.Atoi(setting); err == nil && workers > 0 {
			return workers
		}
	}
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		return 1
	}
	return workers
}
