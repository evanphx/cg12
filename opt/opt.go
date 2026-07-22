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
func OptimizeModule(m *ir.Module) {
	Run(m, DefaultPipeline())
}
