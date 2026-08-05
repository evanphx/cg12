package opt

import (
	"os"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// Pass is one step of the optimization pipeline over a module. Every pass
// reports whether it changed anything, so pipelines (and [Fixpoint]) can iterate
// until the program stops changing.
type Pass interface {
	Name() string
	Run(m *ir.Module) bool
}

// FuncPass lifts a per-function transform to a module pass by applying it to
// every function with a body. Almost every optimization is intraprocedural and
// enters the pipeline this way; only the interprocedural passes ([Inline],
// [DeadFuncElim]) need to see the whole module and are [ModulePass]es.
func FuncPass(name string, run func(*ir.Func) bool) Pass {
	return funcPass{name: name, run: run}
}

type funcPass struct {
	name string
	run  func(*ir.Func) bool
}

func (p funcPass) Name() string { return p.name }

func (p funcPass) Run(m *ir.Module) bool {
	changed := false
	for _, f := range m.Funcs {
		if f.Start == nil {
			continue // a declaration with no body
		}
		// CFG analyses currently model a single entry. Leave metadata-entered
		// functions intact until those analyses grow an explicit virtual root.
		if hasSecondaryEntry(f) {
			continue
		}
		if p.run(f) {
			changed = true
		}
	}
	return changed
}

// ModulePass wraps an interprocedural transform that acts on the whole module.
func ModulePass(name string, run func(*ir.Module) bool) Pass {
	return modulePass{name: name, run: run}
}

type modulePass struct {
	name string
	run  func(*ir.Module) bool
}

func (p modulePass) Name() string          { return p.name }
func (p modulePass) Run(m *ir.Module) bool { return p.run(m) }

// Fixpoint runs its member passes in order, repeating the whole sequence until a
// full round changes nothing. Because a Fixpoint is itself a Pass, fixpoints
// nest: the default pipeline wraps "inline then clean" in a fixpoint so that the
// simplification a round of inlining exposes can, in turn, expose the next
// inlining candidate.
func Fixpoint(name string, passes ...Pass) Pass {
	return fixpoint{name: name, passes: passes}
}

type fixpoint struct {
	name   string
	passes []Pass
}

func (fp fixpoint) Name() string { return fp.name }

func (fp fixpoint) Run(m *ir.Module) bool {
	any := false
	for {
		round := false
		for _, p := range fp.passes {
			if p.Run(m) {
				round = true
			}
		}
		if !round {
			return any
		}
		any = true
	}
}

// Run applies a pipeline to a module in order.
func Run(m *ir.Module, pipeline []Pass) {
	for _, p := range pipeline {
		p.Run(m)
	}
}

// DefaultPipeline is the standard optimization pipeline. Its shape encodes the
// interaction between the interprocedural inliner and the intraprocedural
// simplifications: functions are cleaned up before inlining (so cost estimates
// are honest), inlining and cleanup iterate together to a fixpoint (so the
// opportunities inlining exposes are taken, and taking them exposes more), dead
// functions are dropped, and global code motion schedules once at the end.
func DefaultPipeline() []Pass {
	clean := Fixpoint("clean",
		FuncPass("fold", Fold),
		FuncPass("copy", Copy),
		FuncPass("loadelim", LoadElim),
		FuncPass("deadalloc", DeadAlloc),
		FuncPass("gvn", GVN),
		// Threading turns a phi-then-rebranch into a direct edge; fold/gvn above
		// canonicalize the condition so it fires, and simplifycfg below absorbs the
		// clone into its predecessor and folds the branches threading exposes.
		JumpThreadPass(),
		FuncPass("simplifycfg", SimplifyCFG),
		FuncPass("dce", DCE),
	)
	// One inline pass instance, shared by both inline stages, so a function's growth
	// cap is measured against its original size across the whole pipeline (not reset
	// to the already-grown size at the second stage, which would let growth compound).
	inline := InlinePass()
	return []Pass{
		FuncPass("mem2reg", Mem2Reg),
		clean,
		Fixpoint("inline-fixpoint",
			inline,
			clean,
		),
		// Inlining a helper that took the address of a caller's local (e.g. an
		// interpreter's pop(&sp)/push(&sp)) dissolves the address-taking, leaving a
		// scalar that is now only loaded and stored -- promotable, but the initial
		// mem2reg ran before inlining and could not see it. Promote again, then clean
		// up the phis and redundant loads it exposes.
		FuncPass("mem2reg", Mem2Reg),
		clean,
		ModulePass("unroll", UnrollRecursion), // bounded in-place recursion unrolling
		Fixpoint("inline-fixpoint", // inline/simplify what unrolling exposed
			inline,
			clean,
		),
		// Inlining has now propagated constants into the __builtin_constant_p markers;
		// resolve them and let clean fold the fast paths they gate (Ruby's RB_TYPE_P).
		FuncPass("constantp", ResolveConstantP),
		// With inlining and simplification settled, convert short branch diamonds
		// exposed anywhere in the pipeline into selects, then clean up the drained
		// arms and fold any select whose condition simplification made constant.
		FuncPass("ifconvert", IfConvert),
		clean,
		// Share each function's return epilogue: fold its duplicate return blocks
		// into one, then drop the blocks that leaves unreachable.
		FuncPass("tailmerge", TailMerge),
		FuncPass("simplifycfg", SimplifyCFG),
		FuncPass("dce", DCE),
		ModulePass("deadfunc", DeadFuncElim),
		FuncPass("gcm", GCM),
		FuncPass("dce", DCE),
		// Last, because it measures. Inlining into a function that emits no
		// stack-growth check is bounded by bytes of stack rather than by the size
		// proxy the rest of the pipeline uses, and the only way to know the bytes
		// is to lay the frame out -- which means the code being measured has to be
		// the code the backend will see. Nothing runs after this but the backend.
		ModulePass("inline-nosplit", InlineIntoNoSplitCallers),
	}
}

// BoundedPipeline keeps only the local linear-time cleanup passes that do not
// build CFGs or clone code: fold, copy and DCE. It is no longer what a build
// gets by default; it is the bisection arm, selected by GOC_OPT_PIPELINE=bounded
// (see [ModulePipeline]).
//
// It was the default for every whole-program Go build for as long as the module
// budget existed. The program module carries the stdlib closure the prebuilt
// runtime pack did not already hold, which even for a 168-line program is 5101
// functions, 70160 blocks and 297389 instructions against caps of 2048, 50000
// and 200000 -- so this three-pass pipeline was what `goc -O` meant in practice,
// and no goc module had ever run [DefaultPipeline] (RUNTIME_PLAN.md:4282 records
// the same fact from the other side).
//
// That is where the performance suite's 1.63x floor came from. Without mem2reg
// every local stays in its frame slot and is reloaded and stored back on each
// use, so a loop-carried dependence runs through store-to-load forwarding instead
// of a register. The suite's control -- a two-variable integer multiply-add loop
// -- took 22 instructions per iteration here where the host toolchain takes 14,
// and ran at 1.632x in all eleven programs that contain it.
//
// Two miscompiles had to be fixed before promotion could be turned on, both in
// Mem2Reg's markManagedDef: a promoted managed local was carried across
// safepoints by a value nothing described to the collector, so the object was
// freed under it (placement_bench/p256 failed to verify its own signatures 35
// runs in 40 at GOGC=10 and 0 in 40 at the default), and the same unmarked value
// lost the private spill slot a GC root gets, which broke
// stdlib-netpoll-stress/tcp-churn's interface dispatch under GOGC=off too. See
// goc/testdata/runtime_gc_promoted_local_root.go and
// goc/testdata/runtime_opt_promoted_interface_root.go for the reducers.
//
// CCWORK_REPORT.md has the bisections and the measurements.
func BoundedPipeline() []Pass {
	clean := Fixpoint("bounded-clean",
		FuncPass("fold", Fold),
		FuncPass("copy", Copy),
		FuncPass("dce", DCE),
	)
	return []Pass{clean}
}

// PromotePipeline is BoundedPipeline plus mem2reg: the promotion-only step
// between the two, kept as a named arm so a failure can be attributed to
// promotion or to the rest of [DefaultPipeline] without editing the compiler.
// Selected by GOC_OPT_PIPELINE=promote.
func PromotePipeline() []Pass {
	return append([]Pass{FuncPass("mem2reg", Mem2Reg)}, BoundedPipeline()...)
}

// ModulePipeline chooses the pipeline a whole module is optimized with. The
// default is [DefaultPipeline] for every module regardless of size.
//
// GOC_OPT_PIPELINE selects an arm explicitly:
//
//	full     (default) DefaultPipeline
//	promote            BoundedPipeline plus mem2reg
//	bounded            BoundedPipeline, the pre-2026-08 default
//
// GOC_OPT_SKIP is a comma-separated list of pass names dropped from whichever
// pipeline was selected, at any nesting depth. It exists so a miscompile can be
// attributed to one pass by an outside job -- the corpus and the capability
// matrix compile in-process, so there is no compiler flag to thread through --
// and it is the first thing to reach for when a program that passes under
// GOC_OPT_PIPELINE=bounded fails under the default.
//
// A fixpoint's own name is skippable too, and drops everything inside it: "clean"
// removes the whole cleanup set wherever it appears. The two inliner fixpoints
// are called "inline-fixpoint" rather than "inline" precisely so that skipping
// "inline" means the inliner and not also the two rounds of cleanup it is
// bracketed with -- a bisection that removes more than it names is a bisection
// that attributes to the wrong pass.
//
// An unrecognized GOC_OPT_PIPELINE value panics rather than silently selecting
// the default: a typo in a bisection variable that quietly measures the default
// arm is worse than a crash.
func ModulePipeline() []Pass {
	var pipeline []Pass
	switch selected := os.Getenv("GOC_OPT_PIPELINE"); selected {
	case "", "full", "default":
		pipeline = DefaultPipeline()
	case "promote":
		pipeline = PromotePipeline()
	case "bounded":
		pipeline = BoundedPipeline()
	default:
		panic("opt: GOC_OPT_PIPELINE=" + selected + " is not one of full, promote, bounded")
	}
	return withoutPasses(pipeline, skippedPasses())
}

// PipelineIdentity names the pipeline [ModulePipeline] would select, in a form
// stable enough to key a build cache on.
//
// It exists because the prebuilt runtime pack is cached by content
// (cmd/goc/packcache.go), and the key covers the compiler binary's bytes rather
// than the environment it runs in. GOC_OPT_PIPELINE and GOC_OPT_SKIP change what
// the pack contains without changing a byte of the compiler, so without this a
// bisection run under GOC_OPT_PIPELINE=bounded would silently link the
// full-pipeline pack the previous run cached -- a stale hit that is not a slow
// build but a wrong one, and one that would have quietly invalidated every
// bisection this switch exists to make possible.
func PipelineIdentity() string {
	identity := os.Getenv("GOC_OPT_PIPELINE")
	if identity == "" {
		identity = "full"
	}
	if skip := os.Getenv("GOC_OPT_SKIP"); skip != "" {
		identity += "-skip:" + skip
	}
	return identity
}

// skippedPasses parses GOC_OPT_SKIP.
func skippedPasses() map[string]bool {
	value := os.Getenv("GOC_OPT_SKIP")
	if value == "" {
		return nil
	}
	skipped := make(map[string]bool)
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			skipped[name] = true
		}
	}
	return skipped
}

// withoutPasses drops the named passes from a pipeline, descending into
// fixpoints so that skipping "gvn" removes it from the clean set too.
func withoutPasses(pipeline []Pass, skipped map[string]bool) []Pass {
	if len(skipped) == 0 {
		return pipeline
	}
	kept := make([]Pass, 0, len(pipeline))
	for _, pass := range pipeline {
		if skipped[pass.Name()] {
			continue
		}
		if nested, ok := pass.(fixpoint); ok {
			nested.passes = withoutPasses(nested.passes, skipped)
			if len(nested.passes) == 0 {
				continue
			}
			kept = append(kept, nested)
			continue
		}
		kept = append(kept, pass)
	}
	return kept
}
