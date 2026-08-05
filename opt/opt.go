package opt

import "github.com/evanphx/cg12/ir"

// Optimize runs the available passes to a fixpoint: constant folding, copy
// elimination, CFG simplification, and dead-code elimination. It mutates f in
// place and leaves it in valid SSA form for target lowering.
func Optimize(f *ir.Func) {
	// The dataflow passes currently model a single CFG entry. A secondary entry
	// needs a virtual root before those transforms can safely move frame values.
	if hasSecondaryEntry(f) {
		return
	}
	Mem2Reg(f) // promote memory to SSA once, up front
	for {
		folded := Fold(f)
		copied := Copy(f)
		loads := LoadElim(f)
		numbered := GVN(f)
		simplified := SimplifyCFG(f)
		dead := DCE(f)
		if !folded && !copied && !loads && !numbered && !simplified && !dead {
			break
		}
	}
	// Global code motion is a scheduling pass: run it once after the algebraic
	// fixpoint, then clean up any values it stranded.
	GCM(f)
	DCE(f)
}

func hasSecondaryEntry(function *ir.Func) bool {
	for _, block := range function.Blocks {
		if block.SecondaryEntry {
			return true
		}
	}
	return false
}

// OptimizeModule runs the default whole-module pipeline: the intraprocedural
// passes of [Optimize] plus the interprocedural inliner and dead-function
// elimination, sequenced by [DefaultPipeline]. Unlike [Optimize] on a single
// function, it can inline across functions.
//
// It used to route any module past 2048 functions, 50000 blocks or 200000
// instructions to [BoundedPipeline] instead, which meant every whole-program Go
// build -- all of which exceed all three by an order of magnitude, because the
// program module carries the stdlib closure -- was optimized by fold, copy and
// DCE alone. The budget is gone: size no longer decides the pipeline, and
// [ModulePipeline] does, from GOC_OPT_PIPELINE.
//
// What the budget was protecting against was peak memory on the five largest C
// programs in the tree (48200ab), measured against a 3 GiB ceiling. Those
// programs are re-measured on this tree in CCWORK_REPORT.md; the full pipeline
// on the Go builds costs compile time, not a memory cliff.
func OptimizeModule(m *ir.Module) {
	Run(m, ModulePipeline())
}
