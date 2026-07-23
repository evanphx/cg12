package opt

import (
	"sort"

	"github.com/evanphx/cg12/ir"
)

// NoSplitViolation records a direct call from a nosplit function to a function
// that still has an ordinary stack-growth check.
type NoSplitViolation struct {
	Caller string
	Callee string
}

// AuditNoSplitCalls reports direct nosplit-to-splittable call edges that remain
// in a module. Such edges are dangerous in runtime code because a nosplit path
// may run on g0, gsignal, or another context where invoking morestack is fatal.
func AuditNoSplitCalls(module *ir.Module) []NoSplitViolation {
	graph := buildCallGraph(module)
	seen := make(map[NoSplitViolation]bool)
	var violations []NoSplitViolation

	for _, caller := range module.Funcs {
		if caller.Start == nil || !caller.NoSplit {
			continue
		}
		for _, block := range caller.Blocks {
			for index := range block.Instrs {
				instruction := &block.Instrs[index]
				if instruction.Op != ir.OCall {
					continue
				}
				callee := directCallee(caller, instruction, graph.byName)
				if callee == nil || callee.NoSplit {
					continue
				}
				violation := NoSplitViolation{
					Caller: caller.Name,
					Callee: callee.Name,
				}
				if !seen[violation] {
					seen[violation] = true
					violations = append(violations, violation)
				}
			}
		}
	}

	sort.Slice(violations, func(left, right int) bool {
		if violations[left].Caller != violations[right].Caller {
			return violations[left].Caller < violations[right].Caller
		}
		return violations[left].Callee < violations[right].Callee
	})
	return violations
}
