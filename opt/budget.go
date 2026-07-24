package opt

import "github.com/evanphx/cg12/ir"

const (
	cfgOptimizationInstructionBudget = 1000
	cfgOptimizationBlockBudget       = 200
	cfgOptimizationTempBudget        = 4000
)

func cfgOptimizationOverBudget(function *ir.Func) bool {
	if len(function.Blocks) > cfgOptimizationBlockBudget {
		return true
	}
	if len(function.Temps) > cfgOptimizationTempBudget {
		return true
	}
	instructions := 0
	for _, block := range function.Blocks {
		instructions += len(block.Phis) + len(block.Instrs)
		if instructions > cfgOptimizationInstructionBudget {
			return true
		}
	}
	return false
}
