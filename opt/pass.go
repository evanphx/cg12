package opt

import (
	"os"
	"strings"
	"sync/atomic"

	"github.com/evanphx/cg12/ir"
)

// Pass is one step of the optimization pipeline over a module. Every pass
// reports whether it changed anything, so pipelines (and [Fixpoint]) can iterate
// until the program stops changing.
type Pass interface {
	Name() string
	Run(m *ir.Module) bool
}

// changeLog is the pipeline's record of which functions have moved since which
// pass last looked at them, and it is why a whole-module compile does not
// re-converge over the whole module on every fixpoint round.
//
// The pipeline's shape means most of a round's work is already known to be
// wasted. `clean` is a seven-pass fixpoint; the two `inline-fixpoint` blocks
// call it again after every inlining round; and each of those calls re-ran all
// seven passes over all 5101 functions of a whole-program Go module. Measured on
// goc/testdata/fmt_sprintf.go: 50 rounds of `clean` across 13 calls, 252,550
// visits per pass, of which between 0.17% (deadalloc) and 3.8% (simplifycfg)
// changed anything. Better than 96% of every pass's work was re-proving a
// fixpoint it had already reached.
//
// The rule that makes skipping sound: every pass entered through [FuncPass] is a
// deterministic function of the one function it is handed -- the transform's
// signature is func(*ir.Func) bool and nothing in the default pipeline reaches
// past it. So if a pass ran on f and reported no change, and no pass has changed
// f since, running it again would report no change again. Skipping a visit that
// provably cannot change anything cannot change the compiler's output, and the
// determinism and byte-identity checks say it does not.
//
// Passes that cannot say what they touched (any [ModulePass] but the inliner)
// are handled the other way: if one reports a change, every function is marked
// moved, and the next pass over them starts from scratch.
type changeLog struct {
	// version counts the changes made to a function. Any increment invalidates
	// every pass's convergence on it.
	version map[*ir.Func]uint32
	// converged records, per (pass, function), the version at which that pass
	// last ran and reported no change.
	converged map[passFunc]uint32
}

type passFunc struct {
	pass int32
	fn   *ir.Func
}

func newChangeLog() *changeLog {
	return &changeLog{
		version:   map[*ir.Func]uint32{},
		converged: map[passFunc]uint32{},
	}
}

// settled reports whether pass has already converged on f at f's current
// version, so that running it again cannot change anything.
func (log *changeLog) settled(pass int32, f *ir.Func) bool {
	if log == nil {
		return false
	}
	at, ok := log.converged[passFunc{pass: pass, fn: f}]
	return ok && at == log.version[f]
}

// record files the outcome of running pass on f.
func (log *changeLog) record(pass int32, f *ir.Func, changed bool) {
	if log == nil {
		return
	}
	if changed {
		log.version[f]++
		return
	}
	log.converged[passFunc{pass: pass, fn: f}] = log.version[f]
}

// moveEverything marks every function as changed. It is what a pass that cannot
// report which functions it touched costs.
func (log *changeLog) moveEverything(m *ir.Module) {
	if log == nil {
		return
	}
	for _, f := range m.Funcs {
		log.version[f]++
	}
}

// tracked is the optional half of [Pass] for the passes that can take part in
// change tracking: the per-function ones, the fixpoints built from them, and the
// inliner, which knows which callers it spliced into. A pass that does not
// implement it still runs, and is assumed to have changed everything.
type tracked interface {
	runTracked(m *ir.Module, log *changeLog) bool
}

// runPass applies one pipeline step, using change tracking where the pass
// supports it.
func runPass(p Pass, m *ir.Module, log *changeLog) bool {
	if t, ok := p.(tracked); ok {
		return t.runTracked(m, log)
	}
	if !p.Run(m) {
		return false
	}
	log.moveEverything(m)
	return true
}

// passIDs hands each constructed pass an identity, so that two positions in a
// pipeline that happen to share a name (the default pipeline has three "dce"s)
// keep their convergence records apart.
var passIDs atomic.Int32

// FuncPass lifts a per-function transform to a module pass by applying it to
// every function with a body. Almost every optimization is intraprocedural and
// enters the pipeline this way; only the interprocedural passes ([Inline],
// [DeadFuncElim]) need to see the whole module and are [ModulePass]es.
func FuncPass(name string, run func(*ir.Func) bool) Pass {
	return funcPass{id: passIDs.Add(1), name: name, run: run}
}

type funcPass struct {
	id   int32
	name string
	run  func(*ir.Func) bool
}

func (p funcPass) Name() string { return p.name }

// Run applies the transform to every function, without change tracking. It is
// the entry point for callers outside a pipeline.
func (p funcPass) Run(m *ir.Module) bool { return p.runTracked(m, nil) }

func (p funcPass) runTracked(m *ir.Module, log *changeLog) bool {
	changed := false
	for _, f := range m.Funcs {
		if f.Start == nil {
			continue // a declaration with no body
		}
		if log.settled(p.id, f) {
			continue // converged on exactly these bytes already
		}
		// CFG analyses currently model a single entry. Leave metadata-entered
		// functions intact until those analyses grow an explicit virtual root.
		// Declining to transform one is a settled outcome like any other, so the
		// scan for it is not repeated until something changes the function.
		if hasSecondaryEntry(f) {
			log.record(p.id, f, false)
			continue
		}
		moved := p.run(f)
		log.record(p.id, f, moved)
		if moved {
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

// Run iterates to a fixpoint without change tracking, for callers outside a
// pipeline.
func (fp fixpoint) Run(m *ir.Module) bool { return fp.runTracked(m, nil) }

func (fp fixpoint) runTracked(m *ir.Module, log *changeLog) bool {
	any := false
	for {
		round := false
		for _, p := range fp.passes {
			if runPass(p, m, log) {
				round = true
			}
		}
		if !round {
			return any
		}
		any = true
	}
}

// Run applies a pipeline to a module in order, tracking which functions each
// pass has already converged on across the whole pipeline -- so the `clean`
// fixpoint the two inliner stages re-enter starts from the callers the inliner
// actually spliced into, not from all 5101 functions of the module.
func Run(m *ir.Module, pipeline []Pass) {
	log := newChangeLog()
	for _, p := range pipeline {
		runPass(p, m, log)
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
