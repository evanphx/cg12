package goc_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// A pointer local that `-O` promotes out of the frame must still be a GC root.
//
// Promotion is what makes this a question at all. Unpromoted, the local keeps
// its frame slot, the slot is in Func.StackPointerWords, and the collector reads
// the pointer out of the frame at every safepoint. opt.Mem2Reg moves the value
// into SSA, where the backend reports it only if the temporary holding it is
// marked GCRef -- and at a join it must mint a temporary (a phi) rather than
// reuse the stored value's own. A phi that is not marked is a pointer live
// across a loop back edge that is a root nowhere: the collector frees the object
// the loop is still carrying, and the next iteration follows a dangling pointer.
//
// That was the state of the `-O` capability arm: `stack-scan/loop-safepoints`
// failed with "a stack slot live across a loop back edge was not a GC root" for
// as long as the arm had been measured, while the default arm was clean.
//
// The program is run under GODEBUG=clobberfree=1 so a premature collection is a
// fault at 0xdeadbeefdeadbeef rather than a silent read of whatever was
// reallocated over the object, and at GOMAXPROCS=1 so the collection happens at
// the runtime.GC() call rather than on some other thread's schedule.
func TestOptimizedLoopCarriedPointerStaysAGCRoot(t *testing.T) {
	output := runCorpusProgramOutputOptimizedBy(t,
		filepath.Join("testdata", "runtime_opt_loop_carried_root.go"),
		optimizeProgramFunctions,
		"GODEBUG=clobberfree=1")
	require.Contains(t, output, "opt loop carried root ok")
}

// The other half of the same question: a pointer local that `-O` promotes must
// also survive the stack moving under it.
//
// The phi case above is the one a collection exposes. This is the one stack
// growth exposes, and it needs no phi at all. A local of interface type is a
// frame slot holding the address of the two-word descriptor, and for a
// multi-result call that descriptor is the result home the call writes into --
// an allocation in the caller's own frame. Promotion makes the call's result
// temporary the value every later read sees, so a pointer the safepoint map
// described while it sat in a frame slot now lives in an SSA value the map does
// not mention. copystack walks the frame map, does not find it, and leaves the
// local pointing into the freed stack.
//
// That is what stdlib-netpoll-stress/tcp-churn died of on the `-O` arm with
// GOC_BOUNDED_MEM2REG=1: `cg12: interface dispatch failed for dynamic type 0x0`
// in net.Listener.Accept, on a net.Listener whose descriptor main.main had read
// out of a stack the runtime had already recycled. The reducer makes the
// recycling explicit -- the fault address is the pattern its own goroutines
// wrote -- so it fails the same way on every run rather than depending on what
// happened to be left behind.
func TestOptimizedInterfaceLocalSurvivesStackGrowth(t *testing.T) {
	output := runCorpusProgramOutputOptimizedBy(t,
		filepath.Join("testdata", "runtime_opt_promoted_interface_root.go"),
		optimizeProgramFunctions)
	require.Contains(t, output, "opt promoted interface root ok")
}

// optimizeProgramFunctions runs the intraprocedural pipeline over the program's
// own functions, which is what goc's `-O` does to them and what a monolithic
// opt.OptimizeModule on this module does not.
//
// A goc executable module carries the whole Go runtime, which puts it over
// opt.OptimizeModule's function budget, so that call degrades to
// BoundedPipeline -- fold, copy, dce -- and never promotes a slot. The matrix's
// `-O` arm does not have that problem: it compiles against a prebuilt runtime,
// so the program is a module of its own, small enough for DefaultPipeline, and
// mem2reg runs. Restricting the pipeline to the program's functions reproduces
// that here without a prebuilt pack, and leaves the runtime half of the module
// exactly as the unoptimized corpus runs compile it.
func optimizeProgramFunctions(module *ir.Module) {
	for _, function := range module.Funcs {
		if function == nil || function.Start == nil {
			continue
		}
		if !strings.HasPrefix(function.Name, "main.") {
			continue
		}
		opt.Optimize(function)
	}
}
